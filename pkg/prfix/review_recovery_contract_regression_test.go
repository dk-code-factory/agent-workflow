package prfix

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func recoveryAdapterResponses(provider *fakeProvider) map[string]any {
	return map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}},
		"repos/owner/repo/pulls/1/requested_reviewers": map[string]any{"users": []any{}, "teams": []any{}}, "repos/owner/repo/issues/1/timeline?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
}

func TestCompletedReplyReconciliationUsesHistoricalIDWithNewIdenticalAnswer(t *testing.T) {
	for _, kind := range []string{"reply", "reply_inline"} {
		t.Run(kind, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Authorizations = map[string]Authorization{"iterate": {"reply", "Reply to the feedback.", "chat:reply"}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ReadPlan(prepared.Plan)
			if err != nil {
				t.Fatal(err)
			}
			l, err := OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			i := Intent{ID: "historical", Kind: kind, ActorID: 7, Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Before: map[string]any{"comment_ids": []string{}}, Payload: map[string]any{"body": "Verified answer"}, Sources: []string{p.Snapshot.Evidence}}
			if kind == "reply_inline" {
				i.Payload["comment_id"] = "9"
			}
			if err = l.PrepareIntent(ctx, p, i, p.Snapshot.Evidence); err != nil {
				t.Fatal(err)
			}
			i, err = l.Intent(ctx, p.LogicalRun, i.ID)
			if err != nil {
				t.Fatal(err)
			}
			comment := func(id int, body string, actor int) map[string]any {
				return map[string]any{"id": id, "body": body, "in_reply_to_id": 9, "user": map[string]any{"login": "author", "id": actor}}
			}
			original := comment(101, "Verified answer", 7)
			receipt := Receipt{i.ID, i.Kind, p.Identity.URL, i.Head, original, []string{"verified historical publication"}}
			r := &Runtime{Root: root, Provider: &GitHub{Root: root}}
			if _, err = r.completeEffect(ctx, l, i, receipt); err != nil {
				t.Fatal(err)
			}
			responses := recoveryAdapterResponses(provider)
			update := fakeGH(t, root, responses)
			for _, tc := range []struct {
				name     string
				comments []any
				viewer   int
				want     bool
			}{
				{"historical plus duplicate", []any{original, comment(102, "Verified answer", 7)}, 7, true},
				{"substitute only", []any{comment(102, "Verified answer", 7)}, 7, false},
				{"edited historical", []any{comment(101, "Edited", 7), comment(102, "Verified answer", 7)}, 7, false},
				{"wrong historical actor", []any{comment(101, "Verified answer", 8)}, 7, false},
				{"wrong current actor", []any{original}, 8, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					endpoint := "repos/owner/repo/issues/1/comments?per_page=100"
					if kind == "reply_inline" {
						endpoint = "repos/owner/repo/pulls/1/comments?per_page=100"
					}
					responses[endpoint] = []any{tc.comments}
					responses["user"] = map[string]any{"login": "author", "id": tc.viewer}
					update(responses)
					state, observed, err := r.reconcileCompletedIntent(ctx, l, i)
					if tc.want && (err != nil || state != "completed") || !tc.want && (err == nil || state != "unknown") || githubID(observed.Result, "id") != "101" {
						t.Fatal(state, observed, err)
					}
				})
			}
		})
	}
}

func TestUncertainMetadataRecoveryRetainsOriginalActor(t *testing.T) {
	for _, kind := range []string{"edit_pr", "request_review"} {
		t.Run(kind, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Authorizations = map[string]Authorization{"iterate": {"publish", "Publish this PR.", "chat:publish"}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ReadPlan(prepared.Plan)
			if err != nil {
				t.Fatal(err)
			}
			l, err := OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			i := Intent{ID: "uncertain", Kind: kind, ActorID: 7, Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Payload: map[string]any{"title": provider.ev.Metadata.Title}, Sources: []string{p.Snapshot.Evidence}}
			if kind == "request_review" {
				i.Payload = map[string]any{"reviewers": []string{"reviewer"}, "team_reviewers": []string{}}
			}
			if err = l.PrepareIntent(ctx, p, i, p.Snapshot.Evidence); err != nil {
				t.Fatal(err)
			}
			if _, err = l.db.Exec("UPDATE effects SET state='unknown' WHERE run=? AND id=?", p.LogicalRun, i.ID); err != nil {
				t.Fatal(err)
			}
			responses := recoveryAdapterResponses(provider)
			responses["user"] = map[string]any{"id": 8, "login": "other-account"}
			responses["repos/owner/repo/pulls/1/requested_reviewers"] = map[string]any{"users": []any{map[string]any{"login": "reviewer"}}, "teams": []any{}}
			update := fakeGH(t, root, responses)
			r := &Runtime{Root: root, Provider: &GitHub{Root: root}}
			if _, err = r.publish(ctx, p, ActionInput{Intent: i.ID}); err == nil {
				t.Fatal("another actor settled the uncertain operation")
			}
			var state, path string
			if err = l.db.QueryRow("SELECT state,receipt FROM effects WHERE run=? AND id=?", p.LogicalRun, i.ID).Scan(&state, &path); err != nil || state != "unknown" || path != "" {
				t.Fatal(state, path, err)
			}
			responses["user"] = map[string]any{"id": 7, "login": "author"}
			update(responses)
			if out, err := r.publish(ctx, p, ActionInput{Intent: i.ID}); err != nil || !out.Settled {
				t.Fatal(out, err)
			}
		})
	}
}

func TestReviewRequestFailedResponseDiagnosticsSurviveInspection(t *testing.T) {
	for _, tc := range []struct {
		name, response, phase string
		exit, status          int
	}{
		{"malformed response", "HTTP/2.0 200 OK\r\nSecret-Header: private\r\n\r\n{malformed\r\nbody", "response_parse", 0, 200},
		{"HTTP failure", "HTTP/2.0 503 Unavailable\r\nSecret-Header: private\r\n\r\nprivate exception response", "http", 1, 503},
		{"failed command without HTTP", "private transport response", "command", 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, provider, req := fixture(t)
			id, err := ParseIdentity(req.PR)
			if err != nil {
				t.Fatal(err)
			}
			responses := recoveryAdapterResponses(provider)
			var dispatched any = tc.response
			if tc.phase == "command" {
				dispatched = map[string]any{"__body": tc.response, "__stderr": "authorization: private credential", "__exit": tc.exit}
			}
			responses["repos/owner/repo/pulls/1/requested_reviewers"] = map[string]any{"__sequence": []any{map[string]any{"users": []any{}, "teams": []any{}}, dispatched}}
			update := fakeGH(t, root, responses)
			g := &GitHub{Root: root}
			i := Intent{Run: "logical-run", ID: "failed-request", CreatedAt: "2026-11-10T00:00:00Z", Kind: "request_review", PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"reviewers": []string{"reviewer"}, "team_reviewers": []string{}}}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ctx = context.WithValue(ctx, dispatchContextKey{}, workflows.ActionContext{RunID: "engine-run", AttemptID: "attempt", OperationKey: "operation"})
			if _, err := g.Execute(ctx, i); err == nil || !strings.Contains(err.Error(), "safe diagnostics retained") || strings.Contains(err.Error(), "private") {
				t.Fatal("failed response diagnostics not safely retained", err)
			}
			original, err := os.ReadFile(g.diagnosticPath(i, "historical-failure"))
			if err != nil || strings.Contains(string(original), "private") || strings.Contains(string(original), "Secret-Header") {
				t.Fatal(string(original), err)
			}
			if err := g.recordObservationFailure(i); err != nil {
				t.Fatal(err)
			}
			responses["repos/owner/repo/pulls/1/requested_reviewers"] = map[string]any{"users": []any{}, "teams": []any{}}
			update(responses)
			observation, err := g.InspectReviewRequest(ctx, i)
			failure := observation.HistoricalFailure
			body, _ := responseBody([]byte(tc.response))
			if err != nil || failure == nil || failure.Phase != tc.phase || failure.ExitStatus == nil || *failure.ExitStatus != tc.exit || tc.status > 0 && (failure.HTTPStatus == nil || *failure.HTTPStatus != tc.status) || tc.status == 0 && failure.HTTPStatus != nil || failure.ResponseHash != rawHash(body) || failure.ResponseLength != len(body) {
				t.Fatal(observation, err)
			}
			if observation.Execution == nil || observation.Execution.CreatedAt != i.CreatedAt || observation.Execution.Dispatched == "" || observation.Execution.Action.RunID != "engine-run" || len(observation.ReconciliationErrors) != 1 {
				t.Fatal("dispatch provenance or separate observation failure absent", observation)
			}
			after, err := os.ReadFile(g.diagnosticPath(i, "historical-failure"))
			if err != nil || string(after) != string(original) {
				t.Fatal("first failure was overwritten", err)
			}
		})
	}
}

func TestNoReplacementDispositionSettlesObsoleteRequiredAnswerWithoutWrite(t *testing.T) {
	root, base, req := fixture(t)
	base.discussionWrites = true
	provider := &replyRecoveryProvider{base}
	source := recoveryReviewSource(base)
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-feedback-read"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-feedback-reply", "dk-workflows:dk-pr-publish"}
	req.Authorizations = map[string]Authorization{"iterate": {"reply", "Reply to required feedback.", "chat:reply"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		Path        string           `json:"path"`
		Observation ReplyObservation `json:"observation"`
		Fingerprint string           `json:"fingerprint"`
	}
	session := &fakeSession{assignmentHook: func(b []byte) {
		var assignment map[string]json.RawMessage
		if err := json.Unmarshal(b, &assignment); err != nil {
			t.Error(err)
			return
		}
		if raw := assignment["reply_inspection"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &inspected); err != nil {
				t.Error(err)
			}
		}
	}, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "inspection" {
			report.ReplyObligations = []ReplyObligation{{ID: "feedback", Text: "Please explain the old code.", Required: true, Reason: "Concrete author question.", Sources: []string{s.Evidence}}}
		}
		if o.ID == "reply_recovery" {
			report.ReplyDisposition = &ReplyDisposition{Decision: "no_replacement", Reason: "Current code removed the questioned implementation; reassessed code and discussion need no further answer.", Inspection: inspected.Path, Observation: inspected.Fingerprint, Sources: append(append([]string{}, inspected.Observation.Intent.Sources...), inspected.Path)}
		}
	}, reportReplies: func(o Occurrence, s Snapshot, obligations []retainedObligation, report *Report) {
		if o.ID == "reply_recovery" || o.Kind != "reply" {
			return
		}
		for _, item := range obligations {
			report.Responses = append(report.Responses, ReplyResponse{ID: item.Obligation.ID, Outcome: "reply", Reason: "Verified answer.", Sources: []string{item.Original}, Operation: "historical"})
		}
		report.Operations = []Intent{{ID: "historical", Kind: "reply", Source: source, Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The verified historical answer."}, Sources: []string{s.Evidence}}}
	}}
	ex, _, runtime := executeFixture(t, root, provider, prepared, session)
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	i, receipt, err := completedReply(ctx, l, p.LogicalRun, "historical")
	if err != nil {
		t.Fatal(err)
	}
	alias := i
	alias.ID = "historical-alias"
	if err := l.PrepareIntent(ctx, p, alias, p.Snapshot.Evidence); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	base.ev.Comments = []map[string]any{}
	base.mu.Unlock()
	ref := filepath.ToSlash(filepath.Join(filepath.Dir(prepared.Workflow), "reply-recovery.yml"))
	recovery, err := ex.Prepare(ctx, workflows.RunRequest{Ref: ref, Inputs: map[string]any{"plan": prepared.Plan, "intent": map[string]any{"id": alias.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	var lost atomic.Bool
	runtime.BeforeCheckpoint = func(name string, _ ActionOutput) error {
		if name == "reply_dispose" && lost.CompareAndSwap(false, true) {
			return errors.New("lost reply disposition checkpoint")
		}
		return nil
	}
	if _, err := ex.Resume(ctx, recovery.ID, nil, false); err == nil || !lost.Load() {
		t.Fatal("actual disposition checkpoint failure not stimulated", err)
	}
	finished, err := ex.Resume(ctx, recovery.ID, nil, true)
	if err != nil || finished.Status != workflows.RunStatusSucceeded || base.writes != 1 {
		t.Fatal("disposition resume changed publications", finished, base.writes, err)
	}
	state, saved, err := runtime.reconcileCompletedIntent(ctx, l, i)
	if err != nil || state != "completed" || digest(saved) != digest(receipt) {
		t.Fatal(state, saved, err)
	}
	snapshot, err := runtime.live(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := runtime.reports(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := runtime.replyCoverage(ctx, p, snapshot, reports)
	if err != nil || len(blockers) != 0 {
		t.Fatal("required obsolete answer still blocked", blockers, err)
	}
	base.mu.Lock()
	base.ev.Comments = append(base.ev.Comments, receipt.Result)
	base.mu.Unlock()
	if _, err := runtime.verifyReplyDisposition(ctx, l, i); err == nil {
		t.Fatal("restored original reused stale no_replacement")
	}
}
