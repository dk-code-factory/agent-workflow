package workflows

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestManagedCodexSealPreservesOriginalProofAndUnknownExtensions(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	w := managedUsageWitness("sealed", 10)
	var batch managedCodexBatchIdentity
	if err := json.Unmarshal(w.Batch, &batch); err != nil {
		t.Fatal(err)
	}
	batch.CallIDs = []string{"a", "b"}
	w.Batch = mustManagedRecoveryJSON(batch)
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='codex-response:sealed'`, id.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	fields["future_engine_proof"] = json.RawMessage(`{"retained":true}`)
	raw, _ = json.Marshal(fields)
	if _, err := store.db.Exec(`UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key='codex-response:sealed'`, raw, id.ID); err != nil {
		t.Fatal(err)
	}
	if err := SealManagedCodexBatch(ctx, id.RunID, id.ID, w.Digest, w.Batch); err != nil {
		t.Fatal(err)
	}
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
		t.Fatal("same original usage lost after seal", err)
	}
	if err := store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='codex-response:sealed'`, id.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"future_engine_proof":{"retained":true}`) {
		t.Fatal("unknown proof extension overwritten", string(raw))
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := managedRecoveryForInvocation(ctx, tx, id.ID)
	tx.Rollback()
	if err != nil || r == nil || r.State != "sealed" || r.Admission == nil || r.Admission.RemainingCeiling != 0 {
		t.Fatal(r, err)
	}
	batch.CallIDs = []string{"a"}
	if err := SealManagedCodexBatch(ctx, id.RunID, id.ID, w.Digest, mustManagedRecoveryJSON(batch)); err == nil {
		t.Fatal("omitted original call accepted")
	}
	if err := store.ObserveUsage(ctx, id, UsageUpdate{Key: "unknown-attempt", Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = managedRecoveryForInvocation(ctx, tx, id.ID)
	tx.Rollback()
	if err == nil {
		t.Fatal("unknown earlier observation ignored by recovery")
	}
}
func TestManagedCodexBookkeepingCannotHideChargesOrRestartUnknownAttempt(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	if err := BeginManagedCodexInvocation(ctx, id.RunID, id.ID, "build", "policy", "fixture"); err == nil {
		t.Fatal("a second fresh provider attempt inherited original admission")
	}
	w := managedUsageWitness("final", 10)
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, id.ID, managedCodexAdmissionKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	fields["input_tokens"] = 1
	raw, _ = json.Marshal(fields)
	if _, err := store.db.Exec(`UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, id.ID, managedCodexAdmissionKey); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, w.ResponseID, w.Digest); err == nil {
		t.Fatal("charged row accepted as no-token bookkeeping")
	}
}
func TestManagedCodexTerminalCheckpointKeepsResultAndChargesAtomic(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	w := managedUsageWitness("final", 10)
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
		t.Fatal(err)
	}
	var workerRaw, messageRaw []byte
	store.db.QueryRow(`SELECT record FROM workers WHERE id=?`, id.WorkerID).Scan(&workerRaw)
	store.db.QueryRow(`SELECT record FROM messages WHERE id=?`, id.ID).Scan(&messageRaw)
	var worker Worker
	var message WorkerMessage
	json.Unmarshal(workerRaw, &worker)
	json.Unmarshal(messageRaw, &message)
	manager := &WorkerManager{Store: store, Limits: DefaultWorkerLimits()}
	ctx = context.WithValue(ctx, managedCodexTerminalKey{}, manager.managedTerminalRecorder(worker, message, map[string]string{"credential": "PRIVATE"}))
	result := SessionResult{SessionID: "session", Status: RunStatusSucceeded, ProviderStatus: "completed", ProviderVersion: "agent-workflow-managed-v1/build", Outputs: map[string]any{"text": "PRIVATE", "usage": map[string]any{"input_tokens": int64(10), "cached_input_tokens": int64(1), "output_tokens": int64(2)}, "usage_incomplete": false}}
	if err := CheckpointManagedCodexTerminal(ctx, result, w.ResponseID, w.Digest); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := managedRecoveryForInvocation(ctx, tx, id.ID)
	tx.Rollback()
	if err != nil || saved == nil || saved.Terminal == nil || saved.Terminal.Outputs["text"] == "PRIVATE" {
		t.Fatal("terminal checkpoint lost redaction", saved, err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = finishManagedCodexCheckpoint(ctx, tx, id, result); err == nil {
		t.Fatal("altered original terminal result accepted")
	}
	tx.Rollback()
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = finishManagedCodexCheckpoint(ctx, tx, id, *saved.Terminal); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	report, err := store.Usage(ctx, id.RunID)
	if err != nil || report.Invocations[0].Finished {
		t.Fatal("failed completion commit finished usage", report, err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = finishManagedCodexCheckpoint(ctx, tx, id, *saved.Terminal); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	report, err = store.Usage(ctx, id.RunID)
	if err != nil || !report.Summary.Complete || *report.Summary.InputTokens != 10 {
		t.Fatal(report, err)
	}
}

func TestManagedRecoveryHeldCandidateDoesNotStarveUnrelatedWorker(t *testing.T) {
	store, ctx, id := sdkAttemptFixture(t)
	// This malformed original is a recovery candidate, never an authority.
	var raw []byte
	if err := store.db.QueryRow(`SELECT record FROM workers WHERE id=?`, id.WorkerID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var held Worker
	json.Unmarshal(raw, &held)
	held.Status = RunStatusNeedsAttention
	raw, _ = json.Marshal(held)
	if _, err := store.db.Exec(`UPDATE workers SET status=?,record=? WHERE id=?`, held.Status, raw, held.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO usage_observations VALUES(?,?,?)`, id.ID, managedSDKAttemptKey, `{"scope":"invocation","managed_sdk_attempt":{"state":"terminal","profile":"corrupt"}}`); err != nil {
		t.Fatal(err)
	}
	other := &Run{ID: "independent-run", Status: RunStatusQueued, Jobs: map[string]JobExecution{}}
	if err := store.Admit(ctx, other, Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	other.Status = RunStatusRunning
	if err := store.Claim(context.Background(), other, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	manager := &WorkerManager{Store: store, Limits: DefaultWorkerLimits(), Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return &fakeSession{}, nil, nil, nil
	}}
	if err := manager.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	worker, err := manager.Spawn(context.Background(), WorkerSpec{RunID: other.ID, Role: "ordinary", Turns: 1})
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.Send(context.Background(), other.ID, worker.ID, "ordinary", "independent work")
	if err != nil {
		t.Fatal(err)
	}
	service, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); manager.Serve(service) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("scheduler did not join")
		}
	}()
	for service.Err() == nil {
		messages, e := manager.Messages(service, other.ID, worker.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(messages) == 1 && messages[0].ID == message.ID && messages[0].Status == RunStatusSucceeded {
			heldNow, e := manager.Get(service, id.RunID, id.WorkerID)
			if e != nil || heldNow.Status != RunStatusNeedsAttention || heldNow.Remaining != 0 {
				t.Fatal("held original gained authority", heldNow, e)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("held recovery candidate starved independent worker")
}
