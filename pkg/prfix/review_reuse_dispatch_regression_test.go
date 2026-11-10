package prfix

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublicationRejectsActorDriftBeforeReplyWrite(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
	req.MaintainOther = &Authorization{"maintain", "Maintain this PR even when it belongs to another account.", "chat:maintain"}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "actor-bound", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The verified answer."}, Sources: []string{s.Evidence}}}
		}
	}}
	_, _, runtime := executeFixture(t, root, provider, prepared, session)
	plan, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var audience string
	if err := l.db.QueryRow("SELECT id FROM claims WHERE run=? AND occurrence='audience' AND payload='actor-bound' AND state='complete'", plan.LogicalRun).Scan(&audience); err != nil {
		t.Fatal(err)
	}
	if _, err := l.db.Exec("UPDATE effects SET state='prepared',receipt='' WHERE run=? AND id='actor-bound'", plan.LogicalRun); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.ev.Viewer, provider.ev.ViewerID = "another-account", 8
	provider.writes, provider.receipts = 0, nil
	provider.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := runtime.publish(ctx, plan, ActionInput{Intent: "actor-bound", Claim: audience}); err == nil || !strings.Contains(err.Error(), "publication actor changed") {
		t.Fatal("actor drift was not rejected before publication", err)
	}
	if provider.writes != 0 {
		t.Fatal("wrong actor published before reconciliation", provider.writes)
	}
}

func TestFocusedCommitAcceptsRemovedOwnedDirectory(t *testing.T) {
	root, provider, req := fixture(t)
	path := filepath.Join(root, "obsolete", "nested", "old.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("obsolete\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{{"add", "obsolete"}, {"commit", "-m", "Add obsolete module fixture"}} {
		if _, err := git(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA, provider.ev.Metadata.Base.SHA = head, head
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-feedback-address"}
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var removed atomic.Bool
	session := &fakeSession{reportHook: func(o Occurrence, _ Snapshot, report *Report) {
		if o.Kind == "fix" && removed.CompareAndSwap(false, true) {
			if err := os.RemoveAll(filepath.Join(root, "obsolete")); err != nil {
				t.Error(err)
				return
			}
			report.Changes = []FileChange{{Path: "obsolete/nested/old.txt", Before: rawHash([]byte("obsolete\n")), After: "deleted"}}
			report.CommitMessage = "Remove the obsolete module"
		}
	}}
	_, _, _ = executeFixture(t, root, provider, prepared, session)
	paths, err := git(ctx, root, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil || paths != "obsolete/nested/old.txt" {
		t.Fatal("deleted directory was not committed", paths, err)
	}
	if _, err := git(ctx, root, "cat-file", "-e", "HEAD:obsolete/nested/old.txt"); err == nil {
		t.Fatal("owned deletion remained in committed tree")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := verifyOwnedParent(root, "alias/missing/file.txt", true); err == nil {
		t.Fatal("deleted-parent support admitted a symlink alias")
	}
}

func TestReviewerReuseSurvivesReorderingAndUnrelatedFinalizer(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{"custom:first", "custom:second"}
	first, second := reviewAdapter(), reviewAdapter()
	first.Identity, second.Identity = "custom:first", "custom:second"
	req.Adapters = map[string]Adapter{first.Identity: first, second.Identity: second}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
	plan, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	req.Run = plan.LogicalRun
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, addFinalizer := range []bool{false, true} {
		req.Options["tasks"] = []any{"custom:second", "custom:first"}
		if addFinalizer {
			req.Options["finalizers"] = []any{map[string]any{"instructions": "Inspect unrelated final completion evidence."}}
		}
		replacement, err := Prepare(ctx, root, provider, req)
		if err != nil {
			t.Fatal(err)
		}
		current, err := ReadPlan(replacement.Plan)
		if err != nil {
			t.Fatal(err)
		}
		out, err := runtime.reviewAdmission(ctx, current, ActionInput{Cycle: "unchanged-reviewers"})
		if err != nil || out.Invoke {
			t.Fatal("unchanged reviewers consumed another pass", addFinalizer, out, err)
		}
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var count int
	if err := l.db.QueryRow("SELECT count(*) FROM reservations WHERE run=? AND kind='review'", plan.LogicalRun).Scan(&count); err != nil || count != 1 {
		t.Fatal("reordering consumed review budget", count, err)
	}
	req.Options["tasks"] = []any{"custom:second", map[string]any{"skill": "custom:first", "instructions": "Review the changed semantic requirement."}}
	changed, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	changedPlan, err := ReadPlan(changed.Plan)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runtime.reviewAdmission(ctx, changedPlan, ActionInput{Cycle: "changed-reviewer"})
	if err != nil || !out.Invoke {
		t.Fatal("changed reviewer instructions reused stale analysis", out, err)
	}
}

func TestReviewRequestFingerprintUsesHistoricalReferences(t *testing.T) {
	o := ReviewRequestObservation{Intent: Intent{ID: "request"}, PR: PREvidence{ViewerID: 7, Comments: []map[string]any{}, Reviews: []map[string]any{{"id": 11, "state": "COMMENTED", "submitted_at": "then", "commit_id": "head", "body": "Review text.", "user": map[string]any{"login": "reviewer", "id": 9}}}}, Requested: map[string]any{"users": []any{}, "teams": []any{}}, Timeline: []map[string]any{{"id": 10, "event": "review_requested", "created_at": "then", "requested_reviewer": map[string]any{"login": "reviewer", "id": 9}}}}
	before := o.Fingerprint()
	o.PR.Metadata.Updated = "later"
	o.PR.Comments = append(o.PR.Comments, map[string]any{"id": 20, "body": "An independently authorized answer."})
	o.PR.Reviews[0]["body"] = "Edited review text."
	o.Timeline = append(o.Timeline, map[string]any{"id": 12, "event": "labeled", "created_at": "later"})
	if o.Fingerprint() != before {
		t.Fatal("unrelated discussion or timeline changed request reference fingerprint")
	}
	o.PR.Reviews[0]["state"] = "APPROVED"
	if o.Fingerprint() == before {
		t.Fatal("changed historical review state retained stale disposition")
	}
	o.Requested["users"] = []any{map[string]any{"id": json.Number("9007199254740992"), "login": "reviewer"}}
	largeID := o.Fingerprint()
	o.Requested["users"] = []any{map[string]any{"id": json.Number("9007199254740993"), "login": "reviewer"}}
	if o.Fingerprint() == largeID {
		t.Fatal("requested-reviewer fingerprint rounded distinct integer identities")
	}
}

func TestExistingRemoteAnswersAndDescriptionDoNotWrite(t *testing.T) {
	for _, kind := range []string{"reply", "reply_inline", "edit_pr"} {
		t.Run(kind, func(t *testing.T) {
			root, provider, req := fixture(t)
			id, err := ParseIdentity(req.PR)
			if err != nil {
				t.Fatal(err)
			}
			answer := map[string]any{"id": 71, "body": "The existing answer.", "in_reply_to_id": 7, "user": map[string]any{"login": "author", "id": 7}}
			responses := map[string]any{
				"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
				"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{answer}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{answer}},
				"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
			}
			update := fakeGH(t, root, responses)
			calls := filepath.Join(t.TempDir(), "requests")
			t.Setenv("PRFIX_FAKE_REQUESTS", calls)
			intent := Intent{ID: "existing", Kind: kind, PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA, Before: map[string]any{"comment_ids": []string{"71"}, "satisfied_comment_id": "71"}, Payload: map[string]any{"body": "The existing answer.", "comment_id": "7"}}
			if kind == "edit_pr" {
				intent.Payload = map[string]any{"body": provider.ev.Metadata.Body, "title": provider.ev.Metadata.Title}
			}
			g := &GitHub{Root: root}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, err := g.Execute(ctx, intent)
			if err != nil || receipt.ID != intent.ID || len(receipt.Evidence) == 0 {
				t.Fatal("existing remote state did not return verified no-op", receipt, err)
			}
			state, reconciled, err := g.Reconcile(ctx, intent)
			if err != nil || state != "completed" || kind != "edit_pr" && githubID(reconciled.Result, "id") != "71" {
				t.Fatal("no-op receipt lost its exact existing identity", state, reconciled, err)
			}
			if kind != "edit_pr" {
				answer["body"] = "The edited answer."
				update(responses)
				if _, err := g.Execute(ctx, intent); err == nil {
					t.Fatal("changed existing answer was reposted instead of requiring preparation")
				}
			}
			log, err := os.ReadFile(calls)
			if err != nil || strings.Contains(string(log), "POST repos/") || strings.Contains(string(log), "PATCH repos/") {
				t.Fatal("no-op or stale no-op dispatched a write", string(log), err)
			}
		})
	}
}

func TestAcknowledgedRequestResponseSurvivesObservationFailure(t *testing.T) {
	root, provider, req := fixture(t)
	id, err := ParseIdentity(req.PR)
	if err != nil {
		t.Fatal(err)
	}
	responses := map[string]any{
		"repos/owner/repo/pulls/1": map[string]any{"__sequence": []any{provider.ev.Metadata, provider.ev.Metadata, "{malformed observation"}}, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}},
		"repos/owner/repo/pulls/1/requested_reviewers":    map[string]any{"__sequence": []any{map[string]any{"users": []any{}, "teams": []any{}}, map[string]any{"accepted": true, "request_id": json.Number("9007199254740993")}}},
		"repos/owner/repo/issues/1/timeline?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	calls := filepath.Join(t.TempDir(), "requests")
	t.Setenv("PRFIX_FAKE_REQUESTS", calls)
	intent := Intent{ID: "acknowledged", Kind: "request_review", PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"reviewers": []any{"reviewer"}, "team_reviewers": []any{}}}
	g := &GitHub{Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := g.Execute(ctx, intent)
	if err == nil || receipt.ID != "" || !strings.Contains(err.Error(), "response retained") {
		t.Fatal("accepted response followed by failed observation was not distinguished", receipt, err)
	}
	var retained DispatchResponse
	if err := readJSON(g.dispatchResponsePath(intent), &retained); err != nil || retained.Intent.Fingerprint() != intent.Fingerprint() || retained.Response["accepted"] != true || githubID(retained.Response, "request_id") != "9007199254740993" {
		t.Fatal("successful dispatch response was lost", retained, err)
	}
	responses["repos/owner/repo/pulls/1"] = provider.ev.Metadata
	responses["repos/owner/repo/pulls/1/requested_reviewers"] = map[string]any{"users": []any{}, "teams": []any{}}
	update(responses)
	observation, err := g.InspectReviewRequest(ctx, intent)
	if err != nil || observation.Dispatch == nil || observation.Dispatch.Response["accepted"] != true {
		t.Fatal("read-only recovery omitted acknowledged response", observation.Dispatch, err)
	}
	log, err := os.ReadFile(calls)
	if err != nil || strings.Count(string(log), "POST repos/owner/repo/pulls/1/requested_reviewers\n") != 1 {
		t.Fatal("inspection resent the acknowledged request", string(log), err)
	}
}
