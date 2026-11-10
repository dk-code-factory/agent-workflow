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

type replyRecoveryProvider struct{ *fakeProvider }

func recoveryReviewSource(provider *fakeProvider) *FeedbackSource {
	source := &FeedbackSource{Kind: "review", ID: "17", URL: provider.ev.Metadata.URL + "#pullrequestreview-17"}
	provider.ev.Reviews = []map[string]any{{"id": 17, "html_url": source.URL, "body": "Please explain this change."}}
	return source
}

func (f *replyRecoveryProvider) Execute(ctx context.Context, i Intent) (Receipt, error) {
	receipt, err := f.fakeProvider.Execute(ctx, i)
	if err == nil && contains([]string{"reply", "reply_inline"}, i.Kind) {
		f.mu.Lock()
		user, _ := receipt.Result["user"].(map[string]any)
		user["id"] = i.ActorID
		f.mu.Unlock()
	}
	return receipt, err
}
func (f *replyRecoveryProvider) InspectReply(_ context.Context, i Intent, receipt Receipt) (ReplyObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := ReplyObservation{Intent: i, Receipt: receipt, PR: f.ev, Status: "unavailable"}
	comments := f.ev.Comments
	if i.Kind == "reply_inline" {
		comments = f.ev.Inline
	}
	for _, c := range comments {
		if githubID(c, "id") == githubID(receipt.Result, "id") {
			o.Original = c
			o.Status = "edited"
			if stringValue(c, "body") == stringValue(i.Payload, "body") {
				o.Status = "visible"
			}
		}
	}
	return o, nil
}

func TestAuthorizedSameBodyReplyReplacementIsLinkedAndReplaySafe(t *testing.T) {
	root, base, req := fixture(t)
	base.discussionWrites = true
	provider := &replyRecoveryProvider{base}
	source := recoveryReviewSource(base)
	replacementAuth := Authorization{"replacement-user-request", "Replace the missing answer on this PR with the same verified answer.", "chat:replacement"}
	req.Authorizations = map[string]Authorization{"iterate": {"initial-user-request", "Fix and publish this PR.", "chat:initial"}}
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-feedback-read"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-feedback-reply", "dk-workflows:dk-pr-publish"}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Run = prepared.LogicalRun
	req.Authorizations["reply-replacement:"+req.Run+":original"] = replacementAuth
	prepared, err = Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Path        string           `json:"path"`
		Fingerprint string           `json:"fingerprint"`
		Observation ReplyObservation `json:"observation"`
	}
	session := &fakeSession{assignmentHook: func(b []byte) {
		var a struct {
			Inspection json.RawMessage `json:"reply_inspection"`
		}
		if err := json.Unmarshal(b, &a); err != nil {
			t.Error(err)
			return
		}
		if len(a.Inspection) > 0 {
			if err := json.Unmarshal(a.Inspection, &observed); err != nil {
				t.Error(err)
			}
		}
	}, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "inspection" {
			report.ReplyObligations = []ReplyObligation{{ID: "feedback", Text: "Please explain this change.", Required: true, Reason: "Concrete question.", Sources: []string{s.Evidence}}}
		}
		if o.ID != "reply_recovery" {
			return
		}
		i := Intent{ID: "replacement", Kind: "reply", Source: source, Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The exact supported answer."}, Sources: []string{s.Evidence}}
		i.Replaces = &ReplyReplacement{Run: observed.Observation.Intent.Run, ID: observed.Observation.Intent.ID, Inspection: observed.Path, Observation: observed.Fingerprint, Decision: "replacement_required", Reason: "Current code and complete discussion still require the answer; the verified historical comment is unavailable.", Sources: append(append([]string{}, observed.Observation.Intent.Sources...), observed.Path), Authorization: replacementAuth}
		report.Operations = []Intent{i}
	}, reportReplies: func(o Occurrence, s Snapshot, obligations []retainedObligation, report *Report) {
		if o.ID == "reply_recovery" || o.Kind != "reply" {
			return
		}
		for _, item := range obligations {
			report.Responses = append(report.Responses, ReplyResponse{ID: item.Obligation.ID, Outcome: "reply", Reason: "Verified answer.", Sources: []string{item.Original}, Operation: "original"})
		}
		report.Operations = []Intent{{ID: "original", Kind: "reply", Source: source, Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The exact supported answer."}, Sources: []string{s.Evidence}}}
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
	var originalIntent, originalReceipt string
	if err = l.db.QueryRow("SELECT intent,receipt FROM effects WHERE run=? AND id='original'", p.LogicalRun).Scan(&originalIntent, &originalReceipt); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	base.ev.Comments = []map[string]any{}
	base.mu.Unlock()
	ref := filepath.ToSlash(filepath.Join(filepath.Dir(prepared.Workflow), "reply-recovery.yml"))
	recovery, err := ex.Prepare(ctx, workflows.RunRequest{Ref: ref, Inputs: map[string]any{"plan": prepared.Plan, "intent": map[string]any{"id": "original"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ex.Resume(ctx, recovery.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	replacement, err := l.Intent(ctx, p.LogicalRun, "replacement")
	if err != nil || replacement.ID != "replacement" || replacement.Replaces == nil {
		t.Fatal("replacement incorrectly aliases history", replacement, err)
	}
	if base.writes != 1 {
		t.Fatal("preparation published", base.writes)
	}
	original, receipt, err := completedReply(ctx, l, p.LogicalRun, "original")
	if err != nil {
		t.Fatal(err)
	}
	if state, _, err := runtime.reconcileCompletedIntent(ctx, l, original); err == nil || state != "unknown" {
		t.Fatal("unpublished replacement settled the original", state, err)
	}
	other := replacement
	other.ID = "evade-disposition"
	if err = l.PrepareIntent(ctx, p, other, "fixture"); err == nil {
		t.Fatal("second replacement for same disposition admitted")
	}
	publication, err := ex.Prepare(ctx, workflows.RunRequest{Ref: filepath.ToSlash(filepath.Join(filepath.Dir(prepared.Workflow), "publication.yml")), Inputs: map[string]any{"plan": prepared.Plan, "cycle": "reply-recovery", "intent": map[string]any{"id": "replacement"}}})
	if err != nil {
		t.Fatal(err)
	}
	var lost atomic.Bool
	runtime.BeforeCheckpoint = func(name string, _ ActionOutput) error {
		if name == "publish" && lost.CompareAndSwap(false, true) {
			return errors.New("lost replacement checkpoint")
		}
		return nil
	}
	if _, err = ex.Resume(ctx, publication.ID, nil, false); err == nil || !lost.Load() {
		t.Fatal("missing fault injection", err)
	}
	finished, err := ex.Resume(ctx, publication.ID, nil, true)
	if err != nil || finished.Status != workflows.RunStatusSucceeded || base.writes != 2 {
		t.Fatal("replacement replay duplicated publication", finished, base.writes, err)
	}
	var afterIntent, afterReceipt string
	if err = l.db.QueryRow("SELECT intent,receipt FROM effects WHERE run=? AND id='original'", p.LogicalRun).Scan(&afterIntent, &afterReceipt); err != nil || afterIntent != originalIntent || afterReceipt != originalReceipt {
		t.Fatal("historical receipt rewritten", err)
	}
	if len(base.ev.Comments) != 1 || stringValue(base.ev.Comments[0], "body") != "The exact supported answer." {
		t.Fatal("replacement not visible", base.ev.Comments)
	}
	state, saved, err := runtime.reconcileCompletedIntent(ctx, l, original)
	if err != nil || state != "completed" || digest(saved) != digest(receipt) {
		t.Fatal("visible linked replacement did not settle original with its historical receipt", state, saved, err)
	}
	reports, err := runtime.reports(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.live(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := runtime.replyCoverage(ctx, p, snapshot, reports)
	if err != nil || len(blockers) != 0 {
		t.Fatal("linked replacement did not settle required original answer", blockers, err)
	}
	t.Run("invalid historical receipt", func(t *testing.T) {
		raw, err := os.ReadFile(originalReceipt)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(originalReceipt, raw, 0600); err != nil {
				t.Error(err)
			}
		}()
		changed := receipt
		changed.ID = "another-operation"
		if err := os.WriteFile(originalReceipt, jsonBytes(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if state, _, err := runtime.reconcileCompletedIntent(ctx, l, original); err == nil || state != "unknown" {
			t.Fatal("linked replacement accepted an invalid historical receipt", state, err)
		}
	})
	visible := base.ev.Comments[0]
	for _, change := range []string{"missing", "edited", "wrong actor", "wrong viewer"} {
		t.Run(change, func(t *testing.T) {
			base.mu.Lock()
			comment := jsonObject(visible)
			base.ev.Comments = []map[string]any{comment}
			switch change {
			case "missing":
				base.ev.Comments = []map[string]any{}
			case "edited":
				comment["body"] = "A changed answer."
			case "wrong actor":
				comment["user"] = map[string]any{"id": 8, "login": "other"}
			case "wrong viewer":
				base.ev.ViewerID = 8
			}
			base.mu.Unlock()
			defer func() {
				base.mu.Lock()
				base.ev.Comments = []map[string]any{visible}
				base.ev.ViewerID = original.ActorID
				base.mu.Unlock()
			}()
			if state, _, err := runtime.reconcileCompletedIntent(ctx, l, original); err == nil || state != "unknown" {
				t.Fatal("unverified linked replacement settled original", state, err)
			}
		})
	}
	// A stale disposition cannot authorize another write after new discussion.
	base.mu.Lock()
	base.ev.Comments = append(base.ev.Comments, map[string]any{"id": 999, "body": "New question"})
	base.mu.Unlock()
	if err = runtime.verifyReplyReplacement(ctx, p, replacement); err == nil {
		t.Fatal("stale replacement accepted")
	}
}

func TestReplyReplacementRejectsUncertainOriginalAndMissingAuthority(t *testing.T) {
	root, base, req := fixture(t)
	req.Authorizations = map[string]Authorization{"iterate": {"user", "Publish this PR.", "chat"}}
	prepared, err := Prepare(context.Background(), root, base, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	i := Intent{ID: "unknown", Kind: "reply", Head: base.ev.Metadata.Head.SHA, Lease: base.ev.Metadata.Head.SHA, Target: base.ev.Metadata.Base.SHA, ActorID: 7, Payload: map[string]any{"body": "Answer"}, Sources: []string{"fixture"}}
	if err = l.PrepareIntent(context.Background(), p, i, "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec("UPDATE effects SET state='unknown' WHERE run=? AND id=?", p.LogicalRun, i.ID); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Root: root, Provider: &replyRecoveryProvider{base}}
	if _, err = runtime.replyInspect(context.Background(), p, ActionInput{Intent: i.ID}); err == nil || !strings.Contains(err.Error(), "previously verified") {
		t.Fatal("uncertain original admitted", err)
	}
	i.ID = "replacement"
	i.Replaces = &ReplyReplacement{Run: p.LogicalRun, ID: "unknown", Decision: "replacement_required", Reason: "Answer still required", Inspection: "fixture", Sources: []string{"fixture"}}
	if err = l.PrepareIntent(context.Background(), p, i, "fixture"); err == nil || !strings.Contains(err.Error(), "separate exact replacement") {
		t.Fatal("replacement lacked authority", err)
	}
	if base.writes != 0 {
		t.Fatal("rejected recovery wrote", base.writes)
	}
}

func TestGitHubReplyInspectionDistinguishesMissingEditedAndInaccessible(t *testing.T) {
	root, base, req := fixture(t)
	source := recoveryReviewSource(base)
	id, _ := ParseIdentity(req.PR)
	const created = "9007199254740993"
	comment := map[string]any{"id": json.Number(created), "body": "Original answer", "user": map[string]any{"id": 7, "login": "author"}}
	i := Intent{ID: "original", Kind: "reply", Source: source, PR: id, ActorID: 7, Head: base.ev.Metadata.Head.SHA, Payload: map[string]any{"body": "Original answer"}, Sources: []string{"original-source"}}
	receipt := Receipt{ID: i.ID, Kind: i.Kind, PR: id.URL, Head: i.Head, Result: comment}
	endpoint := "repos/owner/repo/issues/comments/" + created
	responses := map[string]any{
		"repos/owner/repo/pulls/1": base.ev.Metadata,
		"user":                     map[string]any{"id": 7, "login": "author"},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{comment}},
		"repos/owner/repo/pulls/1/comments?per_page=100":  []any{[]any{}},
		"repos/owner/repo/pulls/1/reviews?per_page=100":   []any{[]any{base.ev.Reviews[0]}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
		endpoint:  "HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + string(jsonBytes(comment)),
	}
	update := fakeGH(t, root, responses)
	runtime := &Runtime{Root: root, Provider: &GitHub{Root: root}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct {
		name, status   string
		listed, accept bool
	}{{"visible", "visible", true, true}, {"edited", "edited", true, true}, {"missing", "unavailable", false, true}, {"inconsistent missing", "unavailable", true, false}, {"inaccessible", "denied", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			current := map[string]any{"id": json.Number(created), "body": "Original answer", "user": comment["user"]}
			if tc.status == "edited" {
				current["body"] = "Edited answer"
			}
			responses[endpoint] = "HTTP/2.0 200 OK\r\n\r\n" + string(jsonBytes(current))
			if tc.status == "unavailable" {
				responses[endpoint] = "HTTP/2.0 404 Not Found\r\n\r\n{\"message\":\"Not Found\"}"
			}
			if tc.status == "denied" {
				responses[endpoint] = "HTTP/2.0 403 Forbidden\r\n\r\n{\"message\":\"Forbidden\"}"
			}
			listed := []any{}
			if tc.listed {
				listed = append(listed, current)
			}
			responses["repos/owner/repo/issues/1/comments?per_page=100"] = []any{listed}
			update(responses)
			o, err := runtime.inspectReply(ctx, i, receipt)
			if (err == nil) != tc.accept {
				t.Fatal(o, err)
			}
			if tc.accept {
				path, err := artifact(root, o)
				if err != nil {
					t.Fatal(err)
				}
				var saved ReplyObservation
				if err = readJSON(path, &saved); err != nil || saved.Fingerprint() != o.Fingerprint() || saved.Status != tc.status {
					t.Fatal("inspection changed at rest", saved, err)
				}
			}
		})
	}
}
