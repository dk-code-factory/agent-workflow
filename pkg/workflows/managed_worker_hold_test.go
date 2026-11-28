package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

type managedHoldSession struct {
	fakeSession
	failFirst bool
	calls     atomic.Int32
}

func (s *managedHoldSession) AdmitNativeTask(d *AgentDefinition) (*AgentDefinition, error) {
	copy := *d
	return &copy, nil
}

func (s *managedHoldSession) UsageProvider(string, string) (string, string) {
	return "codex-app-server", "fixture"
}

func (s *managedHoldSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	s.calls.Add(1)
	var result SessionResult
	if err := BeginManagedCodexInvocation(ctx, req.RunID, req.InvocationID, "build", "policy", "fixture"); err != nil {
		return result, err
	}
	result = SessionResult{SessionID: "original-session", Status: RunStatusSucceeded, Outputs: map[string]any{"text": "original output"}}
	emit(ProviderEvent{Type: "session", SessionID: result.SessionID})
	if err := BindManagedCodexInvocation(ctx, req.RunID, req.InvocationID, result.SessionID, "turn"); err != nil {
		return result, err
	}
	invocation, err := runtimeexec.Admit(ctx, "native")
	if err != nil {
		return result, err
	}
	if err = invocation.Save(ctx, "cleaned"); err != nil {
		return result, err
	}
	if s.failFirst && req.Prompt == "first" {
		return result, ErrProviderFailure
	}
	w := managedUsageWitness("final", 10)
	w.SessionID, w.TurnID = result.SessionID, "turn"
	w.Batch = mustManagedRecoveryJSON(managedCodexBatchIdentity{ThreadID: w.SessionID, TurnID: w.TurnID, InvocationID: req.InvocationID, PolicyID: w.PolicyID, BuildID: w.BuildID, BatchID: "batch-final", ResponseID: w.ResponseID, CallIDs: []string{}})
	if err = RecordManagedCodexUsage(ctx, req.RunID, req.InvocationID, w); err != nil {
		return result, err
	}
	result.ProviderStatus, result.ProviderVersion = "completed", "agent-workflow-managed-v1/build"
	result.Outputs["usage"], result.Outputs["usage_incomplete"] = w.Usage, false
	return result, CheckpointManagedCodexTerminal(ctx, result, w.ResponseID, w.Digest)
}

func managedHoldServe(t *testing.T, m *WorkerManager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Serve(ctx) }()
	m.notify()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("managed hold scheduler did not join")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestManagedCodexHeldFailureBlocksPublicTaskContinuation(t *testing.T) {
	for _, failed := range []bool{true, false} {
		t.Run(fmt.Sprint("held=", failed), func(t *testing.T) {
			e, store, _ := agentTaskFixture(t, `jobs:
  native:
    steps:
      - id: first
        uses: agent/run
        continue-on-error: true
        with: {agent: reviewer.yaml, prompt: first, turns: 3}
      - id: successor
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: second}
`, nil)
			session := &managedHoldSession{failFirst: failed}
			e.Workers.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return session, nil, nil, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if !failed {
				if err != nil || run.Status != RunStatusSucceeded || session.calls.Load() != 2 {
					t.Fatalf("completed originals did not allow continuation: %+v calls=%d err=%v", run, session.calls.Load(), err)
				}
				return
			}
			workers, loadErr := e.Workers.List(ctx, run.ID)
			if loadErr != nil || len(workers) != 1 {
				t.Fatal(workers, loadErr)
			}
			messages, loadErr := e.Workers.Messages(ctx, run.ID, workers[0].ID)
			if loadErr != nil || len(messages) == 0 {
				t.Fatal(messages, loadErr)
			}
			if err == nil || session.calls.Load() != 1 || len(messages) != 1 || messages[0].FailureAcknowledgement != nil {
				t.Fatalf("held original was handled or dispatched successor: run=%+v messages=%+v calls=%d err=%v", run, messages, session.calls.Load(), err)
			}
			var state string
			if err = store.db.QueryRow(`SELECT json_extract(record,'$.managed_codex_admission.state') FROM usage_observations WHERE invocation_id=? AND observation_key=?`, messages[0].ID, managedCodexAdmissionKey).Scan(&state); err != nil || state != "held" {
				t.Fatal("original hold lost", state, err)
			}
		})
	}
}

func TestManagedCodexHeldWorkerSurvivesRestartAndBlocksDisposition(t *testing.T) {
	for _, operation := range []string{"acknowledge", "observed-ack-queued", "send", "reopen", "resume", "retained-idle-queued", "retained-ack-finalize", "completed"} {
		t.Run(operation, func(t *testing.T) {
			m, run := workerFixture(t, &fakeSession{})
			owner, _, err := m.Store.Load(t.Context(), run)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Store.Claim(t.Context(), owner, RunStatusRunning); err != nil {
				t.Fatal(err)
			}
			session := &managedHoldSession{failFirst: operation != "completed"}
			factory := func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return session, nil, nil, nil
			}
			m.Factory = factory
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			w, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer", Turns: 3, Definition: &AgentDefinition{Name: "reviewer", DeveloperInstructions: "Review"}})
			if err != nil {
				t.Fatal(err)
			}
			original, err := m.Send(ctx, run, w.ID, "original", "first")
			if err != nil {
				t.Fatal(err)
			}
			var queued WorkerMessage
			if operation == "observed-ack-queued" || operation == "retained-idle-queued" || operation == "completed" {
				queued, err = m.Send(ctx, run, w.ID, "queued", "second")
				if err != nil {
					t.Fatal(err)
				}
			}
			stop := managedHoldServe(t, m)
			wait := WorkerWaitRequest{RunID: run, WorkerID: w.ID, MessageID: original.ID, OperationID: "original-wait", Deadline: time.Now().Add(4 * time.Second)}
			observed, err := m.WaitMessage(ctx, wait)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "completed" {
				wait.MessageID, wait.OperationID = queued.ID, "queued-wait"
				next, err := m.WaitMessage(ctx, wait)
				if err != nil || observed.Status != RunStatusSucceeded || next.Status != RunStatusSucceeded || session.calls.Load() != 2 {
					t.Fatal("completed native originals blocked successor", observed, next, err)
				}
				if err = m.StopRun(ctx, run); err != nil {
					t.Fatal("completed native evidence blocked finalization", err)
				}
				return
			}
			stop()
			if observed.Status != RunStatusFailed || observed.FailureClass != "provider_failure" {
				t.Fatal("wrong original failure", observed)
			}
			// Earlier writers retained this ordinary failure without an unsafe class.
			// Its durable native marker must hold even those retained outcomes.
			observed.UnsafeFailureClass = ""
			raw, err := json.Marshal(observed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Store.db.Exec(`UPDATE messages SET record=? WHERE id=?`, raw, observed.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = m.Store.db.Exec(`UPDATE worker_waits SET record=json_remove(record,'$.result.unsafe_failure_class') WHERE run_id=? AND operation_id=?`, run, wait.OperationID); err != nil {
				t.Fatal(err)
			}
			var markerBefore, usageBefore []byte
			if err = m.Store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, observed.ID, managedCodexAdmissionKey).Scan(&markerBefore); err != nil {
				t.Fatal(err)
			}
			if err = m.Store.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, observed.ID).Scan(&usageBefore); err != nil {
				t.Fatal(err)
			}
			before, err := m.Get(ctx, run, w.ID)
			if err != nil || before.Remaining != 2 || before.SessionID != "original-session" {
				t.Fatal(before, err)
			}
			dir := m.Store.Dir
			if err = m.Store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			m = &WorkerManager{Store: reopened, Slots: make(chan struct{}, 1), Factory: factory}
			if err = m.Init(ctx); err != nil {
				t.Fatal(err)
			}
			owner, _, err = reopened.Load(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			request := WorkerFailureRequest{RunID: run, WorkerID: w.ID, MessageID: original.ID, Owner: owner.Owner, Generation: owner.Generation, RequestID: "handled", Reason: "ordinary error observed", Evidence: "original result"}
			switch operation {
			case "acknowledge":
				_, err = m.AcknowledgeFailure(ctx, request)
			case "observed-ack-queued":
				_, err = m.acknowledgeFailure(ctx, request, wait.OperationID)
			case "send":
				_, err = m.Send(ctx, run, w.ID, "new", "second")
			case "reopen":
				err = m.reopenAgentTask(ctx, run, w.ID, "reviewer")
			case "resume":
				err = m.Resume(ctx, run, w.ID)
			case "retained-ack-finalize":
				// Retain the exact ordinary observation receipt an earlier writer
				// could acknowledge while this native marker was already held.
				ack := &WorkerFailureAcknowledgement{RequestID: actionHash([]any{observed.RequestID, "task-result-observed"}), MessageID: observed.ID, Generation: observed.Generation, Reason: "pipeline observed the durable task error", Evidence: actionHash(observed.Result), AcknowledgedAt: time.Now().UTC()}
				observed.FailureAcknowledgement, before.FailureAcknowledgement = ack, ack
				messageRaw, marshalErr := json.Marshal(observed)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				workerRaw, marshalErr := json.Marshal(before)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, err = reopened.db.Exec(`UPDATE messages SET record=? WHERE id=?`, messageRaw, observed.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = reopened.db.Exec(`UPDATE workers SET record=? WHERE id=?`, workerRaw, before.ID); err != nil {
					t.Fatal(err)
				}
				err = m.StopRun(ctx, run)
			case "retained-idle-queued":
				// An old acknowledgement could leave this retained worker idle.
				before.Status = "idle"
				raw, marshalErr := json.Marshal(before)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, err = reopened.db.Exec(`UPDATE workers SET status='idle',record=? WHERE id=?`, raw, w.ID); err != nil {
					t.Fatal(err)
				}
				// Drive ordinary queued admission synchronously after reopening. A
				// scheduler timeout would not prove this durable guard was reached.
				pool := &Capacity{Slots: m.Slots}
				lease, acquireErr := pool.Acquire(ctx)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				m.turn(ctx, before, lease)
				lease.Release()
				current, loadErr := m.Get(ctx, run, w.ID)
				if loadErr != nil || current.Status != RunStatusNeedsAttention || !strings.Contains(current.Error, "original managed native attempt") {
					t.Fatal("ordinary queued admission did not retain the hold", current, loadErr)
				}
				var queuedStatus string
				if loadErr = reopened.db.QueryRow(`SELECT status FROM messages WHERE id=?`, queued.ID).Scan(&queuedStatus); loadErr != nil || queuedStatus != "queued" {
					t.Fatal("held queued message was admitted", queuedStatus, loadErr)
				}
				err = ErrActionIntegrity
			}
			if err == nil {
				t.Fatal("held native original allowed", operation)
			}
			var markerAfter, usageAfter []byte
			if readErr := reopened.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, observed.ID, managedCodexAdmissionKey).Scan(&markerAfter); readErr != nil {
				t.Fatal(readErr)
			}
			if readErr := reopened.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, observed.ID).Scan(&usageAfter); readErr != nil {
				t.Fatal(readErr)
			}
			currentOwner, _, ownerErr := reopened.Load(ctx, run)
			if ownerErr != nil || currentOwner.Owner != owner.Owner || currentOwner.Generation != owner.Generation || string(markerAfter) != string(markerBefore) || string(usageAfter) != string(usageBefore) {
				t.Fatal("hold changed native admission, usage or execution owner", ownerErr)
			}
			after, loadErr := m.Get(ctx, run, w.ID)
			messages, messageErr := m.Messages(ctx, run, w.ID)
			if loadErr != nil || messageErr != nil || after.Remaining != before.Remaining || after.SessionID != before.SessionID || session.calls.Load() != 1 || (messages[0].FailureAcknowledgement != nil && operation != "retained-ack-finalize") || !messages[0].StartedAt.Equal(observed.StartedAt) || messages[0].Generation != observed.Generation {
				t.Fatal("hold changed original authority or allowance", before, after, messages, loadErr, messageErr)
			}
		})
	}
}
