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

func TestFocusedCommitUsesLiteralManifestPaths(t *testing.T) {
	root, provider, req := fixture(t)
	// A tracked empty literal wildcard passes the old empty git-show expansion;
	// the later pathspec add can then stage unrelated matching files.
	if err := os.WriteFile(filepath.Join(root, "*.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--literal-pathspecs", "add", "--", "*.txt"}, {"commit", "-m", "Add the literal filename fixture"}} {
		if _, err := git(context.Background(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := git(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA, provider.ev.Metadata.Base.SHA = head, head
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-feedback-address"}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var changed atomic.Bool
	session := &fakeSession{reportHook: func(o Occurrence, _ Snapshot, report *Report) {
		if o.Kind != "fix" || !changed.CompareAndSwap(false, true) {
			return
		}
		for name, body := range map[string]string{"*.txt": "owned\n", "user-notes.txt": "private unrelated notes\n"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
				t.Error(err)
			}
		}
		report.CommitMessage = "Add the accepted literal file"
		report.Changes = []FileChange{{Path: "*.txt", Before: rawHash(nil), After: rawHash([]byte("owned\n"))}}
	}}
	_, _, _ = executeFixture(t, root, provider, prepared, session)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paths, err := git(ctx, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil || paths != "*.txt" {
		t.Fatal("manifest escape", paths, err)
	}
	if _, err = git(ctx, root, "cat-file", "-e", "HEAD:user-notes.txt"); err == nil {
		t.Fatal("unrelated notes committed")
	}
	b, err := os.ReadFile(filepath.Join(root, "user-notes.txt"))
	if err != nil || string(b) != "private unrelated notes\n" {
		t.Fatal("unrelated notes changed", err)
	}
}

func TestUntrackedSnapshotRejectsOversizedSparseFile(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, "large-build-artifact"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(65 << 20)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = capture(ctx, root, provider, p)
	if err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatal("unbounded snapshot accepted", err)
	}
	if provider.writes != 0 {
		t.Fatal("snapshot wrote remotely")
	}
}

func TestAlternateReplyIDsShareOneRetainedEffect(t *testing.T) {
	root, provider, req := fixture(t)
	provider.discussionWrites = true
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	req.Authorizations = map[string]Authorization{"iterate": {"reply-authority", "Fix and publish this PR.", "chat:reply"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind != "description" {
			return
		}
		for _, id := range []string{"original-reply", "alternate-reply"} {
			report.Operations = append(report.Operations, Intent{ID: id, Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "Verified exact answer."}, Sources: []string{s.Evidence}})
		}
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true || provider.writes != 1 {
		t.Fatal("duplicate reply dispatched", run.Outputs, provider.writes)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	original, err := l.Intent(context.Background(), p.LogicalRun, "alternate-reply")
	if err != nil || original.ID != "original-reply" {
		t.Fatal("lost original identity", original, err)
	}
	// A checkpoint replay through the alternate ID reads the canonical receipt.
	state, out, err := runtime.reconcile(context.Background(), "receipt", workflows.ActionContext{}, ActionInput{Plan: prepared.Plan, Intent: "alternate-reply"})
	if err != nil || state != "completed" || out.Receipt == nil || out.Receipt.ID != "original-reply" || provider.writes != 1 {
		t.Fatal(state, out, err)
	}
}

func TestGitHubMergeReadinessRequiresBothPredicates(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head := provider.ev.Metadata.Head.SHA
	metadata := provider.ev.Metadata
	metadata.MergeableState = "clean"
	pr := map[string]any{"headRefOid": head, "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": nil}
	responses := map[string]any{"repos/owner/repo/pulls/1": metadata, "graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}}}
	update := fakeGH(t, root, responses)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, rest, graphql, review string
		accept                      bool
	}{{"clean", "clean", "CLEAN", "", true}, {"approved", "clean", "CLEAN", "APPROVED", true}, {"rest missing", "", "CLEAN", "", false}, {"rest blocked", "blocked", "CLEAN", "", false}, {"graphql blocked", "clean", "BLOCKED", "", false}, {"review required", "clean", "CLEAN", "REVIEW_REQUIRED", false}} {
		t.Run(tc.name, func(t *testing.T) {
			metadata.MergeableState = tc.rest
			responses["repos/owner/repo/pulls/1"] = metadata
			pr["mergeStateStatus"] = tc.graphql
			pr["reviewDecision"] = tc.review
			update(responses)
			proof, err := (&GitHub{Root: root}).MergeReady(ctx, id, head)
			if err != nil || proof.Satisfied != tc.accept {
				t.Fatal(proof, err)
			}
			var saved map[string]json.RawMessage
			if len(proof.Evidence) != 1 || readJSON(proof.Evidence[0], &saved) != nil || saved["rest"] == nil || saved["graphql"] == nil {
				t.Fatal("missing dual observation", proof)
			}
		})
	}
}

// The write happened, but the reviewer completed before any acknowledgement.
// Current requested reviewers cannot establish historical delivery identity.
type completedRequestProvider struct{ *fakeProvider }

func TestReviewRequestRecoveryRejectsUndispatchedIntent(t *testing.T) {
	root, provider, req := fixture(t)
	req.Authorizations = map[string]Authorization{"iterate": {"user", "Fix and publish this PR.", "chat:1"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
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
	head := provider.ev.Metadata.Head.SHA
	i := Intent{ID: "not-dispatched", Kind: "request_review", Head: head, Lease: head, Target: head, ActorID: 7, Sources: []string{"fixture"}, Payload: map[string]any{"reviewers": []any{"reviewer"}}}
	if err = l.PrepareIntent(context.Background(), p, i, "fixture"); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Root: root, Provider: provider}
	if _, err = runtime.reviewRequestInspect(context.Background(), p, ActionInput{Intent: i.ID, Kind: "uncertain"}); err == nil || !strings.Contains(err.Error(), "original uncertain") {
		t.Fatal("recovery admitted a new request", err)
	}
	if provider.writes != 0 {
		t.Fatal("recovery wrote remotely")
	}
}

func TestGitHubReviewRequestInspectionRetainsExactHistory(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	large := json.Number("9007199254740993")
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}},
		"repos/owner/repo/pulls/1/comments?per_page=100":  []any{[]any{}},
		"repos/owner/repo/pulls/1/reviews?per_page=100":   []any{[]any{map[string]any{"id": large, "state": "COMMENTED", "user": map[string]any{"login": "reviewer"}}}},
		"repos/owner/repo/issues/1/timeline?per_page=100": []any{[]any{map[string]any{"id": large, "event": "review_requested"}}, []any{map[string]any{"id": json.Number("9007199254740994"), "event": "review_request_removed"}}},
		"repos/owner/repo/pulls/1/requested_reviewers":    map[string]any{"users": []any{}, "teams": []any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	runtime := &Runtime{Root: root, Provider: &GitHub{Root: root}}
	intent := Intent{ID: "uncertain-request", Kind: "request_review", PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	observed, err := runtime.inspectReviewRequest(ctx, intent)
	if err != nil || len(observed.Timeline) != 2 || len(observed.PR.Reviews) != 1 {
		t.Fatal(observed, err)
	}
	path, err := artifact(root, observed)
	if err != nil {
		t.Fatal(err)
	}
	var restored ReviewRequestObservation
	if err = readJSON(path, &restored); err != nil || restored.Fingerprint() != observed.Fingerprint() || githubID(restored.Timeline[0], "id") != large.String() {
		t.Fatal("history identity lost", restored, err)
	}
	delete(responses, "repos/owner/repo/issues/1/timeline?per_page=100")
	update(responses)
	if _, err = runtime.inspectReviewRequest(ctx, intent); err == nil {
		t.Fatal("incomplete history accepted")
	}
}

func (f *completedRequestProvider) Reconcile(ctx context.Context, i Intent) (string, Receipt, error) {
	if i.Kind == "request_review" {
		return "unknown", Receipt{}, nil
	}
	return f.fakeProvider.Reconcile(ctx, i)
}
func (f *completedRequestProvider) InspectReviewRequest(_ context.Context, i Intent) (ReviewRequestObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ReviewRequestObservation{Intent: i, PR: f.ev, Requested: map[string]any{"users": []any{}, "teams": []any{}}, Timeline: []map[string]any{{"id": 10, "event": "review_requested", "requested_reviewer": map[string]any{"login": "reviewer"}}}}, nil
}

func TestUncertainReviewRequestNoResendRecoveryAndLostCheckpoint(t *testing.T) {
	root, base, req := fixture(t)
	base.lose = true
	provider := &completedRequestProvider{base}
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	req.Authorizations = map[string]Authorization{"iterate": {"review-authority", "Fix and publish this PR.", "chat:review"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var inspectionPath, inspectionFingerprint string
	var proposed atomic.Bool
	session := &fakeSession{assignmentHook: func(b []byte) {
		var a struct {
			Inspection *struct {
				Path        string `json:"path"`
				Fingerprint string `json:"fingerprint"`
			} `json:"review_request_inspection"`
		}
		if err := json.Unmarshal(b, &a); err != nil {
			t.Error(err)
			return
		}
		if a.Inspection != nil {
			inspectionPath = a.Inspection.Path
			inspectionFingerprint = a.Inspection.Fingerprint
		}
	}, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" && proposed.CompareAndSwap(false, true) {
			report.Operations = []Intent{{ID: "request-once", Kind: "request_review", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"reviewers": []any{"reviewer"}, "team_reviewers": []any{}}, Sources: []string{s.Evidence}}}
		}
		if o.Kind == "audience" && inspectionPath != "" {
			report.ReviewRequestDisposition = &ReviewRequestDisposition{Decision: "no_resend", Reason: "The reviewer completed; current code and discussion require no further request.", Observation: inspectionFingerprint, Sources: []string{inspectionPath}}
		}
	}}
	ex, original, runtime := executeFixture(t, root, provider, prepared, session, true)
	if base.writes != 1 {
		t.Fatal(base.writes)
	}
	base.mu.Lock()
	base.ev.Reviews = []map[string]any{{"id": 11, "state": "COMMENTED", "user": map[string]any{"login": "reviewer"}}}
	base.mu.Unlock()
	// This shared deadline covers recovery, injected checkpoint loss and the
	// original plan's remaining finalizers under the concurrent race shards.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ref := filepath.ToSlash(filepath.Join(filepath.Dir(prepared.Workflow), "review-request-recovery.yml"))
	recovery, err := ex.Prepare(ctx, workflows.RunRequest{Ref: ref, Inputs: map[string]any{"plan": prepared.Plan, "intent": map[string]any{"id": "request-once"}}})
	if err != nil {
		t.Fatal(err)
	}
	var lost atomic.Bool
	runtime.BeforeCheckpoint = func(name string, _ ActionOutput) error {
		if name == "review_request_dispose" && lost.CompareAndSwap(false, true) {
			return errors.New("lost disposition checkpoint")
		}
		return nil
	}
	if _, err = ex.Resume(ctx, recovery.ID, nil, false); err == nil || !lost.Load() {
		t.Fatal("missing injected checkpoint loss", err)
	}
	recovered, err := ex.Resume(ctx, recovery.ID, nil, true)
	if err != nil || recovered.Status != workflows.RunStatusSucceeded {
		t.Fatal(recovered, err)
	}
	finished, err := ex.Resume(ctx, original.ID, nil, true)
	if err != nil || finished.Status != workflows.RunStatusSucceeded || base.writes != 1 {
		t.Fatal("recovery resent or blocked", finished, base.writes, err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p, _ := ReadPlan(prepared.Plan)
	var state, receipt, path string
	if err = l.db.QueryRow("SELECT state,receipt FROM effects WHERE run=? AND id='request-once'", p.LogicalRun).Scan(&state, &receipt); err != nil || state != "disposed" || receipt != "" {
		t.Fatal("invented receipt", state, receipt, err)
	}
	if err = l.db.QueryRow("SELECT artifact FROM review_request_dispositions WHERE run=? AND id='request-once'", p.LogicalRun).Scan(&path); err != nil {
		t.Fatal(err)
	}
	var disposition retainedReviewRequestDisposition
	if err = readJSON(path, &disposition); err != nil || disposition.Delivery != "unknown" || disposition.HistoricalState != "dispatched" {
		t.Fatal(disposition, err)
	}
	state, _, err = runtime.reconcile(ctx, "review_request_dispose", workflows.ActionContext{OperationKey: "another-action"}, ActionInput{Plan: prepared.Plan, Intent: "request-once", Claim: disposition.Claim})
	if err != nil || state != "no_effect" {
		t.Fatal("another action reused a disposition receipt", state, err)
	}
	intent, err := l.Intent(ctx, p.LogicalRun, "request-once")
	if err != nil {
		t.Fatal(err)
	}
	newRequest := intent
	newRequest.ID = "request-under-another-id"
	if err := l.PrepareIntent(ctx, p, newRequest, "fixture:new-id"); err != nil {
		t.Fatal(err)
	}
	newRequest, err = l.Intent(ctx, p.LogicalRun, newRequest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.publicationGate(ctx, p, newRequest, disposition.Claim); err == nil || !strings.Contains(err.Error(), "pending request_review") {
		t.Fatal("disposed unknown delivery allowed a fresh request ID", err)
	}
	answer := Intent{ID: "independent-answer", Kind: "reply", ActorID: 7, Head: intent.Head, Lease: intent.Lease, Target: intent.Target, Branch: intent.Branch, HeadRepository: intent.HeadRepository, Sources: []string{p.Snapshot.Evidence}, Payload: map[string]any{"body": "The independently authorized answer."}}
	if err := l.PrepareIntent(ctx, p, answer, "fixture:answer"); err != nil {
		t.Fatal(err)
	}
	answer, err = l.Intent(ctx, p.LogicalRun, answer.ID)
	if err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	base.discussionWrites, base.lose = true, false
	base.mu.Unlock()
	answerReceipt, err := provider.Execute(ctx, answer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.completeEffect(ctx, l, answer, answerReceipt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.verifyReviewRequestDisposition(ctx, l, intent); err != nil {
		t.Fatal("independent verified answer invalidated request disposition", err)
	}
	base.mu.Lock()
	base.ev.Comments = append(base.ev.Comments, map[string]any{"id": 12, "body": "Please request another review."})
	base.mu.Unlock()
	if err = runtime.verifyReviewRequestDisposition(ctx, l, intent); err == nil {
		t.Fatal("stale disposition accepted")
	}
	if base.writes != 2 {
		t.Fatal("stale inspection wrote")
	}
}
