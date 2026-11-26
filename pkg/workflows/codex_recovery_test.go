package workflows

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestManagedRecoveryDiscoveryAdvancesPastHeldPage(t *testing.T) {
	store, original, identity := managedUsageFixture(t)
	witness := managedUsageWitness("sealed", 10)
	var originalBatch managedCodexBatchIdentity
	if err := json.Unmarshal(witness.Batch, &originalBatch); err != nil {
		t.Fatal(err)
	}
	originalBatch.CallIDs = []string{"original-call"}
	witness.Batch = mustManagedRecoveryJSON(originalBatch)
	if err := RecordManagedCodexUsage(original, identity.RunID, identity.ID, witness); err != nil {
		t.Fatal(err)
	}
	if err := SealManagedCodexBatch(original, identity.RunID, identity.ID, witness.Digest, witness.Batch); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	manager := &WorkerManager{Store: store}
	worker, err := manager.Get(ctx, identity.RunID, identity.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := manager.Messages(ctx, identity.RunID, identity.WorkerID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	worker.Status = RunStatusNeedsAttention
	workerRaw, _ := json.Marshal(worker)
	if _, err = store.db.Exec(`UPDATE workers SET status=?,record=? WHERE id=?`, worker.Status, workerRaw, worker.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := store.QueueResume(ctx, identity.RunID, "new-owner", true, 128)
	if err != nil {
		t.Fatal(err)
	}
	current.Status = RunStatusRunning
	if err = store.Claim(ctx, current, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	// Fifty held originals in each of two other origins respect the ordinary
	// per-run worker bound while filling the complete discovery page.
	for origin := range 2 {
		run := &Run{ID: fmt.Sprintf("held-origin-%d", origin), Status: RunStatusQueued, Jobs: map[string]JobExecution{}}
		if err = store.Admit(ctx, run, Snapshot{Version: 1}); err != nil {
			t.Fatal(err)
		}
		run.Status = RunStatusRunning
		if err = store.Claim(ctx, run, RunStatusQueued); err != nil {
			t.Fatal(err)
		}
		for index := range 50 {
			id := fmt.Sprintf("held-%d-%d", origin, index)
			held := Worker{ID: id, RunID: run.ID, Role: "held", Status: RunStatusNeedsAttention}
			heldRaw, _ := json.Marshal(held)
			if _, err = store.db.Exec(`INSERT INTO workers VALUES(?,?,?,?,?,?)`, id, run.ID, "", held.Role, held.Status, heldRaw); err != nil {
				t.Fatal(err)
			}
			msg := WorkerMessage{ID: id + "-message", WorkerID: id, Sequence: 1, Status: RunStatusRunning}
			msgRaw, _ := json.Marshal(msg)
			if _, err = store.db.Exec(`INSERT INTO messages VALUES(?,?,?,?,?,?)`, msg.ID, id, "held", 1, msg.Status, msgRaw); err != nil {
				t.Fatal(err)
			}
			usage := UsageIdentity{ID: msg.ID, RunID: run.ID, WorkerID: id, Kind: "worker", Provider: "claude-sdk", Model: "fixture"}
			if err = store.BeginUsage(ctx, usage); err != nil {
				t.Fatal(err)
			}
			if _, err = store.db.Exec(`INSERT INTO usage_observations VALUES(?,?,?)`, msg.ID, managedSDKAttemptKey, `{"scope":"invocation","managed_sdk_attempt":{"state":"terminal","profile":"corrupt"}}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Physical ordering is a fixture detail, not recovery authority.
	if _, err = store.db.Exec(`UPDATE workers SET rowid=10000 WHERE id=?`, worker.ID); err != nil {
		t.Fatal(err)
	}
	first, err := manager.managedCodexCandidates(ctx)
	if err != nil || len(first) != 0 || manager.recoveryAfter == 0 {
		t.Fatal("held first page did not advance discovery", first, err, manager.recoveryAfter)
	}
	cursor := manager.recoveryAfter
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = manager.managedCodexCandidates(canceled); err == nil || manager.recoveryAfter != cursor {
		t.Fatal("failed query changed discovery or gained authority", err, manager.recoveryAfter)
	}
	next, err := manager.managedCodexCandidates(ctx)
	if err != nil || len(next) != 1 || next[0].worker.ID != worker.ID || next[0].worker.Remaining != 0 || next[0].message.ID != messages[0].ID || !next[0].message.StartedAt.Equal(messages[0].StartedAt) || next[0].run.Generation != current.Generation || manager.recoveryAfter != 0 {
		t.Fatal("later original recovery hidden or changed", next, err, manager.recoveryAfter)
	}
	var heldCount int
	if err = store.db.QueryRow(`SELECT count(*) FROM workers WHERE role='held' AND status=?`, RunStatusNeedsAttention).Scan(&heldCount); err != nil || heldCount != 100 {
		t.Fatal("discovery regranted held originals", heldCount, err)
	}
	// Removing the only eligible recovery from discovery leaves an exactly full
	// held page followed by an empty page, which must wrap without new grants.
	if _, err = store.db.Exec(`UPDATE workers SET status=? WHERE id=?`, RunStatusSucceeded, worker.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := manager.managedCodexCandidates(ctx); err != nil || len(rows) != 0 || manager.recoveryAfter == 0 {
		t.Fatal("full held page did not advance after wrap", rows, err)
	}
	if rows, err := manager.managedCodexCandidates(ctx); err != nil || len(rows) != 0 || manager.recoveryAfter != 0 {
		t.Fatal("empty page did not wrap discovery", rows, err)
	}
}
