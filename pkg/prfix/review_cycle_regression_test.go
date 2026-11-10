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
		})
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
	intent := Intent{ID: "request", Kind: "request_review", PR: id, Head: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"reviewers": []any{"reviewer"}, "team_reviewers": []any{"maintainers"}}}
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
