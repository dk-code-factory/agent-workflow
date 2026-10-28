package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// Commit Cancel after the accepted input transaction succeeds, with the owner
// context deliberately still healthy. The next effect must not be admitted.
func TestInputCancelAfterConsumedCommitPreventsNewEffects(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			ctx := context.Background()
			var effects atomic.Int32
			source := `jobs:
  decide:
    steps:
      - uses: input/wait
        with: {prompt: q, evidence-id: e}
      - uses: action/capture
        with: {value: forbidden-after-input}
  dependent: {needs: decide, steps: [{uses: action/capture, with: {value: forbidden-dependent}}]}
`
			if nested {
				source = `jobs:
  decide: {uses: workflows/child.yml}
  dependent: {needs: decide, steps: [{uses: action/capture, with: {value: forbidden-root}}]}
`
			}
			e, s := durableExecutor(t, source, nil)
			registerTestAction(t, e, readSpec(&effects))
			if nested {
				writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs:
  ask:
    steps:
      - uses: input/wait
        with: {prompt: q, evidence-id: e}
      - uses: action/capture
        with: {value: forbidden-child}
`)
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusWaiting {
				t.Fatal(run, err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			wrapper := &lifecycleConsumeJournal{SQLiteStore: s, afterConsume: func(g ExternalInputGate) {
				if err := s.RequestCancelGeneration(ctx, g.OriginID, run.Generation+1); err != nil {
					t.Error(err)
				}
			}}
			e.Store = wrapper
			done, err := e.Resume(ctx, run.ID, nil, false)
			if done == nil || done.Status != RunStatusCanceled || effects.Load() != 0 {
				t.Fatalf("effect admitted after committed Cancel: %+v %v calls=%d", done, err, effects.Load())
			}
			gate := inputGate(t, s, run.ID)
			if gate.State != "consumed" || gate.Answer == nil || len(gate.History) != 3 {
				t.Fatal("Cancel rewrote consumed history", gate)
			}
			attempts, err := s.ActionAttempts(ctx, run.ID)
			if err != nil || len(attempts) != 0 {
				t.Fatal("post-Cancel action admitted", attempts, err)
			}
		})
	}
}

func TestInputCancelBlocksStorageAdmissionsPreservesCompletion(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []string{"step", "child", "claim", "action", "invocation"} {
		t.Run(entry, func(t *testing.T) {
			s, run := executionStore(t)
			run.Status = RunStatusRunning
			if err := s.Save(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := s.RequestCancel(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			var err error
			switch entry {
			case "step":
				_, _, err = s.Begin(ctx, run.ID, "job", "new", 1, false)
			case "child":
				child := &Run{ID: randomID(), Status: RunStatusRunning, Version: 1}
				err = s.Admit(ctx, child, Snapshot{Version: 1, Metadata: map[string]any{"origin_run_id": run.ID}})
			case "claim":
				run.Status = RunStatusRunning
				err = s.Claim(ctx, run, RunStatusRunning)
			case "action":
				id := randomID()
				err = s.AdmitAction(ctx, ActionAttempt{State: "admitted", Revision: 1, Attempt: Attempt{ID: id, RunID: run.ID, JobID: "job", StepID: "new", Iteration: 1, Number: 1, Status: RunStatusRunning}, Context: ActionContext{AttemptID: id, RunID: run.ID}})
			case "invocation":
				err = s.AdmitInvocation(ctx, runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, State: "intent"})
			}
			if err == nil {
				t.Fatal("admission accepted durable Cancel", entry)
			}
		})
	}
	t.Run("completion", func(t *testing.T) {
		s, run := executionStore(t)
		run.Status = RunStatusRunning
		if err := s.Save(ctx, run); err != nil {
			t.Fatal(err)
		}
		attempt, _, err := s.Begin(ctx, run.ID, "job", "effect", 1, false)
		if err != nil {
			t.Fatal(err)
		}
		record := runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, State: "intent"}
		if err = s.AdmitInvocation(ctx, record); err != nil {
			t.Fatal(err)
		}
		if err = s.RequestCancel(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if err = s.Complete(ctx, attempt, StepExecution{ID: "effect", Status: RunStatusSucceeded, Outputs: map[string]any{"receipt": "completed-before-cancel"}}); err != nil {
			t.Fatal("Cancel blocked completed effect persistence", err)
		}
		record.State = "cleaned"
		if err = s.UpdateInvocation(ctx, record); err != nil {
			t.Fatal("Cancel blocked cleanup persistence", err)
		}
		completed, reused, reuseErr := s.Begin(ctx, run.ID, "job", "effect", 1, false)
		if reuseErr != nil || !reused || completed.ID != attempt.ID || completed.Status != RunStatusSucceeded {
			t.Fatal("Cancel blocked completed attempt reuse", completed, reused, reuseErr)
		}
		attempts, err := s.Attempts(ctx, run.ID)
		if err != nil || len(attempts) != 1 || attempts[0].Status != RunStatusSucceeded {
			t.Fatal(attempts, err)
		}
	})
}

func TestInputCancelBlocksWorkerAndToolAdmission(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []string{"spawn", "send", "turn", "resume"} {
		t.Run(entry, func(t *testing.T) {
			session := &fakeSession{}
			manager, run := workerFixture(t, session)
			worker, err := manager.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
			if err != nil {
				t.Fatal(err)
			}
			var pending WorkerMessage
			if entry == "turn" {
				pending, err = manager.Send(ctx, run, worker.ID, "queued-before-cancel", "must not dispatch")
				if err != nil {
					t.Fatal(err)
				}
			}
			if entry == "resume" {
				worker.Status = RunStatusNeedsAttention
				if err = manager.saveWorker(ctx, worker); err != nil {
					t.Fatal(err)
				}
			}
			if err = manager.Store.RequestCancel(ctx, run); err != nil {
				t.Fatal(err)
			}
			switch entry {
			case "spawn":
				_, err = manager.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
			case "send":
				_, err = manager.Send(ctx, run, worker.ID, "new", "forbidden")
			case "resume":
				err = manager.Resume(ctx, run, worker.ID)
			case "turn":
				manager.turn(ctx, worker, nil)
				messages, loadErr := manager.Messages(ctx, run, worker.ID)
				if loadErr != nil || len(messages) != 1 || messages[0].ID != pending.ID || messages[0].Status == RunStatusSucceeded || !messages[0].StartedAt.IsZero() {
					t.Fatal("queued worker dispatched after Cancel", messages, loadErr)
				}
				return
			}
			if err == nil {
				t.Fatal("worker admission accepted Cancel", entry)
			}
		})
	}
	t.Run("external tool", func(t *testing.T) {
		var calls atomic.Int32
		broker, binding, run := brokerFixture(t, func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`{"ok":true}`), nil
		})
		if err := broker.Store.RequestCancel(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		_, err := binding.Call(ctx, ToolRequest{CallID: "new", Server: "external", Tool: "lookup", Arguments: json.RawMessage(`{"query":"must not dispatch"}`)})
		if err == nil || calls.Load() != 0 {
			t.Fatal("tool admitted after Cancel", err, calls.Load())
		}
	})
}

func TestInputCancelCompletedActionCheckpointSurvives(t *testing.T) {
	var effects atomic.Int32
	e, s := durableExecutor(t, actionSource, nil)
	spec := readSpec(&effects)
	spec.Handler = func(ctx context.Context, ac ActionContext, input actionInput) (ActionResult[actionOutput], error) {
		effects.Add(1)
		if err := s.RequestCancel(ctx, ac.RunID); err != nil {
			return ActionResult[actionOutput]{}, err
		}
		return ActionResult[actionOutput]{Output: actionOutput{input.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}, nil
	}
	registerTestAction(t, e, spec)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run == nil || run.Status != RunStatusCanceled || effects.Load() != 1 {
		t.Fatal(run, err, effects.Load())
	}
	records, err := s.ActionAttempts(context.Background(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "completed" || records[0].Attempt.Status != RunStatusSucceeded {
		t.Fatal("Cancel discarded completed action", records, err)
	}
}

func TestInputLifecycleCallbackAcceptanceFailureReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_answer BEFORE UPDATE OF state ON input_gates WHEN NEW.state='accepted' BEGIN SELECT RAISE(ABORT,'answer acceptance fault'); END`); err != nil {
		t.Fatal(err)
	}
	e.Store = &executionBoundaryJournal{SQLiteStore: s, afterGate: func(ctx context.Context, g ExternalInputGate) error {
		_, err := s.AcceptInput(ctx, g.OriginID, g.ID, ExternalInputAnswer{AnswerID: "fault", EvidenceID: g.EvidenceID, Value: json.RawMessage(`true`)})
		return err
	}}
	_, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "answer acceptance fault") {
		t.Fatal("callback acceptance failure did not return", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("callback relied on deadline", err)
	}
}

type cancelBeforeBeginJournal struct {
	*SQLiteStore
	origin string
}

func (s *cancelBeforeBeginJournal) Begin(ctx context.Context, run, job, step string, iteration int, retry bool) (Attempt, bool, error) {
	if err := s.RequestCancel(ctx, s.origin); err != nil {
		return Attempt{}, false, err
	}
	return s.SQLiteStore.Begin(ctx, run, job, step, iteration, retry)
}

type cancelBeforeChildJournal struct {
	*SQLiteStore
	origin string
}

func (s *cancelBeforeChildJournal) Admit(ctx context.Context, run *Run, snapshot Snapshot) error {
	if nativeString(snapshot.Metadata, "origin_run_id") == s.origin && run.ID != s.origin {
		if err := s.RequestCancel(ctx, s.origin); err != nil {
			return err
		}
	}
	return s.SQLiteStore.Admit(ctx, run, snapshot)
}

func TestInputCancelLateAdmissionBoundaries(t *testing.T) {
	for _, boundary := range []string{"step", "action binding", "new child"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			var effects atomic.Int32
			source := `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}, {uses: agent/a, with: {prompt: forbidden}}]}}`
			if boundary == "action binding" {
				source = `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}, {uses: action/capture, with: {value: forbidden}}]}}`
			}
			if boundary == "new child" {
				source = `jobs:
  ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}
  child: {needs: ask, uses: workflows/child.yml}
`
			}
			e, s := durableExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				effects.Add(1)
				return map[string]any{}, nil
			}))
			if boundary == "new child" {
				writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {effect: {steps: [{uses: agent/a, with: {prompt: forbidden}}]}}`)
			}
			spec := readSpec(&effects)
			var origin string
			spec.Bindings = func(ctx context.Context, _ ActionContext, _ actionInput) (json.RawMessage, error) {
				if origin != "" {
					if err := s.RequestCancel(ctx, origin); err != nil {
						return nil, err
					}
				}
				return json.RawMessage(`{"repository":"fixture"}`), nil
			}
			if boundary == "action binding" {
				registerTestAction(t, e, spec)
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			origin = run.ID
			answerGate(t, s, inputGate(t, s, run.ID))
			switch boundary {
			case "step":
				e.Store = &cancelBeforeBeginJournal{SQLiteStore: s, origin: run.ID}
			case "new child":
				e.Store = &cancelBeforeChildJournal{SQLiteStore: s, origin: run.ID}
			}
			result, err := e.Resume(ctx, run.ID, nil, false)
			if result == nil || result.Status != RunStatusCanceled || effects.Load() != 0 || err != nil {
				t.Fatal("late Cancel admission escalated/dispatched", result, err, effects.Load())
			}
			records, err := s.Attempts(ctx, run.ID)
			if err != nil || len(records) != 0 {
				t.Fatal("new effect admitted", records, err)
			}
		})
	}
}

// Static embedding deliberately hides SQLite's private admission helpers. The
// executor must honor only the complete declared journal's durable controls.
type declaredCancelJournal struct {
	ExternalInputJournal
	canceled atomic.Bool
}

func (j *declaredCancelJournal) InputControl(ctx context.Context, id string) (ExternalInputControl, error) {
	control, err := j.ExternalInputJournal.InputControl(ctx, id)
	if j.canceled.Load() {
		control.Canceled = true
	}
	return control, err
}
func (j *declaredCancelJournal) ConsumeInput(ctx context.Context, id string) (ExternalInputGate, error) {
	gate, err := j.ExternalInputJournal.ConsumeInput(ctx, id)
	if err == nil {
		j.canceled.Store(true)
	}
	return gate, err
}
func TestInputCancelDeclaredJournalPreDispatch(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}, {uses: agent/a, with: {prompt: forbidden}}]}}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{}, nil
	}))
	journal := &declaredCancelJournal{ExternalInputJournal: s}
	e.Store = journal
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	answerGate(t, s, inputGate(t, s, run.ID))
	result, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || result == nil || result.Status != RunStatusCanceled || calls.Load() != 0 {
		t.Fatal("declared journal Cancel missed", result, err, calls.Load())
	}
	if inputGate(t, s, run.ID).State != "consumed" {
		t.Fatal("declared journal lost consumed checkpoint")
	}
}

func TestInputCancelAdmittedToolCompletionAndReuseSurvives(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	var store *SQLiteStore
	var runID string
	broker, binding, run := brokerFixture(t, func(ctx context.Context, _ ToolDefinition, _ json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		if err := store.RequestCancel(ctx, runID); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"receipt":"done"}`), nil
	})
	store = broker.Store
	runID = run.ID
	request := ToolRequest{CallID: "before-cancel", Server: "external", Tool: "lookup", Arguments: json.RawMessage(`{"query":"admitted"}`)}
	first, err := binding.Call(ctx, request)
	if err != nil {
		t.Fatal("Cancel blocked admitted tool result", err)
	}
	second, err := binding.Call(ctx, request)
	if err != nil || string(first) != string(second) || calls.Load() != 1 {
		t.Fatal("Cancel blocked completed result reuse/replayed", string(second), err, calls.Load())
	}
	records, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(records) != 1 || records[0].Status != "completed" {
		t.Fatal("completion not persisted", records, err)
	}
}

type cancelBeforeActionJournal struct {
	*SQLiteStore
	origin string
}

func (s *cancelBeforeActionJournal) AdmitAction(ctx context.Context, record ActionAttempt) error {
	if err := s.RequestCancel(ctx, s.origin); err != nil {
		return err
	}
	return s.SQLiteStore.AdmitAction(ctx, record)
}

type cancelOnAdmittedStepJournal struct {
	*SQLiteStore
	origin string
}

func (s *cancelOnAdmittedStepJournal) Begin(ctx context.Context, run, job, step string, iteration int, retry bool) (Attempt, bool, error) {
	attempt, reused, err := s.SQLiteStore.Begin(ctx, run, job, step, iteration, retry)
	if err == nil {
		err = s.RequestCancel(ctx, s.origin)
	}
	return attempt, reused, err
}

func TestInputCancelLateTransactionalActionAndStepPublication(t *testing.T) {
	for _, kind := range []string{"action admission", "admitted step"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			source := `jobs: {work: {steps: [{uses: agent/a, with: {prompt: forbidden}}]}}`
			if kind == "action admission" {
				source = actionSource
			}
			e, s := durableExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			if kind == "action admission" {
				registerTestAction(t, e, readSpec(&calls))
			}
			run, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "action admission" {
				e.Store = &cancelBeforeActionJournal{SQLiteStore: s, origin: run.ID}
			} else {
				e.Store = &cancelOnAdmittedStepJournal{SQLiteStore: s, origin: run.ID}
			}
			result, err := e.Resume(context.Background(), run.ID, nil, false)
			if err != nil || result == nil || result.Status != RunStatusCanceled || calls.Load() != 0 {
				t.Fatal("late pure denial escalated/dispatched", result, err, calls.Load())
			}
			attempts, err := s.Attempts(context.Background(), run.ID)
			want := 0
			if kind == "admitted step" {
				want = 1
			}
			if err != nil || len(attempts) != want {
				t.Fatal(attempts, err)
			}
			if kind == "admitted step" && attempts[0].Status == RunStatusRunning {
				t.Fatal("already admitted no-effect step not checkpointed", attempts)
			}
		})
	}
}

type cancelDuringTurnSession struct {
	*fakeSession
	cancel func() error
}

func (s *cancelDuringTurnSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	if err := s.cancel(); err != nil {
		return SessionResult{}, err
	}
	return s.fakeSession.RunTurn(ctx, req, emit)
}
func TestInputCancelAdmittedWorkerResultPersistenceSurvives(t *testing.T) {
	ctx := context.Background()
	base := &fakeSession{}
	manager, run := workerFixture(t, base)
	manager.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return &cancelDuringTurnSession{fakeSession: base, cancel: func() error { return manager.Store.RequestCancel(ctx, run) }}, []string{"Read", "Bash"}, nil, nil
	}
	worker, err := manager.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.Send(ctx, run, worker.ID, "before-cancel", "completed effect")
	if err != nil {
		t.Fatal(err)
	}
	manager.turn(ctx, worker, nil)
	messages, err := manager.Messages(ctx, run, worker.ID)
	if err != nil || len(messages) != 1 || messages[0].ID != message.ID || messages[0].Status != RunStatusSucceeded || messages[0].Result.Outputs["text"] != "completed effect" {
		t.Fatal("Cancel lost admitted worker result", messages, err)
	}
	if _, err = manager.Send(ctx, run, worker.ID, "after-cancel", "forbidden"); err == nil {
		t.Fatal("later worker admission escaped Cancel")
	}
}

func TestInputCancelDuringWorkerFactoryFencesTransaction(t *testing.T) {
	for _, entry := range []string{"spawn", "send", "turn"} {
		t.Run(entry, func(t *testing.T) {
			ctx := context.Background()
			base := &fakeSession{}
			manager, run := workerFixture(t, base)
			worker, err := manager.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
			if err != nil {
				t.Fatal(err)
			}
			if entry == "turn" {
				if _, err = manager.Send(ctx, run, worker.ID, "prior", "forbidden"); err != nil {
					t.Fatal(err)
				}
			}
			// Spawn's preliminary owner check has passed before Factory is called.
			// Cancellation here must still fence the transaction that admits work.
			manager.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				if err := manager.Store.RequestCancel(ctx, run); err != nil {
					return nil, nil, nil, err
				}
				return base, []string{"Read", "Bash"}, nil, nil
			}
			switch entry {
			case "spawn":
				_, err = manager.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
			case "send":
				_, err = manager.Send(ctx, run, worker.ID, "new", "forbidden")
			case "turn":
				manager.turn(ctx, worker, nil)
				messages, loadErr := manager.Messages(ctx, run, worker.ID)
				if loadErr != nil || len(messages) != 1 || !messages[0].StartedAt.IsZero() {
					t.Fatal("late Cancel dispatched worker", messages, loadErr)
				}
				return
			}
			if !errors.Is(err, ErrExternalInputCanceled) {
				t.Fatal("transaction missed late durable Cancel", entry, err)
			}
		})
	}
}

func TestInputCancelStableWorkerAndMessageReceiptsRemainReadable(t *testing.T) {
	ctx := context.Background()
	manager, run := workerFixture(t, &fakeSession{})
	spec := WorkerSpec{RunID: run, Role: "reviewer", StableID: randomID()}
	worker, err := manager.Spawn(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.Send(ctx, run, worker.ID, "same", "already admitted")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Store.RequestCancel(ctx, run); err != nil {
		t.Fatal(err)
	}
	repeated, err := manager.Spawn(ctx, spec)
	if err != nil || repeated.ID != worker.ID {
		t.Fatal("Cancel lost stable worker receipt", repeated, err)
	}
	repeatedMessage, err := manager.Send(ctx, run, worker.ID, "same", "already admitted")
	if err != nil || repeatedMessage.ID != message.ID {
		t.Fatal("Cancel lost prior message receipt", repeatedMessage, err)
	}
	if _, err = manager.Send(ctx, run, worker.ID, "new", "forbidden"); !errors.Is(err, ErrExternalInputCanceled) {
		t.Fatal("receipt reuse reopened admission", err)
	}
	changed := spec
	changed.StableID = randomID()
	if _, err = manager.Spawn(ctx, changed); !errors.Is(err, ErrExternalInputCanceled) {
		t.Fatal("changed worker identity reopened admission", err)
	}
	changed = spec
	changed.ParentID = "other-parent"
	if _, err = manager.Spawn(ctx, changed); err == nil {
		t.Fatal("stable receipt crossed parent identity")
	}
	changed = spec
	changed.CreatorInvocation = "other-creator"
	if _, err = manager.Spawn(ctx, changed); err == nil {
		t.Fatal("stable receipt crossed creator identity")
	}
	changed = spec
	changed.Owner = "stale-owner"
	if _, err = manager.Spawn(ctx, changed); err == nil {
		t.Fatal("stable receipt crossed owner generation")
	}
}
