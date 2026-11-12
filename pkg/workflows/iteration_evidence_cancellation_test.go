package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type iterationEvidenceBoundary struct {
	*SQLiteStore
	pinned atomic.Bool
	check  func(context.Context) error
}

func (s *iterationEvidenceBoundary) PinIteration(ctx context.Context, p IterationPlan) error {
	err := s.SQLiteStore.PinIteration(ctx, p)
	if err == nil {
		s.pinned.Store(true)
	}
	return err
}

func (s *iterationEvidenceBoundary) CheckIteration(ctx context.Context) error {
	if s.check != nil {
		return s.check(ctx)
	}
	return s.SQLiteStore.CheckIteration(ctx)
}

func TestParallelIterationSuspensionDuringEvidenceCheck(t *testing.T) {
	for _, phase := range []string{"journal", "ready"} {
		for _, independent := range []struct {
			name string
			err  error
		}{
			{name: "cancellation"},
			{name: "integrity", err: ErrActionIntegrity},
			{name: "storage", err: errors.New("independent storage failure")},
			{name: "cleanup", err: process.ErrCleanup},
		} {
			t.Run(phase+"/"+independent.name, func(t *testing.T) {
				e, store := parallelIterationExecutor(t, 2, 2, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
					t.Error("evidence-blocked sibling dispatched an agent")
					return nil, nil
				}))
				writeWorkflow(t, e.WorkspaceDir, "child.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
      ordinal: {type: number}
      identity: {type: string}
jobs:
  question:
    steps:
      - uses: input/wait
        with: {prompt: 'Continue?', evidence-id: retained}
`)
				wrapper := &iterationEvidenceBoundary{SQLiteStore: store}
				e.Store = wrapper
				entered := make(chan struct{})
				observed := make(chan error, 1)
				joined := make(chan struct{})
				var calls atomic.Int32
				check := func(ctx context.Context) error {
					switch calls.Add(1) {
					case 1:
						// Neither item can create its child until both outer item
						// controllers have reached the selected evidence phase.
						select {
						case <-entered:
						case <-ctx.Done():
							return ctx.Err()
						}
					case 2:
						defer close(joined)
						close(entered)
						<-ctx.Done()
						var err error
						if phase == "journal" {
							// Exercise the actual SQLite transaction admission with
							// the context canceled by the first item's suspension.
							err = store.CheckIteration(ctx)
						} else {
							err = ctx.Err()
						}
						observed <- err
						return errors.Join(err, independent.err)
					}
					return store.CheckIteration(ctx)
				}
				if phase == "journal" {
					wrapper.check = check
				} else {
					e.Ready = func(ctx context.Context) error {
						if !wrapper.pinned.Load() {
							return nil // Preflight precedes the pinned iteration plan.
						}
						return check(ctx)
					}
				}
				ctx, _, done := startParallelIteration(t, e, iterationRequest("first", "second", "pending"))
				result := iterationReceive(t, ctx, done)
				if err := iterationReceive(t, ctx, observed); !errors.Is(err, context.Canceled) {
					t.Fatal("boundary did not observe sibling cancellation:", err)
				}
				select {
				case <-joined:
				default:
					t.Fatal("returned before joining the evidence check")
				}
				job := result.run.Jobs["review"]
				if independent.err == nil {
					if result.err != nil || result.run.Status != RunStatusWaiting || job.Status != RunStatusWaiting {
						t.Fatalf("pure evidence cancellation overrode suspension: run=%+v job=%+v err=%v", result.run, job, result.err)
					}
				} else if result.err == nil || result.run.Status != RunStatusNeedsAttention || job.Status != RunStatusNeedsAttention || job.FailureClass == "canceled" {
					t.Fatalf("suspension hid independent evidence failure: run=%+v job=%+v err=%v", result.run, job, result.err)
				}
				waiting, pending := 0, 0
				for _, item := range job.Items {
					switch item.Status {
					case "waiting":
						waiting++
					case "pending":
						pending++
					default:
						t.Fatal("unexpected item observation:", item)
					}
				}
				if waiting != 1 || pending != 2 || job.Complete {
					t.Fatal("evidence cancellation admitted unfinished work:", job)
				}
				gates, err := store.Inputs(ctx, result.run.ID)
				if err != nil || len(gates) != 1 || gates[0].State != "pending" || gates[0].EvidenceID != "retained" {
					t.Fatalf("retained gates=%+v err=%v", gates, err)
				}
			})
		}
	}
}

func TestIterationStepReadinessPreservesCancellation(t *testing.T) {
	for _, independent := range []error{nil, ErrActionIntegrity, process.ErrCleanup, errors.New("storage failure")} {
		t.Run(classifyFailure(independent), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := &Executor{Ready: func(context.Context) error {
				cancel()
				return errors.Join(ctx.Err(), independent)
			}}
			_, err := e.executeStep(ctx, Step{}, expressionContext{}, make(chan struct{}, 1))
			if independent == nil {
				if !cancellationOnly(err) {
					t.Fatal("readiness lost cancellation identity:", err)
				}
			} else if err == nil || cancellationOnly(err) {
				t.Fatal("readiness discarded an independent failure:", err)
			}
		})
	}
}

func TestIterationRecoveryValidationPreservesCancellation(t *testing.T) {
	for _, phase := range []string{"bindings", "checkpoint"} {
		for _, independent := range []error{nil, process.ErrCleanup, errors.New("storage failure")} {
			t.Run(phase+"/"+classifyFailure(independent), func(t *testing.T) {
				e, store := durableExecutor(t, actionSource, nil)
				var calls atomic.Int32
				spec := readSpec(&calls)
				var failure error
				if phase == "bindings" {
					bindings := spec.Bindings
					spec.Bindings = func(ctx context.Context, ac ActionContext, in actionInput) (json.RawMessage, error) {
						if failure != nil {
							return nil, failure
						}
						return bindings(ctx, ac, in)
					}
				} else {
					validate := spec.ValidateCheckpoint
					spec.ValidateCheckpoint = func(ctx context.Context, ac ActionContext, checkpoint ActionCheckpoint[actionInput, actionOutput]) error {
						if failure != nil {
							return failure
						}
						return validate(ctx, ac, checkpoint)
					}
				}
				registerTestAction(t, e, spec)
				ctx := context.Background()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				if err != nil {
					t.Fatal(err)
				}
				_, snapshot, err := store.Load(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := e.configureActions(snapshot.Definitions); err != nil {
					t.Fatal(err)
				}
				failure = errors.Join(context.Canceled, independent)
				err = e.validateActionRecovery(ctx, run.ID, snapshot)
				if independent == nil {
					if !cancellationOnly(err) {
						t.Fatal("checkpoint validation lost cancellation identity:", err)
					}
				} else if !errors.Is(err, ErrActionIntegrity) || cancellationOnly(err) {
					t.Fatal("checkpoint validation discarded an independent failure:", err)
				}
				if calls.Load() != 1 {
					t.Fatal("validation replayed the action")
				}
			})
		}
	}
}

func TestIterationRetryReconciliationPreservesCancellation(t *testing.T) {
	for _, independent := range []error{nil, process.ErrCleanup, errors.New("storage failure")} {
		t.Run(classifyFailure(independent), func(t *testing.T) {
			e, store := iterationExecutor(t, nil)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", strings.Replace(iterationChild, "uses: agent/reviewer\n        with: {prompt: '${{ inputs.task }}'}", "uses: action/capture\n        with: {value: '${{ inputs.task }}'}", 1))
			var mutations, reconciles atomic.Int32
			var cancelResume context.CancelFunc
			spec := readSpec(&mutations)
			spec.Effect = ActionExternalMutation
			spec.Handler = func(context.Context, ActionContext, actionInput) (ActionResult[actionOutput], error) {
				mutations.Add(1)
				return ActionResult[actionOutput]{}, errors.New("lost remote acknowledgement")
			}
			spec.Reconcile = func(ctx context.Context, _ ActionContext, _ ActionCheckpoint[actionInput, actionOutput]) (ActionReconciliation[actionOutput], error) {
				reconciles.Add(1)
				cancelResume()
				return ActionReconciliation[actionOutput]{}, errors.Join(ctx.Err(), independent)
			}
			registerTestAction(t, e, spec)
			ctx := context.Background()
			run, err := e.Run(ctx, iterationRequest("mutation"))
			if err == nil {
				t.Fatal("missing mutation acknowledgement accepted")
			}
			resumeCtx, cancel := context.WithCancel(ctx)
			cancelResume = cancel
			defer cancel()
			_, err = e.Resume(resumeCtx, run.ID, nil, true)
			if independent == nil {
				if !cancellationOnly(err) {
					t.Fatal("reconciliation lost cancellation identity:", err)
				}
			} else if !errors.Is(err, ErrActionIntegrity) || cancellationOnly(err) {
				t.Fatal("reconciliation discarded an independent failure:", err)
			}
			if mutations.Load() != 1 || reconciles.Load() != 1 {
				t.Fatal("canceled reconciliation repeated a mutation:", mutations.Load(), reconciles.Load())
			}
			records, err := store.ActionAttempts(ctx, run.ID)
			if err != nil || len(records) != 1 || records[0].State != "admitted" || len(records[0].Reconciliations) != 1 || records[0].Reconciliations[0].Outcome != "unknown" {
				t.Fatalf("canceled reconciliation lost uncertainty: records=%+v err=%v", records, err)
			}
		})
	}
}

func TestApplicationActionAdmissionPreservesCancellation(t *testing.T) {
	for _, phase := range []string{"ready", "bindings"} {
		for _, independent := range []error{nil, process.ErrCleanup, errors.New("storage failure")} {
			t.Run(phase+"/"+classifyFailure(independent), func(t *testing.T) {
				e, store := durableExecutor(t, actionSource, nil)
				var calls atomic.Int32
				spec := readSpec(&calls)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				interrupt := func() error {
					cancel()
					return errors.Join(ctx.Err(), independent)
				}
				if phase == "bindings" {
					spec.Bindings = func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
						return nil, interrupt()
					}
				} else {
					e.Ready = func(context.Context) error { return interrupt() }
				}
				registerTestAction(t, e, spec)
				definitions, err := e.Load("workflows/main.yml")
				if err != nil {
					t.Fatal(err)
				}
				if err := e.configureActions(definitions); err != nil {
					t.Fatal(err)
				}
				step := definitions["workflows/main.yml"].Jobs["capture"].Steps[0]
				_, err = e.executeApplicationAction(ctx, step, expressionContext{RunID: "unadmitted", JobID: "capture"})
				if independent == nil {
					if !cancellationOnly(err) {
						t.Fatal("action admission lost cancellation identity:", err)
					}
				} else if !errors.Is(err, ErrActionIntegrity) || cancellationOnly(err) {
					t.Fatal("action admission discarded an independent failure:", err)
				}
				records, loadErr := store.ActionAttempts(context.Background(), "unadmitted")
				if calls.Load() != 0 || loadErr != nil || len(records) != 0 {
					t.Fatal("interrupted admission dispatched or journaled an action:", calls.Load(), records, loadErr)
				}
			})
		}
	}
}

func TestMutationHandlerCancellationRemainsUncertain(t *testing.T) {
	e, store := durableExecutor(t, actionSource, nil)
	var calls atomic.Int32
	spec := readSpec(&calls)
	spec.Effect = ActionExternalMutation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec.Handler = func(context.Context, ActionContext, actionInput) (ActionResult[actionOutput], error) {
		calls.Add(1)
		cancel()
		return ActionResult[actionOutput]{}, ctx.Err()
	}
	registerTestAction(t, e, spec)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if !errors.Is(err, ErrActionIntegrity) || cancellationOnly(err) || run.Status != RunStatusNeedsAttention || calls.Load() != 1 {
		t.Fatalf("mutation cancellation lost uncertainty: run=%+v calls=%d err=%v", run, calls.Load(), err)
	}
	records, err := store.ActionAttempts(context.Background(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "admitted" || records[0].Error == "" {
		t.Fatalf("mutation cancellation lost admitted effect: records=%+v err=%v", records, err)
	}
}
