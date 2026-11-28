package workflows

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func sdkAttemptFixture(t *testing.T) (*SQLiteStore, context.Context, UsageIdentity) {
	t.Helper()
	store, ctx, id := managedUsageFixture(t)
	if _, err := store.db.Exec(`DELETE FROM usage_observations WHERE invocation_id=?`, id.ID); err != nil {
		t.Fatal(err)
	}
	id.Provider = "claude-sdk"
	raw, _ := json.Marshal(id)
	if _, err := store.db.Exec(`UPDATE usage_invocations SET record=? WHERE id=?`, raw, id.ID); err != nil {
		t.Fatal(err)
	}
	return store, ctx, id
}
func TestManagedSDKOriginalAttemptCannotRestartOrAcknowledgeUnknown(t *testing.T) {
	store, ctx, id := sdkAttemptFixture(t)
	if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, "fixture"); err == nil {
		t.Fatal("fresh attempt borrowed consumed admission")
	}
	if err := store.ObserveUsage(ctx, id, UsageUpdate{Key: "sdk-response:known", Provider: "claude-sdk", Model: "fixture", SessionID: "session", Usage: map[string]any{"input_tokens": 16, "output_tokens": 5}, Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	manager := &WorkerManager{Store: store, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return &fakeSession{}, nil, nil, nil
	}}
	w, err := manager.Get(ctx, id.RunID, id.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := manager.Messages(ctx, id.RunID, id.WorkerID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	msg := messages[0]
	w.Status = RunStatusNeedsAttention
	msg.Status = RunStatusFailed
	msg.UnsafeFailureClass = "integrity"
	raw, _ := json.Marshal(w)
	if _, err = store.db.Exec(`UPDATE workers SET status=?,record=? WHERE id=?`, w.Status, raw, w.ID); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(msg)
	if _, err = store.db.Exec(`UPDATE messages SET status=?,record=? WHERE id=?`, msg.Status, raw, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err = manager.Resume(ctx, id.RunID, w.ID); err == nil {
		t.Fatal("public Resume dropped original unknown SDK attempt")
	}
	if _, err = manager.Send(ctx, id.RunID, w.ID, "new-request", "must not run"); err == nil {
		t.Fatal("new SDK work bypassed original hold")
	}
	state := store.Dir
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(state)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	manager.Store = reopened
	if err = manager.Resume(context.Background(), id.RunID, w.ID); err == nil {
		t.Fatal("SQLite restart erased SDK original attempt hold")
	}
	report, err := reopened.Usage(context.Background(), id.RunID)
	if err != nil || report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 16 {
		t.Fatal("original known charges lost or unknown declared complete", report, err)
	}
	current, err := manager.Get(context.Background(), id.RunID, w.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal(current, err)
	}
	reloaded, err := manager.Messages(context.Background(), id.RunID, w.ID)
	if err != nil || len(reloaded) != 1 || reloaded[0].ID != msg.ID || !reloaded[0].StartedAt.Equal(msg.StartedAt) {
		t.Fatal("restart changed original message/deadline", reloaded, err)
	}
}
func TestManagedSDKTerminalRequiresOriginalPrivateCheckpointAndCurrentOwner(t *testing.T) {
	store, ctx, id := sdkAttemptFixture(t)
	if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	manager := &WorkerManager{Store: store, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return &fakeSession{}, nil, nil, nil
	}}
	w, err := manager.Get(ctx, id.RunID, id.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := manager.Messages(ctx, id.RunID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := SessionResult{Status: RunStatusSucceeded, ProviderStatus: "success", ProviderVersion: "claude-agent-sdk@0.3.322", SessionID: "session", Outputs: map[string]any{"text": "original", "cost_usd": float64(0), "usage": map[string]any{"input_tokens": 16, "output_tokens": 5}}}
	if err = CheckpointManagedSDKTerminal(ctx, result); err == nil {
		t.Fatal("provider result installed its own checkpoint authority")
	}
	if err = store.ObserveUsage(ctx, id, UsageUpdate{Key: "sdk-response:known-terminal", Provider: "claude-sdk", Model: "fixture", SessionID: "session", Usage: map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	result.Outputs["usage"] = map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	ctx = context.WithValue(ctx, managedSDKTerminalKey{}, manager.sdkTerminalRecorder(w, messages[0], nil))
	if err = CheckpointManagedSDKTerminal(ctx, result); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := messages[0]
	msg.Status = RunStatusSucceeded
	changed := result
	changed.Outputs = map[string]any{"text": "changed", "usage": result.Outputs["usage"]}
	if err = settleManagedSDKAttempt(ctx, tx, w, msg, changed); err == nil {
		t.Fatal("terminal result replacement accepted")
	}
	tx.Rollback()
	binding, _ := runtimeexec.BindingFrom(ctx)
	binding.Owner = "stale"
	stale := runtimeexec.WithBinding(ctx, binding)
	if err = CheckpointManagedSDKTerminal(stale, result); err == nil {
		t.Fatal("stale owner committed terminal")
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleManagedSDKAttempt(ctx, tx, w, msg, result); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = managedSDKWorkerHold(ctx, tx, w.ID); err != nil {
		t.Fatal("settled original remained held", err)
	}
}

func TestManagedSDKRequestedModelBindsOnceBeforeOriginalResponses(t *testing.T) {
	for _, selector := range []string{"sonnet", ""} {
		t.Run("selector-"+selector, func(t *testing.T) {
			store, ctx, id := sdkAttemptFixture(t)
			id.Model = selector
			raw, _ := json.Marshal(id)
			if _, err := store.db.Exec(`UPDATE usage_invocations SET record=? WHERE id=?`, raw, id.ID); err != nil {
				t.Fatal(err)
			}
			if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, selector); err != nil {
				t.Fatal(err)
			}
			if err := BindManagedSDKModel(ctx, id.RunID, id.ID, "changed", "canonical-model"); err == nil {
				t.Fatal("requested selector changed")
			}
			if err := BindManagedSDKModel(ctx, id.RunID, id.ID, selector, "canonical-model"); err != nil {
				t.Fatal(err)
			}
			if err := BindManagedSDKModel(ctx, id.RunID, id.ID, selector, "other-model"); err == nil {
				t.Fatal("canonical model rebound")
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, attempt, err := readManagedSDKAttempt(ctx, tx, id.ID)
			tx.Rollback()
			if err != nil || !attempt.ModelBound || attempt.RequestedModel != selector || attempt.Model != "canonical-model" {
				t.Fatal(attempt, err)
			}
			if err := store.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, id.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var current UsageIdentity
			if err := json.Unmarshal(raw, &current); err != nil || current.Model != "canonical-model" || current.Finished {
				t.Fatal(current, err)
			}
		})
	}
}
