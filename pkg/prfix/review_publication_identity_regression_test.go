package prfix

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestResolutionObservationRequiresUnchangedHistoricalAnswer(t *testing.T) {
	root, provider, req := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	head := provider.ev.Metadata.Head.SHA
	reply := Intent{ID: "answer", Kind: "reply_inline", ActorID: 7, Head: head, Lease: head, Target: head, Sources: []string{plan.Snapshot.Evidence}, Payload: map[string]any{"comment_id": "9", "body": "The verified answer."}}
	if err := l.PrepareIntent(ctx, plan, reply, "fixture"); err != nil {
		t.Fatal(err)
	}
	reply, err = l.Intent(ctx, plan.LogicalRun, reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	comment := func(id int, body string, actor int) map[string]any {
		return map[string]any{"id": id, "body": body, "in_reply_to_id": 9, "user": map[string]any{"id": actor, "login": "author"}}
	}
	original := comment(101, "The verified answer.", 7)
	receipt := Receipt{reply.ID, reply.Kind, reply.PR.URL, reply.Head, original, []string{"verified original answer"}}
	runtime := &Runtime{Root: root}
	if _, err := runtime.completeEffect(ctx, l, reply, receipt); err != nil {
		t.Fatal(err)
	}
	thread := map[string]any{"id": "thread", "isResolved": true, "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 9}}}}
	rootComment := map[string]any{"id": 9, "body": "Please explain this change."}
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 1, "nodes": []any{thread}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	resolution := Intent{ID: "resolution", Run: plan.LogicalRun, Kind: "resolve_thread", PR: plan.Identity, Head: head, Payload: map[string]any{"thread_id": "thread", "reply_operation": reply.ID}}
	g := &GitHub{Root: root}
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		viewer   int
		accept   bool
	}{
		{"unchanged", []map[string]any{original}, 7, true},
		{"missing with same-actor substitute", []map[string]any{comment(102, "The verified answer.", 7)}, 7, false},
		{"edited with same-actor substitute", []map[string]any{comment(101, "Edited answer.", 7), comment(102, "The verified answer.", 7)}, 7, false},
		{"wrong original actor", []map[string]any{comment(101, "The verified answer.", 8)}, 7, false},
		{"wrong current actor", []map[string]any{original}, 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses["user"] = map[string]any{"login": "author", "id": tc.viewer}
			nodes := []any{rootComment}
			for _, c := range tc.comments {
				nodes = append(nodes, c)
			}
			responses["repos/owner/repo/pulls/1/comments?per_page=100"] = []any{nodes}
			update(responses)
			state, _, err := g.Reconcile(ctx, resolution)
			if tc.accept && (err != nil || state != "completed") || !tc.accept && (err == nil || state != "unknown") {
				t.Fatal("resolved state bypassed historical answer verification", state, err)
			}
			if tc.accept {
				thread["isResolved"] = false
				update(responses)
				state, _, err = g.Reconcile(ctx, resolution)
				thread["isResolved"] = true
				if err != nil || state != "no_effect" {
					t.Fatal("unresolved state with a verified answer was not observed", state, err)
				}
			}
		})
	}
}

func TestLostPublishCheckpointPreservesHistoricalReplyReceipt(t *testing.T) {
	root, provider, req := fixture(t)
	provider.discussionWrites = true
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	propose := true
	session := &fakeSession{failCheckpoint: "publish", reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" && propose {
			report.Operations = []Intent{{ID: "answer", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The verified answer."}, Sources: []string{s.Evidence}}}
		}
	}}
	ex, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	propose = false
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	plan, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	intent, err := l.Intent(ctx, plan.LogicalRun, "answer")
	if err != nil {
		t.Fatal(err)
	}
	var originalPath string
	if err := l.db.QueryRowContext(ctx, "SELECT receipt FROM effects WHERE run=? AND id=? AND state='completed'", intent.Run, intent.ID).Scan(&originalPath); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	substitute := provider.receipts[intent.ID]
	substitute.Result = map[string]any{"id": 102, "body": "The verified answer.", "user": map[string]any{"login": "author", "id": intent.ActorID}}
	provider.receipts[intent.ID] = substitute
	provider.ev.Comments = []map[string]any{substitute.Result}
	provider.mu.Unlock()
	state, observed, err := provider.Reconcile(ctx, intent)
	if err != nil || state != "completed" || githubID(observed.Result, "id") != "102" {
		t.Fatal("fixture did not supply the replacement creation", state, observed, err)
	}
	run, err = resumeFixture(t, ex, run.ID, true)
	if err != nil || run == nil {
		t.Fatal("historical publication checkpoint did not recover", err)
	}
	if run.Status != workflows.RunStatusSucceeded || provider.writes != 1 {
		t.Fatal("historical publication was replayed or not recovered", run.Status, provider.writes)
	}
	attempts, err := runtime.Store.ActionAttempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := false
	for _, attempt := range attempts {
		if attempt.Contract.Name != "prfix_publish" || len(attempt.Reconciliations) == 0 {
			continue
		}
		var out ActionOutput
		if err := json.Unmarshal(attempt.Result.Output, &out); err != nil {
			t.Fatal(err)
		}
		if attempt.State == "completed" && attempt.Reconciliations[len(attempt.Reconciliations)-1].Outcome == "completed" && out.Receipt != nil && githubID(out.Receipt.Result, "id") == "101" {
			recovered = true
		}
	}
	if !recovered {
		t.Fatal("the engine did not recover its lost publish checkpoint with the original receipt")
	}
	state, _, err = runtime.reconcileCompletedIntent(ctx, l, intent)
	if err == nil || state != "unknown" || !strings.Contains(err.Error(), "historical receipt") {
		t.Fatal("a new proposal accepted the substitute as the original answer", state, err)
	}
	if _, err := runtime.completeEffect(ctx, l, intent, substitute); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatal("a substitute receipt replaced completed publication evidence", err)
	}
	var retainedPath string
	if err := l.db.QueryRowContext(ctx, "SELECT receipt FROM effects WHERE run=? AND id=? AND state='completed'", intent.Run, intent.ID).Scan(&retainedPath); err != nil {
		t.Fatal(err)
	}
	retainedBytes, err := os.ReadFile(retainedPath)
	if err != nil || retainedPath != originalPath || string(retainedBytes) != string(originalBytes) {
		t.Fatal("historical publication receipt changed", originalPath, retainedPath, err)
	}
}
