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

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type executionBoundaryJournal struct {
	*SQLiteStore
	afterSave  func(context.Context, *Run) error
	afterGate  func(context.Context, ExternalInputGate) error
	beforeSave func(context.Context, *Run) error
}

func (s *executionBoundaryJournal) Save(ctx context.Context, r *Run) error {
	if s.beforeSave != nil {
		if err := s.beforeSave(ctx, r); err != nil {
			return err
		}
	}
	if err := s.SQLiteStore.Save(ctx, r); err != nil {
		return err
	}
	if s.afterSave != nil {
		return s.afterSave(ctx, r)
	}
	return nil
}
func (s *executionBoundaryJournal) EnsureInputGate(ctx context.Context, g ExternalInputGate) (ExternalInputGate, error) {
	g, err := s.SQLiteStore.EnsureInputGate(ctx, g)
	if err == nil && s.afterGate != nil {
		err = s.afterGate(ctx, g)
	}
	return g, err
}

func TestExternalInputInterruptRetainsIndependentFailure(t *testing.T) {
	for _, timing := range []string{"executor", "publication"} {
		t.Run(timing, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			e, s := durableExecutor(t, `jobs:
  question: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}
  failure: {steps: [{uses: agent/a, with: {prompt: failure}}]}
  active: {steps: [{uses: agent/a, with: {prompt: active}}]}
`, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
				if req.Prompt == "failure" {
					return nil, errors.New("independent failure diagnostic")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}))
			wrapper := &executionBoundaryJournal{SQLiteStore: s}
			e.Store = wrapper
			canceled := false
			wrapper.afterSave = func(ctx context.Context, r *Run) error {
				if canceled || r.Status != RunStatusRunning || r.Jobs["failure"].Status != RunStatusFailed || r.Jobs["question"].Status != RunStatusWaiting {
					return nil
				}
				canceled = true
				if timing == "executor" {
					if err := s.InterruptInput(ctx, r.ID, r.Generation); err != nil {
						return err
					}
				}
				cancel()
				return nil
			}
			wrapper.beforeSave = func(ctx context.Context, r *Run) error {
				if timing == "publication" && r.Status != RunStatusRunning && r.Status != RunStatusQueued {
					return s.InterruptInput(ctx, r.ID, r.Generation)
				}
				return nil
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), "independent failure diagnostic") || run.Status != RunStatusFailed || run.FailureClass == "canceled" || !strings.Contains(run.Error, "independent failure diagnostic") {
				t.Fatalf("failure erased: %+v %v", run, err)
			}
			saved, _, loadErr := s.Load(context.Background(), run.ID)
			control, controlErr := s.InputControl(context.Background(), run.ID)
			if loadErr != nil || controlErr != nil || saved.Status != run.Status || saved.Error != run.Error || !control.Paused || inputGate(t, s, run.ID).State != "pending" {
				t.Fatalf("durable failure/control lost: %+v %+v %v %v", saved, control, loadErr, controlErr)
			}
		})
	}
}

func TestExternalInputOrderedInterruptAfterGateCommit(t *testing.T) {
	for _, mode := range []string{"resume", "accepted-at-commit", "acknowledged", "uncertain", "canceled", "independent-failure"} {
		t.Run(mode, func(t *testing.T) {
			retry := mode == "acknowledged"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			e, s := durableExecutor(t, `jobs: {review: {uses: workflows/child.yml, for-each: {items: [{id: a}], key: item.id}}}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs:
  x:
    steps:
      - {id: before, uses: agent/a, with: {prompt: before}}
      - {id: question, uses: input/wait, with: {prompt: q, evidence-id: e}}
      - {id: after, uses: agent/a, with: {prompt: after}}
`)
			wrapper := &executionBoundaryJournal{SQLiteStore: s}
			wrapper.afterGate = func(ctx context.Context, g ExternalInputGate) error {
				wrapper.afterGate = nil
				if mode == "accepted-at-commit" {
					if _, err := s.AcceptInput(context.WithoutCancel(ctx), g.OriginID, g.ID, ExternalInputAnswer{AnswerID: "answer-" + g.ID, EvidenceID: g.EvidenceID, Value: json.RawMessage(`"approved"`)}); err != nil {
						return err
					}
				}
				if err := s.InterruptInput(context.WithoutCancel(ctx), g.OriginID, g.Generation); err != nil {
					return err
				}
				cancel()
				return nil
			}
			e.Store = wrapper
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if run == nil || err != nil || run.Status != RunStatusInterrupted || run.Error != "" || run.FailureClass != "" || !run.FinishedAt.IsZero() || run.Jobs["review"].Status != RunStatusInterrupted {
				t.Fatalf("pure wait interruption reported failure: %+v %v", run, err)
			}
			saved, _, loadErr := s.Load(context.Background(), run.ID)
			control, controlErr := s.InputControl(context.Background(), run.ID)
			if loadErr != nil || controlErr != nil || saved.Status != RunStatusInterrupted || saved.Error != "" || saved.FailureClass != "" || !saved.FinishedAt.IsZero() || !control.Paused || control.Canceled {
				t.Fatalf("incoherent persisted interruption: %+v %+v %v %v", saved, control, loadErr, controlErr)
			}
			records, recordErr := s.IterationAttempts(context.Background(), run.ID, "review")
			if recordErr != nil || len(records) != 2 || records[1].Status != "waiting" {
				t.Fatalf("boundary not reproduced: %+v %+v %v %v", run, records, err, recordErr)
			}
			original := inputGate(t, s, run.ID)
			wantState, wantHistory := "pending", 1
			if mode == "accepted-at-commit" {
				wantState, wantHistory = "accepted", 2
			}
			if original.State != wantState || len(original.History) != wantHistory {
				t.Fatalf("interruption changed gate: %+v", original)
			}
			if original.State == "pending" {
				answerGate(t, s, original)
			}
			switch mode {
			case "uncertain":
				if _, _, err := s.Begin(context.Background(), original.RunID, "independent", "effect", 1, false); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				if err := s.RequestCancelGeneration(context.Background(), run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			case "independent-failure":
				child, _, err := s.Load(context.Background(), original.RunID)
				if err != nil {
					t.Fatal(err)
				}
				child.Status, child.FailureClass, child.Error = RunStatusFailed, "execution", "independent failure"
				if err := s.Save(context.Background(), child); err != nil {
					t.Fatal(err)
				}
			}
			done, err := e.Resume(context.Background(), run.ID, nil, retry)
			if mode == "uncertain" || mode == "canceled" || mode == "independent-failure" {
				if err == nil || calls.Load() != 1 {
					t.Fatalf("unsafe child resumed: %+v %v calls=%d", done, err, calls.Load())
				}
				gate := inputGate(t, s, run.ID)
				want := "accepted"
				if mode == "canceled" {
					want = "canceled"
				}
				if gate.ID != original.ID || gate.State != want || gate.Answer == nil {
					t.Fatalf("guard changed answer: %+v", gate)
				}
				return
			}
			if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 2 {
				t.Fatalf("retained child did not continue: %+v %v calls=%d", done, err, calls.Load())
			}
			gates, err := s.Inputs(context.Background(), run.ID)
			records, recordErr = s.IterationAttempts(context.Background(), run.ID, "review")
			if err != nil || recordErr != nil || len(gates) != 1 || gates[0].ID != original.ID || gates[0].State != "consumed" || len(gates[0].History) != 3 {
				t.Fatalf("answer not consumed exactly once: %+v %v %v", gates, err, recordErr)
			}
			for _, record := range records {
				if record.Attempt != 1 || record.ChildID != original.RunID {
					t.Fatalf("physical attempt changed: %+v", records)
				}
			}
		})
	}
}

func TestFailureClassMixedCancellation(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"},
		{errors.Join(context.Canceled, context.Canceled), "canceled"},
		{fmt.Errorf("wrapped: %w", errors.Join(context.Canceled, context.Canceled)), "canceled"},
		{fmt.Errorf("wrapped: %w", errors.Join(errors.New("command failed independently"), context.Canceled)), "command"},
		{errors.Join(errors.New("command failed independently"), context.Canceled), "command"},
		{errors.Join(ErrProviderFailure, context.Canceled), "provider_failure"},
		{errors.Join(process.ErrCleanup, context.Canceled), "needs_attention"},
	} {
		if got := FailureClass(tc.err); got != tc.want {
			t.Errorf("%v: got %s want %s", tc.err, got, tc.want)
		}
	}
}

func TestExternalInputOrderedInterruptionPreservesUnsafeChild(t *testing.T) {
	for _, mode := range []string{"uncertain", "failure", "cleanup", "integrity", "no-gate"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			var store *SQLiteStore
			e, s := durableExecutor(t, `jobs: {review: {uses: workflows/child.yml, for-each: {items: [{id: a}], key: item.id}}}`, agentFunc(func(ctx context.Context, request AgentRequest) (map[string]any, error) {
				close(started)
				switch mode {
				case "no-gate":
					_, snapshot, err := store.Load(ctx, request.RunID)
					if err != nil {
						return nil, err
					}
					origin, _, err := store.Load(ctx, nativeString(snapshot.Metadata, "origin_run_id"))
					if err != nil {
						return nil, err
					}
					if err := store.InterruptInput(ctx, origin.ID, origin.Generation); err != nil {
						return nil, err
					}
					cancel()
					return nil, context.Canceled
				case "failure":
					return nil, errors.New("independent genuine diagnostic")
				case "cleanup":
					return nil, process.ErrCleanup
				case "integrity":
					return nil, ErrActionIntegrity
				default:
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}))
			store = s
			source := `on: {workflow_call: {}}
jobs:
  question: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}
  independent: {steps: [{uses: agent/a, with: {prompt: effect}}]}
`
			if mode == "no-gate" {
				source = `on: {workflow_call: {}}
jobs: {independent: {steps: [{uses: agent/a, with: {prompt: effect}}]}}
`
			}
			writeWorkflow(t, e.WorkspaceDir, "child.yml", source)
			wrapper := &executionBoundaryJournal{SQLiteStore: s}
			interrupted := false
			interrupt := func(ctx context.Context, origin string, generation int64) error {
				if err := s.InterruptInput(context.WithoutCancel(ctx), origin, generation); err != nil {
					return err
				}
				interrupted = true
				cancel()
				return nil
			}
			wrapper.afterGate = func(ctx context.Context, g ExternalInputGate) error {
				if mode != "uncertain" {
					return nil
				}
				select {
				case <-started:
				case <-ctx.Done():
					return ctx.Err()
				}
				return interrupt(ctx, g.OriginID, g.Generation)
			}
			wrapper.afterSave = func(ctx context.Context, r *Run) error {
				if interrupted || r.OriginID == r.ID || r.Status != RunStatusRunning {
					return nil
				}
				if mode == "no-gate" {
					return nil
				}
				if mode == "uncertain" || r.Jobs["question"].Status != RunStatusWaiting {
					return nil
				}
				status := r.Jobs["independent"].Status
				if status != RunStatusFailed && status != RunStatusNeedsAttention {
					return nil
				}
				return interrupt(ctx, r.OriginID, r.Generation)
			}
			e.Store = wrapper
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || run.Status != RunStatusNeedsAttention || run.Error == "" {
				t.Fatalf("unsafe child reported pure interruption: %+v %v", run, err)
			}
			if mode == "failure" && !strings.Contains(run.Error, "independent genuine diagnostic") {
				t.Fatal("genuine diagnostic lost", run.Error)
			}
			saved, _, loadErr := s.Load(context.Background(), run.ID)
			control, controlErr := s.InputControl(context.Background(), run.ID)
			if loadErr != nil || controlErr != nil || saved.Status != run.Status || saved.Error != run.Error || !control.Paused {
				t.Fatalf("unsafe durable state changed: %+v %+v %v %v", saved, control, loadErr, controlErr)
			}
			if mode != "no-gate" && inputGate(t, s, run.ID).State != "pending" {
				t.Fatal("unsafe outcome invalidated question")
			}
			if mode == "uncertain" || mode == "no-gate" {
				if _, err := e.Resume(context.Background(), run.ID, nil, false); err == nil {
					t.Fatal("uncertain attempt resumed without acknowledgement")
				}
			}
		})
	}
}

func TestExternalInputFailureReturningAfterCancellation(t *testing.T) {
	for _, boundary := range []string{"step", "repeat", "reusable"} {
		for _, cause := range []struct {
			name   string
			err    error
			status string
		}{
			{"provider", ErrProviderFailure, RunStatusFailed}, {"generic", errors.New("genuine late diagnostic"), RunStatusFailed},
			{"provider-bare", ErrProviderFailure, RunStatusFailed}, {"generic-bare", errors.New("genuine late diagnostic"), RunStatusFailed},
			{"cleanup", process.ErrCleanup, RunStatusNeedsAttention}, {"integrity", ErrActionIntegrity, RunStatusNeedsAttention}, {"pure", nil, RunStatusInterrupted},
		} {
			t.Run(boundary+"/"+cause.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				started := make(chan struct{})
				job := `{steps: [{uses: agent/a, with: {prompt: effect}}]}`
				if boundary == "repeat" {
					job = `{repeat: {max-attempts: 2, until: 'iteration.number == 2'}, steps: [{uses: agent/a, with: {prompt: effect}}]}`
				}
				if boundary == "reusable" {
					job = `{uses: workflows/child.yml}`
				}
				e, s := durableExecutor(t, "jobs:\n  question: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}\n  effect: "+job+"\n", agentFunc(func(ctx context.Context, _ AgentRequest) (map[string]any, error) {
					close(started)
					<-ctx.Done()
					if strings.HasSuffix(cause.name, "-bare") {
						return nil, cause.err
					}
					return nil, errors.Join(cause.err, ctx.Err())
				}))
				if boundary == "reusable" {
					writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\njobs: {effect: {steps: [{uses: agent/a, with: {prompt: effect}}]}}")
				}
				wrapper := &executionBoundaryJournal{SQLiteStore: s}
				wrapper.afterGate = func(ctx context.Context, g ExternalInputGate) error {
					select {
					case <-started:
					case <-ctx.Done():
						return ctx.Err()
					}
					if err := s.InterruptInput(context.WithoutCancel(ctx), g.OriginID, g.Generation); err != nil {
						return err
					}
					cancel()
					return nil
				}
				e.Store = wrapper
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				if run == nil || run.Status != cause.status {
					t.Fatalf("incorrect outcome: %+v %v", run, err)
				}
				if cause.err == nil {
					if err != nil || run.Error != "" || run.FailureClass != "" {
						t.Fatalf("pure cancellation became failure: %+v %v", run, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), cause.err.Error()) || !strings.Contains(run.Error, cause.err.Error()) || run.FailureClass == "canceled" {
					t.Fatalf("genuine failure lost: %+v %v", run, err)
				}
				if errors.Is(cause.err, ErrProviderFailure) && run.FailureClass != "provider_failure" {
					t.Fatalf("provider class lost: %+v", run)
				}
				if errors.Is(cause.err, ErrActionIntegrity) && run.FailureClass != "action_integrity" {
					t.Fatalf("integrity class lost: %+v", run)
				}
				attempts, attemptsErr := s.Attempts(context.Background(), run.ID)
				if attemptsErr != nil || len(attempts) != 1 {
					t.Fatalf("attempts: %+v %v", attempts, attemptsErr)
				}
				wantAttempt := cause.status
				if cause.err == nil {
					wantAttempt = RunStatusCanceled
				}
				if attempts[0].Status != wantAttempt || (cause.err != nil && attempts[0].Result.FailureClass == "canceled") {
					t.Fatalf("step outcome discarded failure: %+v", attempts[0])
				}
				saved, _, loadErr := s.Load(context.Background(), run.ID)
				control, controlErr := s.InputControl(context.Background(), run.ID)
				if loadErr != nil || controlErr != nil || saved.Status != run.Status || saved.Error != run.Error || !control.Paused || inputGate(t, s, run.ID).State != "pending" {
					t.Fatalf("durable outcome lost: %+v %+v %v %v", saved, control, loadErr, controlErr)
				}
			})
		}
	}
}

func TestExternalInputRepeatedOrderedInterruptions(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			var calls atomic.Int32
			e, s := durableExecutor(t, `jobs: {review: {uses: workflows/child.yml, for-each: {items: [{id: a}], key: item.id}}}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs:
  x:
    steps:
      - {uses: agent/a, with: {prompt: before}}
      - {id: q1, uses: input/wait, with: {prompt: one, evidence-id: e1}}
      - {uses: agent/a, with: {prompt: middle-one}}
      - {id: q2, uses: input/wait, with: {prompt: two, evidence-id: e2}}
      - {uses: agent/a, with: {prompt: middle-two}}
      - {id: q3, uses: input/wait, with: {prompt: three, evidence-id: e3}}
      - {uses: agent/a, with: {prompt: after}}
`)
			wrapper := &executionBoundaryJournal{SQLiteStore: s}
			e.Store = wrapper
			var run *Run
			var childID, legacyFingerprint string
			for index := 0; index < 3; index++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				wrapper.afterGate = func(ctx context.Context, g ExternalInputGate) error {
					if g.State != "pending" {
						return nil
					}
					if err := s.InterruptInput(context.WithoutCancel(ctx), g.OriginID, g.Generation); err != nil {
						return err
					}
					cancel()
					return nil
				}
				var err error
				if index == 0 {
					run, err = e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				} else {
					run, err = e.Resume(ctx, run.ID, nil, false)
				}
				cancel()
				if err != nil || run == nil || run.Status != RunStatusInterrupted || run.Error != "" {
					t.Fatalf("interrupt %d: %+v %v", index+1, run, err)
				}
				gate := inputGate(t, s, run.ID)
				if childID == "" {
					childID = gate.RunID
				}
				if gate.RunID != childID || gate.StepID != fmt.Sprint("q", index+1) || gate.State != "pending" || calls.Load() != int32(index+1) {
					t.Fatalf("gate/checkpoint changed: %+v calls=%d", gate, calls.Load())
				}
				if legacy && index == 0 {
					records, err := s.IterationAttempts(context.Background(), run.ID, "review")
					if err != nil {
						t.Fatal(err)
					}
					last := records[len(records)-1]
					if last.Status != "interrupted" {
						last.Status = "interrupted"
						last.FailureClass = "canceled"
						last.Error = context.Canceled.Error()
						last.Fingerprint = iterationResultHash(last)
						legacyFingerprint = last.Fingerprint
						if err := s.AppendIteration(context.Background(), run.ID, "review", last); err != nil {
							t.Fatal(err)
						}
					}
				}
				// Unanswered resume and another pause preserve this gate without admitting
				// another execution owner or reserving another item outcome.
				generation := run.Generation
				unanswered, err := e.Resume(context.Background(), run.ID, nil, false)
				if err != nil || unanswered.Status != RunStatusWaiting || unanswered.Generation != generation {
					t.Fatalf("unanswered resume: %+v %v", unanswered, err)
				}
				if err := s.InterruptInput(context.Background(), run.ID, generation); err != nil {
					t.Fatal(err)
				}
				if again := inputGate(t, s, run.ID); again.ID != gate.ID || len(again.History) != 1 {
					t.Fatal("same question changed", again)
				}
				answerGate(t, s, gate)
			}
			wrapper.afterGate = nil
			done, err := e.Resume(context.Background(), run.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 4 {
				t.Fatalf("completion: %+v %v calls=%d", done, err, calls.Load())
			}
			gates, err := s.Inputs(context.Background(), run.ID)
			if err != nil || len(gates) != 3 {
				t.Fatalf("gates: %+v %v", gates, err)
			}
			for _, gate := range gates {
				if gate.RunID != childID || gate.State != "consumed" || len(gate.History) != 3 {
					t.Fatalf("answer history changed: %+v", gate)
				}
			}
			records, err := s.IterationAttempts(context.Background(), run.ID, "review")
			if err != nil {
				t.Fatal(err)
			}
			completed, interrupted := 0, 0
			for _, record := range records {
				if record.ChildID != childID || record.Attempt != 1 {
					t.Fatalf("attempt identity changed: %+v", records)
				}
				if record.Status == "completed" {
					completed++
				}
				if record.Status == "interrupted" {
					interrupted++
					if record.Fingerprint != legacyFingerprint {
						t.Fatal("legacy terminal history changed", record)
					}
				}
			}
			wantInterrupted := 0
			if legacy {
				wantInterrupted = 1
			}
			if completed != 1 || interrupted != wantInterrupted {
				t.Fatalf("terminal history changed: %+v", records)
			}
		})
	}
}

func TestExternalInputDeadlineOutcomeCompatibility(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	e := &Executor{}
	definitions := map[string]*Workflow{"main": {Jobs: map[string]Job{"x": {Steps: []Step{{Uses: "agent/a"}}}}}}
	run, err := e.execute(ctx, "main", RunRequest{Ref: "main"}, definitions, make(chan struct{}, 1), 0, nil)
	if !errors.Is(err, context.DeadlineExceeded) || run == nil || run.Status != RunStatusCanceled || run.FailureClass != "timeout" {
		t.Fatalf("deadline run outcome changed: %+v %v", run, err)
	}
	if job := run.Jobs["x"]; job.Status != RunStatusCanceled || job.FailureClass != "timeout" {
		t.Fatalf("deadline job outcome changed: %+v", job)
	}
}
