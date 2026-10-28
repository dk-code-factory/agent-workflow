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

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type lifecycleConsumeJournal struct {
	*SQLiteStore
	afterConsume func(ExternalInputGate)
}

func (s *lifecycleConsumeJournal) ConsumeInput(ctx context.Context, id string) (ExternalInputGate, error) {
	gate, err := s.SQLiteStore.ConsumeInput(ctx, id)
	if err == nil && s.afterConsume != nil {
		s.afterConsume(gate)
	}
	return gate, err
}

func TestInputLifecycleCancellationRecoversBoundaries(t *testing.T) {
	for _, boundary := range []string{"gate", "accepted", "queued", "consumed"} {
		t.Run(boundary, func(t *testing.T) {
			var calls atomic.Int32
			e, s := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if boundary == "gate" || boundary == "accepted" {
				e.Store = &executionBoundaryJournal{SQLiteStore: s, afterGate: func(ctx context.Context, g ExternalInputGate) error {
					if boundary == "accepted" {
						answerGate(t, s, g)
					}
					cancel()
					return nil
				}}
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if boundary == "queued" || boundary == "consumed" {
				if err != nil || run.Status != RunStatusWaiting {
					t.Fatal(run, err)
				}
				answerGate(t, s, inputGate(t, s, run.ID))
				if boundary == "queued" {
					run, err = s.QueueResume(context.Background(), run.ID, "", false, 128)
					if err != nil {
						t.Fatal(err)
					}
					run.Status = RunStatusCanceled
					run.FailureClass = "canceled"
					run.Error = context.Canceled.Error()
					run.FinishedAt = time.Now().UTC()
					if err = s.Save(context.Background(), run); err != nil {
						t.Fatal(err)
					}
				} else {
					e.Store = &lifecycleConsumeJournal{SQLiteStore: s, afterConsume: func(ExternalInputGate) { cancel() }}
					run, err = e.Resume(ctx, run.ID, nil, false)
				}
			}
			if run == nil || run.Status != RunStatusCanceled || run.FailureClass != "canceled" || calls.Load() != 1 {
				t.Fatalf("lifecycle boundary %+v %v calls=%d", run, err, calls.Load())
			}
			control, controlErr := s.InputControl(context.Background(), run.ID)
			if controlErr != nil || control.Canceled || control.Paused {
				t.Fatal(control, controlErr)
			}
			generation := run.Generation
			dir := s.Dir
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLite(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			e.Store = reopened
			if err = reopened.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			recovered, _, err := reopened.Load(context.Background(), run.ID)
			if err != nil || recovered.Status != RunStatusWaiting || recovered.Generation != generation {
				t.Fatal("lifecycle wait stranded", recovered, err)
			}
			gate := inputGate(t, reopened, run.ID)
			if boundary == "gate" {
				answerGate(t, reopened, gate)
			}
			if err = reopened.QueueInputContinuations(context.Background(), 128); err != nil {
				t.Fatal(err)
			}
			if err = reopened.QueueInputContinuations(context.Background(), 128); err != nil {
				t.Fatal(err)
			}
			done, err := e.Resume(context.Background(), run.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || done.Generation != generation+1 || calls.Load() != 3 {
				t.Fatal("restart replayed/lost continuation", done, err, calls.Load())
			}
			gate = inputGate(t, reopened, run.ID)
			if gate.State != "consumed" || gate.Answer == nil || len(gate.History) != 3 {
				t.Fatal("restart changed gate history", gate)
			}
		})
	}
}

func TestInputLifecycleRecoveryPreservesUnsafeAndControls(t *testing.T) {
	for _, mode := range []string{"explicit cancel", "cancel record", "uncertain attempt", "uncertain tool", "cleanup", "genuine failure", "mixed canceled", "timeout", "paused owner loss"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			run.Status = RunStatusCanceled
			run.FailureClass = "canceled"
			run.Error = context.Canceled.Error()
			if mode == "genuine failure" {
				run.Status = RunStatusFailed
				run.FailureClass = "provider_failure"
			}
			if mode == "mixed canceled" {
				run.FailureClass = "provider_failure"
			}
			if mode == "timeout" {
				run.FailureClass = "timeout"
			}
			if mode == "paused owner loss" {
				previous := RunStatusWaiting
				run.Status = RunStatusRunning
				if err = s.Claim(ctx, run, previous); err != nil {
					t.Fatal(err)
				}
				if err = s.InterruptInput(ctx, run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			} else if err = s.Save(ctx, run); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "explicit cancel":
				if err = s.RequestCancel(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
			case "cancel record":
				if _, err = s.db.Exec(`INSERT INTO cancellations VALUES(?,'fixture')`, run.ID); err != nil {
					t.Fatal(err)
				}
			case "uncertain attempt":
				if _, _, err = s.Begin(ctx, run.ID, "independent", "effect", 1, false); err != nil {
					t.Fatal(err)
				}
			case "uncertain tool":
				if _, err = s.db.Exec(`INSERT INTO tool_calls VALUES(?,1,'invocation','call','fixture','tool','fingerprint','uncertain','{}')`, run.ID); err != nil {
					t.Fatal(err)
				}
			case "cleanup":
				if _, err = s.db.Exec(`INSERT INTO invocations VALUES('unfinished',?,1,'uncertain','{}',0)`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			stored, _, err := s.Load(ctx, run.ID)
			want := run.Status
			if mode == "paused owner loss" {
				want = RunStatusInterrupted
			}
			if err != nil || stored.Status != want || stored.Generation != run.Generation {
				t.Fatal("unsafe/canceled origin recovered", stored, err)
			}
			gate := inputGate(t, s, run.ID)
			if mode == "explicit cancel" {
				if gate.State != "canceled" {
					t.Fatal(gate)
				}
			} else if gate.State != "accepted" || gate.Answer == nil || len(gate.History) != 2 {
				t.Fatal("unsafe history changed", gate)
			}
		})
	}
}

func TestInputLifecycleRetentionProtectsRecoverableHistory(t *testing.T) {
	for _, state := range []string{"pending", "accepted", "consumed"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {call: {uses: workflows/child.yml}}`, nil)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			gate := inputGate(t, s, run.ID)
			if state != "pending" {
				answerGate(t, s, gate)
			}
			if state == "consumed" {
				if _, err = s.ConsumeInput(ctx, gate.ID); err != nil {
					t.Fatal(err)
				}
			}
			records, err := s.listRecords(ctx, `SELECT record,generation,owner FROM runs WHERE origin_id=?`, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				record.Status = RunStatusCanceled
				record.FailureClass = "canceled"
				record.Error = context.Canceled.Error()
				if err = s.Save(ctx, &record); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.OriginRuns(ctx, run.ID); err == nil {
				t.Error("recoverable origin became retention eligible")
			}
			if err = s.DeleteOrigin(ctx, run.ID); err == nil {
				t.Fatal("recoverable origin history deleted")
			}
			if err = s.Delete(ctx, gate.RunID); err == nil {
				t.Fatal("recoverable descendant deleted")
			}
			if err = s.Delete(ctx, run.ID); err == nil {
				t.Fatal("recoverable root deleted")
			}
			lock, err := ownership.Own(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			retention := Retention{OriginID: run.ID, Root: e.WorkspaceDir, State: "planned"}
			if _, err = s.BeginRetention(ctx, retention, lock); err == nil {
				t.Fatal("filesystem retention allowed")
			}
			for _, planState := range []string{"planned", "removing"} {
				retention.State = planState
				raw, _ := json.Marshal(retention)
				if _, err = s.db.Exec(`INSERT OR REPLACE INTO retention_groups VALUES(?,?,?)`, run.ID, planState, raw); err != nil {
					t.Fatal(err)
				}
				if _, err = s.BeginRetention(ctx, retention, lock); err == nil {
					t.Fatal("stale retention plan resumed", planState)
				}
				if err = s.FinishRetention(ctx, retention, lock); err == nil {
					t.Fatal("stale retention plan deleted origin", planState)
				}
			}
			// Cancel explicitly retires the questions and permits normal terminal cleanup.
			if err = s.RequestCancel(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.DeleteOrigin(ctx, run.ID); err != nil {
				t.Fatal("explicit Cancel no longer retainable", err)
			}
		})
	}
}

func TestInputLifecycleRetentionKeepsTerminalCompatibility(t *testing.T) {
	for _, mode := range []string{"failed pending", "failed accepted", "failed consumed", "timeout consumed", "ordinary failed", "ordinary canceled", "attention", "cleanup", "cancel cleanup", "complete receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, run := executionStore(t)
			if strings.HasPrefix(mode, "failed ") || mode == "timeout consumed" || mode == "attention" || mode == "cleanup" || mode == "cancel cleanup" {
				gate := ExternalInputGate{ID: actionHash([]any{run.ID, "job", "input", 1}), OriginID: run.ID, RunID: run.ID, Generation: run.Generation, JobID: "job", StepID: "input", Iteration: 1, EvidenceID: "e", Prompt: "q"}
				gate, err := s.EnsureInputGate(ctx, gate)
				if err != nil {
					t.Fatal(err)
				}
				if mode != "failed pending" {
					answerGate(t, s, gate)
				}
				if mode != "failed pending" && mode != "failed accepted" {
					if _, err = s.ConsumeInput(ctx, gate.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "ordinary canceled" || mode == "timeout consumed" || mode == "cancel cleanup" {
				run.Status = RunStatusCanceled
				run.FailureClass = "timeout"
				if mode != "timeout consumed" {
					run.FailureClass = "canceled"
				}
			}
			if strings.HasPrefix(mode, "failed ") {
				run.FailureClass = "provider_failure"
			}
			if mode == "attention" {
				run.Status = RunStatusNeedsAttention
			}
			if err := s.Save(ctx, run); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel cleanup" {
				if err := s.RequestCancel(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cleanup" || mode == "cancel cleanup" {
				if _, err := s.db.Exec(`INSERT INTO invocations VALUES('unfinished',?,1,'uncertain','{}',0)`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "complete receipt" {
				lock, err := ownership.Own(s.Dir)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				record := Retention{OriginID: run.ID, Root: t.TempDir(), State: "complete"}
				raw, _ := json.Marshal(record)
				if _, err = s.db.Exec(`INSERT INTO retention_groups VALUES(?,'complete',?)`, run.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err = s.DeleteOrigin(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = s.BeginRetention(ctx, record, lock); err != nil {
					t.Fatal("complete receipt no longer idempotent after deletion", err)
				}
				return
			}
			err := s.DeleteOrigin(ctx, run.ID)
			blocked := mode == "attention" || mode == "cleanup" || mode == "cancel cleanup" || mode == "failed pending" || mode == "failed accepted"
			if (err != nil) != blocked {
				t.Fatal("terminal retention compatibility changed", err)
			}
		})
	}
}

func TestInputPauseSignalSuppressesOnlyPureDenial(t *testing.T) {
	for _, mode := range []string{"pure", "wrapped pure", "joined pure", "storage", "provider", "cleanup", "integrity", "generic conflict"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			signal := ErrExternalInputPaused
			var genuine error
			switch mode {
			case "wrapped pure":
				signal = fmt.Errorf("wrapped: %w", signal)
			case "joined pure":
				signal = errors.Join(signal, fmt.Errorf("second: %w", signal))
			case "storage":
				genuine = errors.New("storage read failed independently")
			case "provider":
				genuine = ErrProviderFailure
			case "cleanup":
				genuine = process.ErrCleanup
			case "integrity":
				genuine = ErrActionIntegrity
			case "generic conflict":
				genuine = ErrExternalInput
			}
			if genuine != nil {
				signal = errors.Join(signal, genuine)
			}
			e.Store = &consumeBoundaryJournal{SQLiteStore: s, beforeConsume: func(ctx context.Context, _ string) error {
				current, _, err := s.Load(ctx, run.ID)
				if err != nil {
					return err
				}
				if err = s.InterruptInput(ctx, run.ID, current.Generation); err != nil {
					return err
				}
				return signal
			}}
			result, err := e.Resume(ctx, run.ID, nil, false)
			want := RunStatusInterrupted
			if genuine != nil {
				want = RunStatusFailed
			}
			if mode == "cleanup" || mode == "integrity" {
				want = RunStatusNeedsAttention
			}
			if result == nil || result.Status != want || (err != nil) != (genuine != nil) {
				t.Fatalf("signal arbitration %+v %v", result, err)
			}
			if genuine != nil && (!strings.Contains(result.Error, genuine.Error()) || !strings.Contains(err.Error(), genuine.Error())) {
				t.Fatal("real diagnostic erased", result.Error, err)
			}
			gate := inputGate(t, s, run.ID)
			if gate.State != "accepted" || gate.Answer == nil || len(gate.History) != 2 {
				t.Fatal("pause changed accepted history", gate)
			}
		})
	}
}

func TestInputLifecycleRecoveryRechecksLateControls(t *testing.T) {
	for _, control := range []string{"cancel", "pause"} {
		t.Run(control, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			run.Status = RunStatusNeedsAttention
			run.FailureClass = "owner_loss"
			if err = s.Save(ctx, run); err != nil {
				t.Fatal(err)
			}
			// This candidate passed the outer recovery checks. A control committed before
			// atomic publication must still fence suspension and continuation intent.
			stale := *run
			if control == "cancel" {
				err = s.RequestCancel(ctx, run.ID)
			} else {
				err = s.InterruptInput(ctx, run.ID, run.Generation)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.recoverInputWait(ctx, stale); err != nil {
				t.Fatal(err)
			}
			if err = s.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			stored, _, err := s.Load(ctx, run.ID)
			want := RunStatusNeedsAttention
			if control == "pause" {
				want = RunStatusInterrupted
			}
			if err != nil || stored.Status != want || stored.Generation != run.Generation {
				t.Fatal("late control overwritten", stored, err)
			}
			gate := inputGate(t, s, run.ID)
			if control == "cancel" {
				if gate.State != "canceled" {
					t.Fatal(gate)
				}
			} else if gate.State != "accepted" || len(gate.History) != 2 {
				t.Fatal(gate)
			}
		})
	}
}

func TestInputLifecycleConsumedRetentionAdmissionWindow(t *testing.T) {
	for _, status := range []string{RunStatusQueued, RunStatusRunning, RunStatusNeedsAttention} {
		for _, cancelPending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", status, cancelPending), func(t *testing.T) {
				ctx := context.Background()
				e, s := durableExecutor(t, `jobs: {call: {uses: workflows/child.yml}}`, nil)
				writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`)
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				if err != nil {
					t.Fatal(err)
				}
				gate := inputGate(t, s, run.ID)
				answerGate(t, s, gate)
				if _, err = s.ConsumeInput(ctx, gate.ID); err != nil {
					t.Fatal(err)
				}
				child, _, err := s.Load(ctx, gate.RunID)
				if err != nil {
					t.Fatal(err)
				}
				child.Status = RunStatusCanceled
				child.FailureClass = "canceled"
				if err = s.Save(ctx, child); err != nil {
					t.Fatal(err)
				}
				// A terminal eligibility inspection can become stale before its DELETE.
				run.Status = RunStatusFailed
				run.FailureClass = "provider_failure"
				if err = s.Save(ctx, run); err != nil {
					t.Fatal(err)
				}
				if _, err = s.OriginRuns(ctx, run.ID); err != nil {
					t.Fatal("terminal consumed fixture not retainable", err)
				}
				run.Status = RunStatusCanceled
				run.FailureClass = "canceled"
				if err = s.Save(ctx, run); err != nil {
					t.Fatal(err)
				}
				if err = s.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				if err = s.QueueInputContinuations(ctx, 1); err != nil {
					t.Fatal(err)
				}
				run, _, err = s.Load(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if status == RunStatusRunning {
					run.Status = status
					if err = s.Claim(ctx, run, RunStatusQueued); err != nil {
						t.Fatal(err)
					}
				}
				if status == RunStatusNeedsAttention {
					run.Status = status
					run.FailureClass = "needs_attention"
					if err = s.Save(ctx, run); err != nil {
						t.Fatal(err)
					}
				}
				if cancelPending {
					if err = s.RequestCancel(ctx, run.ID); err != nil {
						t.Fatal(err)
					}
				}
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				result, deleteErr := tx.ExecContext(ctx, retentionDeleteRun, child.ID, child.ID)
				tx.Rollback()
				if deleteErr != nil {
					t.Fatal(deleteErr)
				}
				if count, _ := result.RowsAffected(); count != 0 {
					t.Fatal("conditional delete used stale eligibility")
				}
				if err = s.Delete(ctx, child.ID); err == nil {
					t.Fatal("consumed child deleted while origin requires continuation", status)
				}
				if gate := inputGate(t, s, run.ID); gate.State != "consumed" || gate.Answer == nil || len(gate.History) != 3 {
					t.Fatal("live-origin history changed", gate)
				}
			})
		}
	}
}
