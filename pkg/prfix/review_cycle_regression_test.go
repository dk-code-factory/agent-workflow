package prfix

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestReconciliationDoesNotInitializeOrRepairLedger(t *testing.T) {
	root, provider, req := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agent-workflow", "pr-fix")
	path := filepath.Join(dir, "ledger.sqlite")
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0755}, {path, 0644}} {
		if err := os.Chmod(entry.path, entry.mode); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Root: root, Provider: provider}
	state, _, err := runtime.reconcile(ctx, "reserve", workflows.ActionContext{OperationKey: "absent"}, ActionInput{Plan: prepared.Plan, Kind: "retry"})
	if err != nil || state != "no_effect" {
		t.Fatal(state, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reconciliation changed ledger bytes", err)
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0755}, {path, 0644}} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatal("reconciliation repaired permissions", entry.path, info, err)
		}
	}
	l, err := runtime.observer(ctx).ledger()
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := l.db.ExecContext(ctx, "CREATE TABLE forbidden(value TEXT)")
	closeErr := l.Close()
	if writeErr == nil || closeErr != nil {
		t.Fatal("observation connection was writable", writeErr, closeErr)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	state, _, err = runtime.reconcile(ctx, "reserve", workflows.ActionContext{}, ActionInput{Plan: prepared.Plan})
	if err == nil || state != "unknown" {
		t.Fatal("missing ledger was interpreted as no effect", state, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reconciliation recreated the missing ledger", err)
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := runtime.observer(stopped).ledger(); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled observation ignored its context", err)
	}
}

type resolutionStateProvider struct{ *fakeProvider }

func (p *resolutionStateProvider) Execute(ctx context.Context, i Intent) (Receipt, error) {
	p.mu.Lock()
	p.ev.Threads[0]["isResolved"] = true
	p.mu.Unlock()
	return p.fakeProvider.Execute(ctx, i)
}

func (p *resolutionStateProvider) Reconcile(_ context.Context, i Intent) (string, Receipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ev.Threads[0]["isResolved"] == true {
		return "completed", p.receipts[i.ID], nil
	}
	return "no_effect", Receipt{}, nil
}

func TestUncertainResolutionDoesNotOverwriteReviewerReopening(t *testing.T) {
	for _, retained := range []string{"dispatched", "unknown"} {
		t.Run(retained, func(t *testing.T) {
			root, base, req := fixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
			base.ev.Threads = []map[string]any{{"id": "thread", "isResolved": false}}
			provider := &resolutionStateProvider{base}
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
			head := base.ev.Metadata.Head.SHA
			intent := Intent{ID: "resolve-once", Kind: "resolve_thread", Head: head, Lease: head, Target: head, Sources: []string{plan.Snapshot.Evidence}, Payload: map[string]any{"thread_id": "thread", "reply_operation": "verified-answer"}}
			if err := l.PrepareIntent(ctx, plan, intent, "fixture"); err != nil {
				t.Fatal(err)
			}
			intent, err = l.Intent(ctx, plan.LogicalRun, intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			base.lose = true
			if _, err := provider.Execute(ctx, intent); err == nil || base.writes != 1 {
				t.Fatal("fixture did not lose a successful resolution response", err, base.writes)
			}
			if base.ev.Threads[0]["isResolved"] != true {
				t.Fatal("original resolution did not reach the remote fixture")
			}
			// The reviewer reopens the thread after the successful but uncertain write.
			base.ev.Threads[0]["isResolved"] = false
			if _, err := l.db.ExecContext(ctx, "UPDATE effects SET state=? WHERE run=? AND id=?", retained, plan.LogicalRun, intent.ID); err != nil {
				t.Fatal(err)
			}
			runtime := &Runtime{Root: root, Provider: provider}
			state, _, err := runtime.reconcile(ctx, "publish", workflows.ActionContext{}, ActionInput{Plan: prepared.Plan, Intent: intent.ID})
			if err != nil || state != "unknown" {
				t.Fatal("current unresolved state authorized replay", state, err)
			}
			if _, err := runtime.publish(ctx, plan, ActionInput{Intent: intent.ID}); err == nil || !strings.Contains(err.Error(), "read-only reconciliation") {
				t.Fatal("uncertain publication did not stop before dispatch", err)
			}
			if base.writes != 1 || base.ev.Threads[0]["isResolved"] != false {
				t.Fatal("reviewer reopening overwritten", base.writes)
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			registry := &workflows.ActionRegistry{}
			if err := runtime.Register(registry); err != nil {
				t.Fatal(err)
			}
			ex := &workflows.Executor{WorkspaceDir: root, Store: store, Roles: prepared.Roles, ApplicationActions: registry}
			engine, err := ex.Prepare(ctx, workflows.RunRequest{Ref: prepared.Workflow, Inputs: prepared.Inputs})
			if err != nil {
				t.Fatal(err)
			}
			// Retain a stopped controller without replaying its uncertain publication.
			engine.Status, engine.Error = workflows.RunStatusFailed, "lost resolution response"
			if err := store.Save(ctx, engine); err != nil {
				t.Fatal(err)
			}
			if err := l.Execution(ctx, plan.LogicalRun, prepared.RequestID, prepared.Plan, engine.ID); err != nil {
				t.Fatal(err)
			}
			req.Run = plan.LogicalRun
			req.Instructions += " Reassess the current thread."
			if _, err := Prepare(ctx, root, provider, req); err == nil || !strings.Contains(err.Error(), "still uncertain") {
				t.Fatal("replacement cleared the uncertain publication barrier", err)
			}
			if err := l.db.QueryRowContext(ctx, "SELECT state FROM effects WHERE run=? AND id=?", plan.LogicalRun, intent.ID).Scan(&state); err != nil || state != retained {
				t.Fatal("replacement changed retained dispatch state", state, err)
			}
			if base.writes != 1 || base.ev.Threads[0]["isResolved"] != false {
				t.Fatal("replacement overwrote reviewer reopening", base.writes)
			}
		})
	}
}

func TestResolutionRequiresTheExactHistoricalInlineAnswer(t *testing.T) {
	root, base, req := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
	base.discussionWrites = true
	rootComment := map[string]any{"id": 9, "body": "Please explain this change.", "user": map[string]any{"id": 8, "login": "reviewer"}}
	base.ev.Inline = []map[string]any{rootComment}
	base.ev.Threads = []map[string]any{{"id": "thread", "isResolved": false, "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 9}}}}}
	provider := &replyRecoveryProvider{base}
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
	head := base.ev.Metadata.Head.SHA
	reply := Intent{ID: "answer", Kind: "reply_inline", ActorID: 7, Head: head, Lease: head, Target: head, Sources: []string{plan.Snapshot.Evidence}, Payload: map[string]any{"comment_id": "9", "body": "The verified answer."}}
	if err := l.PrepareIntent(ctx, plan, reply, "fixture"); err != nil {
		t.Fatal(err)
	}
	reply, err = l.Intent(ctx, plan.LogicalRun, reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := provider.Execute(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Root: root, Provider: provider}
	if _, err := runtime.completeEffect(ctx, l, reply, receipt); err != nil {
		t.Fatal(err)
	}
	resolution := Intent{Kind: "resolve_thread", Payload: map[string]any{"thread_id": "thread", "reply_operation": reply.ID}}
	comment := func(id int, body, actor string, actorID int) map[string]any {
		return map[string]any{"id": id, "body": body, "in_reply_to_id": 9, "user": map[string]any{"id": actorID, "login": actor}}
	}
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		accept   bool
	}{
		{"original visible", []map[string]any{receipt.Result}, true},
		{"original unavailable", []map[string]any{comment(102, "The verified answer.", "reviewer", 8)}, false},
		{"original edited", []map[string]any{comment(101, "Edited answer.", "author", 7), comment(102, "The verified answer.", "reviewer", 8)}, false},
		{"different answer by same actor", []map[string]any{comment(102, "The verified answer.", "author", 7)}, false},
		{"original actor mismatch", []map[string]any{comment(101, "The verified answer.", "reviewer", 8)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := base.ev
			ev.Inline = append([]map[string]any{rootComment}, tc.comments...)
			s := plan.Snapshot
			s.Evidence, err = artifact(root, map[string]any{"pr": ev})
			if err != nil {
				t.Fatal(err)
			}
			err := runtime.threadResolutionGate(ctx, plan, resolution, s)
			if tc.accept && err != nil || !tc.accept && (err == nil || !strings.Contains(err.Error(), "no longer present")) {
				t.Fatal("incorrect historical answer validation", err)
			}
			if base.writes != 1 {
				t.Fatal("resolution validation wrote remotely", base.writes)
			}
		})
	}
}

func TestResourceFingerprintExcludesOnlyPinnedCacheEntries(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "tool")
	if err := os.Mkdir(tool, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool, "entry"), []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := resourceDigestWithOpen(root, 4, os.Open)
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{root, tool} {
		for _, name := range []string{".git", "__pycache__", ".pytest_cache"} {
			dir := filepath.Join(parent, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "generated"), []byte("excluded bytes exceed the remaining budget"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := resourceDigestWithOpen(root, 4, os.Open)
	if err != nil || before != after {
		t.Fatal("excluded generated data changed resource identity or consumed its budget", before, after, err)
	}
	if err := os.WriteFile(filepath.Join(tool, ".cache"), []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err = resourceDigest(root)
	if err != nil || before == after {
		t.Fatal("an additional cache name was incorrectly excluded", err)
	}
}

func TestAlreadyRequestedReviewersSendNoWrite(t *testing.T) {
	root, provider, req := fixture(t)
	id, err := ParseIdentity(req.PR)
	if err != nil {
		t.Fatal(err)
	}
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}},
		"repos/owner/repo/pulls/1/requested_reviewers": map[string]any{"users": []any{map[string]any{"login": "reviewer"}}, "teams": []any{map[string]any{"slug": "maintainers"}}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	fakeGH(t, root, responses)
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PRFIX_FAKE_REQUESTS", log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	intent := Intent{ID: "request", Kind: "request_review", PR: id, ActorID: provider.ev.ViewerID, Head: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"reviewers": []any{"reviewer"}, "team_reviewers": []any{"maintainers"}}}
	receipt, err := (&GitHub{Root: root}).Execute(ctx, intent)
	if err != nil || receipt.ID != intent.ID || !contains(receipt.Evidence, "requested set already satisfied; no write") {
		t.Fatal("already-satisfied request was not reconciled", receipt, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || strings.Contains(string(calls), "POST repos/owner/repo/pulls/1/requested_reviewers") {
		t.Fatal("already-satisfied request sent a POST", string(calls), err)
	}
	if !strings.Contains(string(calls), "GET repos/owner/repo/pulls/1/requested_reviewers") {
		t.Fatal("requested set was not observed", string(calls))
	}
}
