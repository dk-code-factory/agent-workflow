package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Static embedding exposes exactly the advertised optional contract, without
// accidentally promoting SQLite's other recovery or controller methods.
type inputContractStore struct{ ExternalInputJournal }
type inputContractLegacyStore struct{ Journal }

// This is the draft gate-only contract, deliberately missing admission.
type inputContractGateJournal interface {
	Journal
	EnsureInputGate(context.Context, ExternalInputGate) (ExternalInputGate, error)
	Inputs(context.Context, string) ([]ExternalInputGate, error)
	AcceptInput(context.Context, string, string, ExternalInputAnswer) (ExternalInputGate, error)
	ConsumeInput(context.Context, string) (ExternalInputGate, error)
	InputControl(context.Context, string) (ExternalInputControl, error)
}
type inputContractGateStore struct{ inputContractGateJournal }

type inputContractAdmissionJournal interface {
	Journal
	QueueResume(context.Context, string, string, bool, int) (*Run, error)
}
type inputContractAdmissionStore struct{ inputContractAdmissionJournal }

func TestExternalInputContractAcceptedAnswerResumes(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "unpaused"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, sqlite := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			store := inputContractStore{ExternalInputJournal: sqlite}
			e.Store = store
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusWaiting || calls.Load() != 1 {
				t.Fatalf("initial wait %+v err=%v effects=%d", run, err, calls.Load())
			}
			gate := inputGate(t, sqlite, run.ID)
			attempts, err := sqlite.Attempts(ctx, run.ID)
			if err != nil || len(attempts) != 1 || attempts[0].StepID != "before" || attempts[0].Status != RunStatusSucceeded {
				t.Fatal("missing preparation checkpoint", attempts, err)
			}
			checkpoint := attempts[0].ID
			pending, err := e.Resume(ctx, run.ID, nil, false)
			if err != nil || pending.Status != RunStatusWaiting || pending.Generation != run.Generation || calls.Load() != 1 || !reflect.DeepEqual(inputGate(t, sqlite, run.ID), gate) {
				t.Fatal("unanswered resume changed gate, generation or effects", pending, err)
			}
			if paused {
				if err = sqlite.InterruptInput(ctx, run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			}
			answer := ExternalInputAnswer{AnswerID: "contract-answer", EvidenceID: gate.EvidenceID, Value: json.RawMessage(`"approved"`)}
			accepted, err := store.AcceptInput(ctx, run.ID, gate.ID, answer)
			if err != nil || accepted.State != "accepted" {
				t.Fatal(accepted, err)
			}
			if paused {
				if _, err = store.ConsumeInput(ctx, gate.ID); !errors.Is(err, ErrExternalInput) {
					t.Fatal("paused answer consumed", err)
				}
				if err = sqlite.QueueInputContinuations(ctx, 128); err != nil {
					t.Fatal(err)
				}
				current, _, err := sqlite.Load(ctx, run.ID)
				control, controlErr := store.InputControl(ctx, run.ID)
				if err != nil || controlErr != nil || current.Status != RunStatusInterrupted || current.Generation != run.Generation || !control.Paused || !reflect.DeepEqual(inputGate(t, sqlite, run.ID), accepted) {
					t.Fatal("automatic continuation bypassed pause", current, control, err, controlErr)
				}
			}
			// No manual SQLite admission: direct embedded Resume must use the
			// method promoted by the complete optional interface.
			done, err := e.Resume(ctx, run.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || done.Generation != run.Generation+1 || calls.Load() != 3 || done.Jobs["decide"].Outputs["value"] != "approved" {
				t.Fatalf("accepted answer stranded or replayed: %+v err=%v effects=%d", done, err, calls.Load())
			}
			if _, ok := any(store).(inputContractAdmissionJournal); !ok {
				t.Fatal("complete ExternalInputJournal does not promote required admission")
			}
			consumed := inputGate(t, sqlite, run.ID)
			if consumed.ID != gate.ID || consumed.Fingerprint != gate.Fingerprint || consumed.Answer == nil || !reflect.DeepEqual(*consumed.Answer, answer) || len(consumed.History) != 3 || consumed.History[0].State != "pending" || consumed.History[1].State != "accepted" || consumed.History[2].State != "consumed" || consumed.History[2].Generation != done.Generation {
				t.Fatal("continuation replaced gate or lost lifecycle", consumed)
			}
			attempts, err = sqlite.Attempts(ctx, run.ID)
			if err != nil || len(attempts) != 3 || attempts[0].ID != checkpoint {
				t.Fatal("completed preparation replayed", attempts, err)
			}
		})
	}
}

func TestExternalInputContractControlRejectionsPreserveState(t *testing.T) {
	for _, control := range []string{"canceled", "uncertain_pending", "uncertain_accepted"} {
		t.Run(control, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, sqlite := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			e.Store = inputContractStore{ExternalInputJournal: sqlite}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusWaiting {
				t.Fatal(run, err)
			}
			gate := inputGate(t, sqlite, run.ID)
			if control != "uncertain_pending" {
				answerGate(t, sqlite, gate)
			}
			if control == "canceled" {
				if err = sqlite.RequestCancelGeneration(ctx, run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			} else {
				// As in the existing pending-Resume regression, seed a durable
				// unresolved attempt without launching any physical effect.
				if _, _, err = sqlite.Begin(ctx, run.ID, "independent", "effect", 1, false); err != nil {
					t.Fatal(err)
				}
				if err = sqlite.InterruptInput(ctx, run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			}
			before := inputContractSnapshot(t, sqlite, run.ID)
			_, err = e.Resume(ctx, run.ID, nil, false)
			want := "retry_uncertain"
			if control == "canceled" {
				want = "canceled input gates"
			}
			if err == nil || !strings.Contains(err.Error(), want) || calls.Load() != 1 {
				t.Fatal("complete contract bypassed control admission", err, calls.Load())
			}
			if after := inputContractSnapshot(t, sqlite, run.ID); after != before {
				t.Fatal("rejected admission mutated run, pause, answer history or checkpoints")
			}
			if control == "canceled" {
				if _, err = e.Resume(ctx, run.ID, nil, true); err == nil || !strings.Contains(err.Error(), want) || inputContractSnapshot(t, sqlite, run.ID) != before {
					t.Fatal("retry acknowledgement revived canceled gates", err)
				}
			}
		})
	}
}

func TestExternalInputContractLegacyJournalOrdinaryResume(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	e, sqlite := durableExecutor(t, `jobs: {x: {steps: [{id: before, uses: agent/a, with: {prompt: before}}, {id: retry, uses: agent/a, with: {prompt: retry}}, {id: after, uses: agent/a, with: {prompt: after}}]}}`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		n := calls.Add(1)
		if req.Prompt == "retry" && n == 2 {
			return nil, errors.New("ordinary retry fixture")
		}
		return map[string]any{}, nil
	}))
	e.Store = inputContractLegacyStore{Journal: sqlite}
	if _, ok := e.Store.(ExternalInputJournal); ok {
		t.Fatal("legacy wrapper exposed the optional extension")
	}
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status != RunStatusFailed || calls.Load() != 2 {
		t.Fatal("ordinary Journal-only execution changed", run, err, calls.Load())
	}
	if _, err = e.Resume(ctx, run.ID, nil, false); err == nil || calls.Load() != 2 {
		t.Fatal("legacy Resume bypassed uncertain-attempt acknowledgement", err, calls.Load())
	}
	done, err := e.Resume(ctx, run.ID, nil, true)
	if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 4 {
		t.Fatal("ordinary Journal-only Resume lost compatibility/checkpoint reuse", done, err, calls.Load())
	}
}

func incompleteInputContractStore(sqlite *SQLiteStore, kind string) RunStore {
	switch kind {
	case "journal_only":
		return inputContractLegacyStore{Journal: sqlite}
	case "gates_without_admission":
		return inputContractGateStore{inputContractGateJournal: sqlite}
	case "admission_without_gates":
		return inputContractAdmissionStore{inputContractAdmissionJournal: sqlite}
	default:
		panic("unknown input contract fixture")
	}
}

func TestExternalInputContractIncompleteExtensionRejectsFreshRun(t *testing.T) {
	for _, kind := range []string{"journal_only", "gates_without_admission", "admission_without_gates"} {
		for _, nested := range []bool{false, true} {
			name := kind + "/root"
			if nested {
				name = kind + "/nested"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				var calls atomic.Int32
				source := waitSource
				if nested {
					source = `jobs: {review: {uses: workflows/child.yml}, independent: {steps: [{uses: agent/a, with: {prompt: before-admission}}]}}`
				}
				e, sqlite := durableExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
					calls.Add(1)
					return map[string]any{}, nil
				}))
				if nested {
					writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\n"+waitSource)
				}
				e.Store = incompleteInputContractStore(sqlite, kind)
				if _, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), DurableExternalInputV1) {
					t.Fatal("incomplete wait contract admitted", err)
				}
				runs, err := sqlite.List(ctx)
				if err != nil || len(runs) != 0 || calls.Load() != 0 {
					t.Fatal("missing capability rejected after admission/effects", runs, err, calls.Load())
				}
			})
		}
	}
}

func TestExternalInputContractIncompleteExtensionRejectsRetainedSnapshot(t *testing.T) {
	for _, kind := range []string{"journal_only", "gates_without_admission", "admission_without_gates"} {
		for _, state := range []string{"queued", "waiting", "accepted_paused"} {
			for _, nested := range []bool{false, true} {
				name := kind + "/" + state + "/root"
				if nested {
					name = kind + "/" + state + "/nested"
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					var calls atomic.Int32
					source := waitSource
					if nested {
						source = `jobs: {review: {uses: workflows/child.yml}}`
					}
					e, sqlite := durableExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
						calls.Add(1)
						return map[string]any{}, nil
					}))
					if nested {
						writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\n"+waitSource)
					}
					var run *Run
					var err error
					if state == "queued" {
						run, err = e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
					} else {
						run, err = e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
					}
					if err != nil {
						t.Fatal(err)
					}
					if state == "accepted_paused" {
						gate := inputGate(t, sqlite, run.ID)
						answerGate(t, sqlite, gate)
						if err = sqlite.InterruptInput(ctx, run.ID, run.Generation); err != nil {
							t.Fatal(err)
						}
					}
					// Capability validation must use retained definitions even when
					// current files no longer contain any wait.
					writeWorkflow(t, e.WorkspaceDir, "main.yml", `jobs: {x: {steps: [{uses: agent/a, with: {prompt: changed}}]}}`)
					if nested {
						writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\njobs: {}")
					}
					e.Store = incompleteInputContractStore(sqlite, kind)
					before, beforeCalls := inputContractSnapshot(t, sqlite, run.ID), calls.Load()
					if _, err = e.Resume(ctx, run.ID, nil, false); err == nil || !strings.Contains(err.Error(), DurableExternalInputV1) {
						t.Fatal("retained wait snapshot accepted incomplete contract", err)
					}
					if after := inputContractSnapshot(t, sqlite, run.ID); after != before || calls.Load() != beforeCalls {
						t.Fatal("capability rejected after resume admission, control/history mutation or effects")
					}
				})
			}
		}
	}
}

// Capture public durable observations so a rejection cannot quietly allocate a
// generation, clear pause, consume an answer, claim an owner or rewrite a step.
func inputContractSnapshot(t *testing.T, sqlite *SQLiteStore, id string) string {
	t.Helper()
	ctx := context.Background()
	run, snap, err := sqlite.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	gates, err := sqlite.Inputs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	control, err := sqlite.InputControl(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := sqlite.Attempts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	events, err := sqlite.Events(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal([]any{run, snap, gates, control, attempts, events})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
