package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

const waitSource = `jobs:
  decide:
    outputs: {value: '${{ steps.question.outputs.answer }}'}
    steps:
      - id: before
        uses: agent/a
        with: {prompt: preparation}
      - id: question
        uses: input/wait
        with: {prompt: Choose a policy, evidence-id: evidence-v1, payload: {choices: [a, b]}}
      - id: after
        uses: agent/a
        with: {prompt: '${{ steps.question.outputs.answer }}'}
  dependent:
    needs: decide
    steps: [{uses: agent/a, with: {prompt: dependent}}]
`

func inputGate(t *testing.T, s *SQLiteStore, id string) ExternalInputGate {
	t.Helper()
	gates, err := s.Inputs(context.Background(), id)
	if err != nil || len(gates) == 0 {
		t.Fatalf("gates: %+v %v", gates, err)
	}
	return gates[len(gates)-1]
}
func answerGate(t *testing.T, s *SQLiteStore, g ExternalInputGate) {
	t.Helper()
	if _, err := s.AcceptInput(context.Background(), g.OriginID, g.ID, ExternalInputAnswer{AnswerID: "answer-" + g.ID, EvidenceID: g.EvidenceID, Value: json.RawMessage(`"approved"`)}); err != nil {
		t.Fatal(err)
	}
}
func TestExternalInputWaitResumeAndCompletedAttempts(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	e, s := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{}, nil
	}))
	e.Slots = make(chan struct{}, 1)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusWaiting || !run.FinishedAt.IsZero() || calls.Load() != 1 || len(e.Slots) != 0 {
		t.Fatalf("wait %+v %v calls=%d", run, err, calls.Load())
	}
	if _, ok := run.Jobs["dependent"]; ok {
		t.Fatal("dependent ran before answer")
	}
	if err = s.DeleteOrigin(ctx, run.ID); err == nil {
		t.Fatal("waiting run deleted")
	}
	g := inputGate(t, s, run.ID)
	if g.State != "pending" || g.Generation != run.Generation || g.RunID != run.ID {
		t.Fatal(g)
	}
	// Resume with no answer stays suspended and does not create another execution.
	same, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || same.Status != RunStatusWaiting || same.Generation != run.Generation {
		t.Fatalf("pending resume %+v %v", same, err)
	}
	answerGate(t, s, g)
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	queued, _, _ := s.Load(ctx, run.ID)
	if queued.Status != RunStatusQueued || queued.Generation != run.Generation+1 {
		t.Fatal(queued)
	}
	done, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 3 || done.Jobs["decide"].Outputs["value"] != "approved" {
		t.Fatalf("resume %+v %v calls=%d", done, err, calls.Load())
	}
	accepted := inputGate(t, s, run.ID)
	if accepted.State != "consumed" || len(accepted.History) != 3 || accepted.Answer.AnswerID != "answer-"+g.ID {
		t.Fatal(accepted)
	}
	if _, err = s.AcceptInput(ctx, run.ID, g.ID, *accepted.Answer); !errors.Is(err, ErrExternalInput) {
		t.Fatal("duplicate accepted", err)
	}
}
func TestExternalInputConcurrentAnswersAndCancel(t *testing.T) {
	ctx := context.Background()
	for _, cancelRace := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRace), func(t *testing.T) {
			e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: question, evidence-id: e}}]}}`, nil)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			g := inputGate(t, s, run.ID)
			if _, err = s.AcceptInput(ctx, run.ID, g.ID, ExternalInputAnswer{"stale", "other", json.RawMessage(`true`)}); !errors.Is(err, ErrExternalInput) {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var accepted atomic.Int32
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if _, err := s.AcceptInput(ctx, run.ID, g.ID, ExternalInputAnswer{fmt.Sprint(i), "e", json.RawMessage(`true`)}); err == nil {
						accepted.Add(1)
					}
				}(i)
			}
			if cancelRace {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := s.RequestCancel(ctx, run.ID); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if accepted.Load() > 1 || (!cancelRace && accepted.Load() != 1) {
				t.Fatal("accept count", accepted.Load())
			}
			if cancelRace {
				if inputGate(t, s, run.ID).State != "canceled" {
					t.Fatal("cancel did not invalidate gate")
				}
				if err = s.QueueInputContinuations(ctx, 128); err != nil {
					t.Fatal(err)
				}
				r, _, _ := s.Load(ctx, run.ID)
				if r.Status == RunStatusQueued {
					t.Fatal("canceled answer continued")
				}
			}
		})
	}
}
func TestExternalInputMultipleGatesPauseAndIdentity(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs:
  a: {steps: [{uses: input/wait, with: {prompt: a, evidence-id: e}}]}
  b: {steps: [{uses: input/wait, with: {prompt: b, evidence-id: e}}]}
`, nil)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	gates, _ := s.Inputs(ctx, run.ID)
	if len(gates) != 2 {
		t.Fatal(gates)
	}
	answerGate(t, s, gates[0])
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	r, _, _ := s.Load(ctx, run.ID)
	if r.Status != RunStatusWaiting {
		t.Fatal(r)
	}
	if _, err = s.AcceptInput(ctx, run.ID, gates[1].ID, ExternalInputAnswer{"answer-" + gates[0].ID, "e", json.RawMessage(`true`)}); !errors.Is(err, ErrExternalInput) {
		t.Fatal("reused answer ID", err)
	}
	if err = s.InterruptInput(ctx, run.ID, run.Generation); err != nil {
		t.Fatal(err)
	}
	answerGate(t, s, gates[1])
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.Load(ctx, run.ID)
	if r.Status != RunStatusInterrupted {
		t.Fatal(r)
	}
	done, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || done.Status != RunStatusSucceeded {
		t.Fatalf("%+v %v", done, err)
	}
}
func TestExternalInputAnswerDuringFinalization(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	e.BeforeFinalize = func(ctx context.Context, id string) error {
		g := inputGate(t, s, id)
		answerGate(t, s, g)
		if err := s.QueueInputContinuations(ctx, 128); err != nil {
			return err
		}
		r, _, err := s.Load(ctx, id)
		if err == nil && r.Status != RunStatusRunning {
			t.Error("owner replaced during cleanup", r)
		}
		return err
	}
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusWaiting {
		t.Fatalf("%+v %v", run, err)
	}
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	e.BeforeFinalize = nil
	if run, err = e.Resume(ctx, run.ID, nil, false); err != nil || run.Status != RunStatusSucceeded {
		t.Fatal(run, err)
	}
}
func TestExternalInputRestartBoundaries(t *testing.T) {
	for _, boundary := range []string{"gate", "accepted", "queued", "consumed"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, s := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			g := inputGate(t, s, run.ID)
			if boundary != "gate" {
				answerGate(t, s, g)
			}
			if boundary == "queued" || boundary == "consumed" {
				if err = s.QueueInputContinuations(ctx, 128); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "gate" || boundary == "consumed" {
				r, _, _ := s.Load(ctx, run.ID)
				previous := r.Status
				r.Status = RunStatusRunning
				if err = s.Claim(ctx, r, previous); err != nil {
					t.Fatal(err)
				}
				if boundary == "consumed" {
					if _, err = s.ConsumeInput(fenceContext(ctx, r), g.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
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
			if err = reopened.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			r, _, _ := reopened.Load(ctx, run.ID)
			if boundary != "queued" && r.Status != RunStatusWaiting {
				t.Fatalf("recovered %+v", r)
			}
			if boundary == "gate" {
				answerGate(t, reopened, inputGate(t, reopened, run.ID))
			}
			if err = reopened.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			done, err := e.Resume(ctx, run.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 3 {
				t.Fatalf("%+v %v calls=%d", done, err, calls.Load())
			}
		})
	}
}
func TestExternalInputNestedRepeatAndOrderedItems(t *testing.T) {
	for _, mode := range []string{"nested", "repeat", "ordered"} {
		t.Run(mode, func(t *testing.T) {
			source := `jobs: {review: {uses: workflows/child.yml}}`
			if mode == "repeat" {
				source = `jobs: {review: {uses: workflows/child.yml, repeat: {max-attempts: 2, until: 'iteration.number == 2'}}}`
			}
			if mode == "ordered" {
				source = `jobs: {review: {uses: workflows/child.yml, for-each: {items: [{id: a}, {id: b}], key: item.id}}}`
			}
			var calls atomic.Int32
			e, s := durableExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\n"+waitSource[:len(waitSource)-len("  dependent:\n    needs: decide\n    steps: [{uses: agent/a, with: {prompt: dependent}}]\n")])
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusWaiting {
				t.Fatalf("first %+v %v", run, err)
			}
			want := 1
			if mode != "nested" {
				want = 2
			}
			for i := 0; i < want; i++ {
				gates, _ := s.Inputs(context.Background(), run.ID)
				for _, g := range gates {
					if g.State == "pending" {
						answerGate(t, s, g)
					}
				}
				run, err = e.Resume(context.Background(), run.ID, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				if i < want-1 && run.Status != RunStatusWaiting {
					t.Fatal(run)
				}
			}
			if run.Status != RunStatusSucceeded || calls.Load() != int32(want*2) {
				t.Fatalf("%+v calls=%d", run, calls.Load())
			}
			if mode == "ordered" {
				records, err := s.IterationAttempts(context.Background(), run.ID, "review")
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range records {
					if r.Attempt != 1 {
						t.Fatal("waiting allocated new item", r)
					}
				}
			}
		})
	}
}
func TestExternalInputRequiresExtensionAndValidFields(t *testing.T) {
	e, _ := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	e.Store = FileStore{Dir: t.TempDir()}
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil {
		t.Fatal("wait admitted on compatibility store")
	}
	for _, with := range []string{`{prompt: q}`, `{prompt: '', evidence-id: e}`, `{prompt: q, evidence-id: 42}`, `{prompt: q, evidence-id: e, extra: true}`} {
		w, err := Parse([]byte("jobs: {x: {steps: [{uses: input/wait, with: " + with + "}]}}"))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Fatal(with)
		}
	}
}
func TestExternalInputV10MigrationAndRestore(t *testing.T) {
	for _, state := range []string{"pending", "accepted"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			if _, err := s.db.Exec("DROP TABLE input_gates; DROP TABLE input_controls; PRAGMA user_version=9"); err != nil {
				t.Fatal(err)
			}
			lock, err := ownership.Own(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Migrate(ctx, lock); err != nil {
				t.Fatal(err)
			}
			lock.Close()
			if _, err = os.Stat(filepath.Join(s.Dir, "state.pre-v10.db")); err != nil {
				t.Fatal(err)
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			if state == "accepted" {
				answerGate(t, s, inputGate(t, s, run.ID))
			}

			lock, err = ownership.Own(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			backup := filepath.Join(t.TempDir(), "backup.db")
			if err = s.Backup(ctx, backup, lock); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = RestoreDatabase(ctx, s.Dir, backup, lock); err != nil {
				t.Fatal(err)
			}
			restored, err := OpenSQLite(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			g := inputGate(t, restored, run.ID)
			if g.State != state || (state == "accepted" && g.Answer == nil) {
				t.Fatal("answer lost", g)
			}
			recovered, _, err := restored.Load(ctx, run.ID)
			if err != nil || recovered.Status != RunStatusNeedsAttention || recovered.FailureClass != "restore_uncertainty" {
				t.Fatal(recovered, err)
			}
			if err = restored.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			recovered, _, _ = restored.Load(ctx, run.ID)
			if recovered.Status == RunStatusQueued {
				t.Fatal("restored run continued without acknowledgement")
			}
			if _, err = restored.QueueResume(ctx, run.ID, "", false, 128); err == nil {
				t.Fatal("restore quarantine cleared without acknowledgement")
			}
			if state == "pending" {
				answerGate(t, restored, g)
			}
			if err = restored.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			recovered, _, _ = restored.Load(ctx, run.ID)
			if recovered.Status != RunStatusNeedsAttention || recovered.FailureClass != "restore_uncertainty" || recovered.Generation != run.Generation {
				t.Fatal("restore quarantine lost", recovered)
			}
			if _, err = restored.QueueResume(ctx, run.ID, "", true, 128); err != nil {
				t.Fatal("explicit acknowledgement failed", err)
			}
			e.Store = restored
			done, err := e.Resume(ctx, run.ID, nil, true)
			if err != nil || done.Status != RunStatusSucceeded {
				t.Fatalf("acknowledged restore %+v %v", done, err)
			}
		})
	}
}

func TestExternalInputEvidenceDriftBlocksConsumption(t *testing.T) {
	for _, drift := range []string{"evidence", "binding", "implementation"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, s := durableExecutor(t, actionSource+`      - id: q
        uses: input/wait
        with: {prompt: q, evidence-id: pinned}
      - uses: action/capture
        with: {value: after}
`, nil)
			spec := readSpec(&calls)
			registerTestAction(t, e, spec)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != RunStatusWaiting {
				t.Fatal(run, err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			switch drift {
			case "implementation":
				spec.Implementation = "changed"
			case "binding":
				spec.Bindings = func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
					return json.RawMessage(`{"repository":"changed"}`), nil
				}
			case "evidence":
				spec.ValidateCheckpoint = func(context.Context, ActionContext, ActionCheckpoint[actionInput, actionOutput]) error {
					return errors.New("pinned evidence changed")
				}
			}
			registerTestAction(t, e, spec)
			result, err := e.Resume(ctx, run.ID, nil, false)
			if !errors.Is(err, ErrActionIntegrity) || result == nil || result.Status != RunStatusNeedsAttention || calls.Load() != 1 {
				t.Fatalf("drift dispatched %+v %v calls=%d", result, err, calls.Load())
			}
			if inputGate(t, s, run.ID).State != "accepted" {
				t.Fatal("answer consumed before evidence validation")
			}
		})
	}
}

func TestExternalInputNestedOrderedWaitReusesPhysicalItem(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs: {review: {uses: workflows/wrapper.yml, for-each: {items: [{id: a}, {id: b}], key: item.id}}}`, nil)
	writeWorkflow(t, e.WorkspaceDir, "wrapper.yml", `on: {workflow_call: {}}
jobs: {nested: {uses: workflows/child.yml}}
`)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {ask: {steps: [{id: first, uses: input/wait, with: {prompt: first, evidence-id: e}}, {id: second, uses: input/wait, with: {prompt: second, evidence-id: e}}]}}
`)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusWaiting {
		t.Fatal(run, err)
	}
	for i := 0; i < 4; i++ {
		gates, err := s.Inputs(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		pending := 0
		for _, g := range gates {
			if g.State == "pending" {
				pending++
				answerGate(t, s, g)
			}
		}
		if pending != 1 {
			t.Fatal(gates)
		}
		run, err = e.Resume(ctx, run.ID, nil, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.Status != RunStatusSucceeded {
		t.Fatal(run)
	}
	records, err := s.IterationAttempts(ctx, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Attempt != 1 {
			t.Fatal("nested wait allocated fresh item", r)
		}
	}
}

func TestExternalInputDrainsIndependentWorkAndActiveInterrupt(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupt), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			e, s := durableExecutor(t, `jobs:
  question: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}
  independent: {steps: [{uses: agent/a, with: {prompt: independent}}]}
`, agentFunc(func(ctx context.Context, _ AgentRequest) (map[string]any, error) {
				close(started)
				select {
				case <-release:
					return map[string]any{}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}))
			e.Slots = make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				run *Run
				err error
			}
			done := make(chan result, 1)
			go func() { r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"}); done <- result{r, err} }()
			<-started
			var gate ExternalInputGate
			deadline := time.After(5 * time.Second)
			for gate.ID == "" {
				runs, err := s.List(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range runs {
					gates, err := s.Inputs(context.Background(), r.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(gates) > 0 {
						gate = gates[0]
					}
				}
				select {
				case <-deadline:
					t.Fatal("question not committed")
				default:
				}
			}
			if len(e.Slots) != 1 {
				t.Fatal("independent invocation did not own capacity")
			}
			select {
			case <-done:
				t.Fatal("wait returned before independent cleanup")
			default:
			}
			if interrupt {
				if err := s.InterruptInput(context.Background(), gate.OriginID, gate.Generation); err != nil {
					t.Fatal(err)
				}
				cancel()
			} else {
				answerGate(t, s, gate)
				if err := s.QueueInputContinuations(context.Background(), 128); err != nil {
					t.Fatal(err)
				}
				active, _, _ := s.Load(context.Background(), gate.OriginID)
				if active.Status != RunStatusRunning || active.Generation != gate.Generation {
					t.Fatal("competing owner admitted", active)
				}
				close(release)
			}
			r := <-done
			expected := RunStatusWaiting
			if interrupt {
				expected = RunStatusInterrupted
			}
			if r.err != nil || r.run.Status != expected || len(e.Slots) != 0 {
				t.Fatal(r.run, r.err)
			}
			if interrupt && inputGate(t, s, r.run.ID).State != "pending" {
				t.Fatal("interrupt invalidated question")
			}
		})
	}
}

// lateInputControlJournal delivers a control after the executor's final control
// observation, immediately before its outcome reaches the journal transaction.
type lateInputControlJournal struct {
	*SQLiteStore
	beforeSave func(context.Context, *Run) error
}

func (s *lateInputControlJournal) Save(ctx context.Context, r *Run) error {
	if s.beforeSave != nil && r.Status != RunStatusRunning && r.Status != RunStatusQueued {
		before := s.beforeSave
		s.beforeSave = nil
		if err := before(ctx, r); err != nil {
			return err
		}
	}
	return s.SQLiteStore.Save(ctx, r)
}

func TestExternalInputLateControlPublication(t *testing.T) {
	for _, outcome := range []string{"waiting", "completed", "uncertain"} {
		for _, control := range []string{"interrupt", "cancel"} {
			t.Run(outcome+"/"+control, func(t *testing.T) {
				ctx := context.Background()
				e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
				wrapper := &lateInputControlJournal{SQLiteStore: s}
				e.Store = wrapper
				var id string
				var generation int64 = 1
				if outcome == "completed" {
					r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
					if err != nil {
						t.Fatal(err)
					}
					id = r.ID
					answerGate(t, s, inputGate(t, s, id))
					generation++
				}
				if outcome == "uncertain" {
					e.BeforeFinalize = func(context.Context, string) error { return ErrActionIntegrity }
				}
				wrapper.beforeSave = func(ctx context.Context, r *Run) error {
					id = r.ID
					// Retain an accepted answer when interruption arrives during suspension.
					if outcome == "waiting" {
						answerGate(t, s, inputGate(t, s, id))
					}
					if control == "interrupt" {
						return s.InterruptInput(ctx, id, r.Generation)
					}
					return s.RequestCancelGeneration(ctx, id, r.Generation)
				}
				var run *Run
				var err error
				if outcome == "completed" {
					run, err = e.Resume(ctx, id, nil, false)
				} else {
					run, err = e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				}
				want := RunStatusInterrupted
				if control == "cancel" {
					want = RunStatusCanceled
				}
				if outcome == "uncertain" {
					want = RunStatusNeedsAttention
				}
				if run == nil || run.Status != want || (outcome != "uncertain" && err != nil) || (outcome == "uncertain" && !errors.Is(err, ErrActionIntegrity)) {
					t.Fatalf("outcome %+v %v", run, err)
				}
				persisted, _, loadErr := s.Load(ctx, id)
				if loadErr != nil || persisted.Status != want || persisted.Generation != generation {
					t.Fatalf("published %+v %v", persisted, loadErr)
				}
				if control == "interrupt" && outcome != "uncertain" && !persisted.FinishedAt.IsZero() {
					t.Fatal("interrupted outcome is terminal", persisted)
				}
				gate := inputGate(t, s, id)
				if outcome == "waiting" && control == "interrupt" && (gate.State != "accepted" || len(gate.History) != 2 || gate.Answer == nil) {
					t.Fatal("interruption lost answer", gate)
				}
				if control == "cancel" && outcome != "completed" && gate.State != "canceled" {
					t.Fatal("cancel retained outstanding gate", gate)
				}
				if err = s.QueueInputContinuations(ctx, 128); err != nil {
					t.Fatal(err)
				}
				persisted, _, _ = s.Load(ctx, id)
				if persisted.Status != want || persisted.Generation != generation {
					t.Fatal("late control admitted continuation", persisted)
				}
			})
		}
	}
	// Once completion commits, an interruption must be rejected.
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	answerGate(t, s, inputGate(t, s, run.ID))
	run, err = e.Resume(ctx, run.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InterruptInput(ctx, run.ID, run.Generation); !errors.Is(err, ErrExternalInput) {
		t.Fatal("terminal interruption accepted", err)
	}
}

func TestExternalInputRecoveryPublicationRollback(t *testing.T) {
	for _, state := range []string{"accepted", "consumed"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, s := durableExecutor(t, waitSource, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			}))
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			gate := inputGate(t, s, run.ID)
			answerGate(t, s, gate)
			if err = s.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			run, _, err = s.Load(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			previous := run.Status
			run.Status = RunStatusRunning
			if err = s.Claim(ctx, run, previous); err != nil {
				t.Fatal(err)
			}
			if state == "consumed" {
				if _, err = s.ConsumeInput(fenceContext(ctx, run), gate.ID); err != nil {
					t.Fatal(err)
				}
			}
			// Fail reconstruction after the waiting checkpoint has been written inside
			// recovery. Both writes must roll back, retaining an owner-loss candidate.
			if _, err = s.db.Exec(`CREATE TRIGGER fail_input_recovery BEFORE UPDATE OF continuation ON input_controls WHEN NEW.continuation=1 BEGIN SELECT RAISE(ABORT,'recovery fault'); END`); err != nil {
				t.Fatal(err)
			}
			if err = s.Recover(ctx); err == nil {
				t.Fatal("recovery fault not observed")
			}
			r, _, err := s.Load(ctx, run.ID)
			if err != nil || r.Status != RunStatusNeedsAttention || r.FailureClass != "owner_loss" {
				t.Fatalf("stranded recovery %+v %v", r, err)
			}
			control, err := s.InputControl(ctx, run.ID)
			if err != nil || control.Continuation {
				t.Fatalf("intent failed to roll back %+v %v", control, err)
			}
			g := inputGate(t, s, run.ID)
			if g.State != state || g.Answer == nil {
				t.Fatal("answer/history changed", g)
			}
			events, err := s.Events(ctx, run.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			waiting := 0
			for _, event := range events {
				if event.Type == "run.waiting" {
					waiting++
				}
			}
			if waiting != 1 {
				t.Fatal("failed recovery published waiting event", waiting)
			}
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
			if _, err = reopened.db.Exec("DROP TRIGGER fail_input_recovery"); err != nil {
				t.Fatal(err)
			}
			if err = reopened.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if err = reopened.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			if err = reopened.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			done, err := e.Resume(ctx, run.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || done.Generation != run.Generation+1 || calls.Load() != 3 {
				t.Fatalf("recovered %+v %v calls=%d", done, err, calls.Load())
			}
		})
	}
}

func TestExternalInputCanceledNestedRetention(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {review: {uses: workflows/child.yml}}`, nil)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			gate := inputGate(t, s, run.ID)
			if gate.RunID == run.ID {
				t.Fatal("missing nested fixture")
			}
			if uncertain {
				child, _, err := s.Load(ctx, gate.RunID)
				if err != nil {
					t.Fatal(err)
				}
				child.Status, child.FailureClass = RunStatusNeedsAttention, "owner_loss"
				if err = s.Save(ctx, child); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.RequestCancelGeneration(ctx, run.ID, run.Generation); err != nil {
				t.Fatal(err)
			}
			child, _, err := s.Load(ctx, gate.RunID)
			want := RunStatusCanceled
			if uncertain {
				want = RunStatusNeedsAttention
			}
			if err != nil || child.Status != want {
				t.Fatal("child outcome", child, err)
			}
			if inputGate(t, s, run.ID).State != "canceled" {
				t.Fatal("gate not invalidated")
			}
			if err = s.DeleteOrigin(ctx, run.ID); (err != nil) != uncertain {
				t.Fatal("retention", err)
			}
		})
	}
}

func TestExternalInputPendingResumePreservesUncertainty(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Begin(ctx, run.ID, "independent", "effect", 1, false); err != nil {
		t.Fatal(err)
	}
	if err = s.InterruptInput(ctx, run.ID, run.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueResume(ctx, run.ID, "", false, 128); err == nil {
		t.Fatal("pending gate bypassed uncertain attempt")
	}
	r, _, err := s.Load(ctx, run.ID)
	control, controlErr := s.InputControl(ctx, run.ID)
	if err != nil || controlErr != nil || r.Status != RunStatusInterrupted || !control.Paused || r.Generation != run.Generation {
		t.Fatalf("failed resume mutated state %+v %+v %v %v", r, control, err, controlErr)
	}
	answerGate(t, s, inputGate(t, s, run.ID))
	if err = s.QueueInputContinuations(ctx, 128); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.Load(ctx, run.ID)
	if r.Status != RunStatusInterrupted || r.Generation != run.Generation {
		t.Fatal("uncertain run automatically admitted", r)
	}
}
