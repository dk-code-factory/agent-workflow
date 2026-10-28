package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const shutdownInputSource = `jobs:
  ask:
    steps:
      - {id: before, uses: agent/a, with: {prompt: preparation}}
      - {id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}
  after: {needs: ask, steps: [{id: effect, uses: agent/a, with: {prompt: dependent}}]}
`

type shutdownInputAgent struct {
	preparation atomic.Int32
	dependent   atomic.Int32
	failure     atomic.Int32
	holdStarted chan struct{}
}

func (a *shutdownInputAgent) RunAgent(ctx context.Context, req workflows.AgentRequest) (map[string]any, error) {
	switch req.Prompt {
	case "hold":
		close(a.holdStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	case "failure":
		a.failure.Add(1)
		return nil, errors.New("independent provider fixture failure")
	case "preparation":
		a.preparation.Add(1)
	case "dependent":
		a.dependent.Add(1)
	}
	return map[string]any{"text": "done"}, nil
}

// Hold a real journal boundary until the controller cancels its owner. Each
// transaction completes first; no test-only production hooks are involved.
type shutdownInputJournal struct {
	*workflows.SQLiteStore
	boundary string
	reached  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	unblock  func()
	held     atomic.Bool
	ownerCtx context.Context
}

func (j *shutdownInputJournal) hold(ctx context.Context) {
	if !j.held.CompareAndSwap(false, true) {
		return
	}
	close(j.reached)
	select {
	case <-ctx.Done():
		close(j.canceled)
		<-j.release
	case <-j.release:
	}
}
func (j *shutdownInputJournal) EnsureInputGate(ctx context.Context, g workflows.ExternalInputGate) (workflows.ExternalInputGate, error) {
	g, err := j.SQLiteStore.EnsureInputGate(ctx, g)
	if err == nil && j.boundary == "gate" {
		j.hold(ctx)
	}
	return g, err
}
func (j *shutdownInputJournal) ConsumeInput(ctx context.Context, id string) (workflows.ExternalInputGate, error) {
	g, err := j.SQLiteStore.ConsumeInput(ctx, id)
	if err == nil && j.boundary == "checkpoint" {
		j.ownerCtx = ctx
	}
	if err == nil && j.boundary == "consumed" {
		j.hold(ctx)
	}
	return g, err
}
func (j *shutdownInputJournal) Save(ctx context.Context, r *workflows.Run) error {
	if err := j.SQLiteStore.Save(ctx, r); err != nil {
		return err
	}
	if j.boundary == "checkpoint" && r.Status == workflows.RunStatusRunning && r.Jobs["ask"].Status == workflows.RunStatusSucceeded {
		// Save intentionally receives WithoutCancel; the separately retained owner
		// context is captured by ConsumeInput before this checkpoint can be reached.
		j.hold(j.ownerCtx)
	}
	return nil
}

func shutdownInputBarrier(t *testing.T, s *Server, boundary string) *shutdownInputJournal {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	j := &shutdownInputJournal{SQLiteStore: s.Store, boundary: boundary, reached: make(chan struct{}), canceled: make(chan struct{}), release: release, unblock: unblock}
	s.mu.Lock()
	s.Executor.Store = j
	s.mu.Unlock()
	return j
}
func awaitShutdownBoundary(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("input shutdown boundary not reached")
	}
}
func shutdownHeldInput(t *testing.T, c *Client, j *shutdownInputJournal, stop func()) {
	t.Helper()
	if err := c.Call(context.Background(), "POST", "/shutdown", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitShutdownBoundary(t, j.canceled)
	// No other receiver closes release; the owner and its cleanup now finish.
	j.unblock()
	stop()
}

func restartShutdownInput(t *testing.T, old *Server, store *workflows.SQLiteStore, agent workflows.AgentRunner) (*Server, *Client, func()) {
	t.Helper()
	s := &Server{Dir: store.Dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: old.Executor.WorkspaceDir, Store: store, Agents: agent}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("restarted controller did not join")
			}
		})
	}
	t.Cleanup(stop)
	c := NewClient(store.Dir)
	for c.Call(ctx, "GET", "/health", nil, nil) != nil {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	return s, c, stop
}
func protectShutdownInputRetention(t *testing.T, store *workflows.SQLiteStore, root, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.OriginRuns(ctx, id); err == nil {
		t.Fatal("uncanceled input origin eligible for retention")
	}
	if err := store.DeleteOrigin(ctx, id); err == nil {
		t.Fatal("uncanceled input origin deleted")
	}
	if err := store.Delete(ctx, id); err == nil {
		t.Fatal("direct deletion removed recoverable input origin")
	}
	lock, err := Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err = store.BeginRetention(ctx, workflows.Retention{OriginID: id, Root: root}, lock); err == nil {
		t.Fatal("uncanceled input retention planned")
	}
}
func answerShutdownInput(t *testing.T, c *Client, id string, g workflows.ExternalInputGate) {
	t.Helper()
	if err := c.Call(context.Background(), "POST", "/runs/"+id+"/inputs/"+g.ID+"/answer", workflows.ExternalInputAnswer{AnswerID: "shutdown-answer", EvidenceID: g.EvidenceID, Value: json.RawMessage(`"approved"`)}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInputControllerGracefulShutdownRestartsDurableWait(t *testing.T) {
	for _, boundary := range []string{"pending_gate", "accepted_gate", "durable_queued", "consumed_before_dispatch", "consumed_checkpoint", "consumed_child"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			source := shutdownInputSource
			if boundary == "consumed_child" {
				source = `jobs: {review: {uses: workflows/child.yml}}`
			}
			s, c, stop := inputLifecycleFixture(t, source, false)
			if boundary == "consumed_child" {
				if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", "child.yml"), []byte("on: {workflow_call: {}}\n"+shutdownInputSource), 0600); err != nil {
					t.Fatal(err)
				}
			}
			agent := &shutdownInputAgent{holdStarted: make(chan struct{})}
			s.mu.Lock()
			s.Executor.Agents = agent
			s.MaxRuns = 1
			s.mu.Unlock()
			var barrier *shutdownInputJournal
			if boundary != "durable_queued" {
				stage := "gate"
				if boundary == "consumed_before_dispatch" || boundary == "consumed_child" {
					stage = "consumed"
				}
				if boundary == "consumed_checkpoint" {
					stage = "checkpoint"
				}
				barrier = shutdownInputBarrier(t, s, stage)
			} else {
				// Occupy the one controller owner with an actual cancelable execution so
				// the public target admission stays durably queued, never claimed.
				if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", "block.yml"), []byte(`jobs: {hold: {steps: [{uses: agent/a, with: {prompt: hold}}]}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.Call(ctx, "POST", "/runs", StartRequest{Ref: "workflows/block.yml"}, nil); err != nil {
					t.Fatal(err)
				}
				awaitShutdownBoundary(t, agent.holdStarted)
			}
			run := startInput(t, c)
			var original workflows.ExternalInputGate
			if strings.HasPrefix(boundary, "consumed_") {
				awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
				awaitInputOwnerRelease(t, s, run.ID)
				original = controllerGates(t, c, run.ID)[0]
				answerShutdownInput(t, c, run.ID, original)
			}
			if barrier != nil {
				awaitShutdownBoundary(t, barrier.reached)
				original = controllerGates(t, c, run.ID)[0]
				if boundary == "accepted_gate" {
					answerShutdownInput(t, c, run.ID, original)
					original = controllerGates(t, c, run.ID)[0]
				}
				held, _, err := s.Store.Load(ctx, run.ID)
				if err != nil || held.Status != workflows.RunStatusRunning {
					t.Fatal(held, err)
				}
				s.mu.Lock()
				_, active := s.active[run.ID]
				s.mu.Unlock()
				if !active {
					t.Fatal("boundary lost its controller owner")
				}
				if err = s.queueInputContinuations(ctx); err != nil {
					t.Fatal(err)
				}
				after, _, err := s.Store.Load(ctx, run.ID)
				if err != nil || after.Generation != held.Generation {
					t.Fatal("competing continuation admitted before shutdown cleanup", after, err)
				}
				shutdownHeldInput(t, c, barrier, stop)
			} else {
				queued, _, err := s.Store.Load(ctx, run.ID)
				if err != nil || queued.Status != workflows.RunStatusQueued || queued.Owner != "" || agent.preparation.Load() != 0 {
					t.Fatal("target did not remain queued", queued, err)
				}
				if err = c.Call(ctx, "POST", "/shutdown", nil, nil); err != nil {
					t.Fatal(err)
				}
				stop()
			}
			store, err := workflows.OpenSQLite(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			protectShutdownInputRetention(t, store, s.Executor.WorkspaceDir, run.ID)
			if boundary == "consumed_child" {
				if original.RunID == run.ID {
					t.Fatal("nested consumed fixture lost physical child")
				}
				if err := store.Delete(ctx, original.RunID); err == nil {
					t.Fatal("direct deletion removed consumed child of recoverable origin")
				}
			}
			retained, err := store.Inputs(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if boundary != "durable_queued" && (!reflect.DeepEqual(retained[0], original) || agent.preparation.Load() != 1 || agent.dependent.Load() != 0) {
				t.Fatal("shutdown changed gate/checkpoint or dispatched dependent", retained, original)
			}
			restarted, api, stopRestart := restartShutdownInput(t, s, store, agent)
			if boundary == "pending_gate" || boundary == "durable_queued" {
				waiting := awaitStatus(t, api, run.ID, workflows.RunStatusWaiting)
				if waiting.Generation != run.Generation {
					t.Fatal("unanswered shutdown allocated continuation", waiting)
				}
				gate := controllerGates(t, api, run.ID)[0]
				if boundary == "pending_gate" && gate.ID != original.ID {
					t.Fatal("pending gate replaced", gate)
				}
				original = gate
				answerShutdownInput(t, api, run.ID, gate)
			}
			done := awaitStatus(t, api, run.ID, workflows.RunStatusSucceeded)
			awaitInputOwnerRelease(t, restarted, run.ID)
			wantGeneration := run.Generation + 1
			if strings.HasPrefix(boundary, "consumed_") {
				wantGeneration++
			}
			if done.Generation != wantGeneration || agent.preparation.Load() != 1 || agent.dependent.Load() != 1 {
				t.Fatal("shutdown replayed a checkpoint/effect or admitted wrong generation", done, agent.preparation.Load(), agent.dependent.Load())
			}
			final := controllerGates(t, api, run.ID)[0]
			if final.ID != original.ID || final.State != "consumed" || final.Answer == nil || final.Answer.AnswerID != "shutdown-answer" || len(final.History) != 3 {
				t.Fatal("shutdown lost accepted/consumed history", final)
			}
			for i, h := range original.History {
				if !reflect.DeepEqual(final.History[i], h) {
					t.Fatal("retained lifecycle history rewritten", final, original)
				}
			}
			events, err := store.Events(ctx, run.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			admissions := int64(0)
			for _, event := range events {
				if event.Type == "run.resume_admitted" {
					admissions++
				}
			}
			if admissions != wantGeneration-run.Generation {
				t.Fatal("continuation admission was not exactly once", admissions, wantGeneration)
			}
			attempts, err := store.Attempts(ctx, final.RunID)
			if boundary == "consumed_child" && done.Jobs["review"].ChildRunID != final.RunID {
				t.Fatal("restart replaced consumed physical child", done)
			}
			if err != nil || len(attempts) != 2 || attempts[0].StepID != "before" || attempts[0].Number != 1 || attempts[1].StepID != "effect" || attempts[1].Number != 1 {
				t.Fatal("completed effects replayed", attempts, err)
			}
			stopRestart()
		})
	}
}

func TestInputControllerShutdownPreservesExplicitControls(t *testing.T) {
	for _, control := range []string{"cancel", "interrupt"} {
		t.Run(control, func(t *testing.T) {
			ctx := context.Background()
			s, c, stop := inputLifecycleFixture(t, shutdownInputSource, false)
			agent := &shutdownInputAgent{}
			s.mu.Lock()
			s.Executor.Agents = agent
			s.mu.Unlock()
			barrier := shutdownInputBarrier(t, s, "gate")
			run := startInput(t, c)
			awaitShutdownBoundary(t, barrier.reached)
			gate := controllerGates(t, c, run.ID)[0]
			answerShutdownInput(t, c, run.ID, gate)
			if err := c.Call(ctx, "POST", "/runs/"+run.ID+"/"+control, nil, nil); err != nil {
				t.Fatal(err)
			}
			shutdownHeldInput(t, c, barrier, stop)
			store, err := workflows.OpenSQLite(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			if control == "interrupt" {
				protectShutdownInputRetention(t, store, s.Executor.WorkspaceDir, run.ID)
			}
			restarted, api, stopRestart := restartShutdownInput(t, s, store, agent)
			if err = restarted.queueInputContinuations(ctx); err != nil {
				t.Fatal(err)
			}
			want := workflows.RunStatusCanceled
			wantGate := "canceled"
			if control == "interrupt" {
				want = workflows.RunStatusInterrupted
				wantGate = "accepted"
			}
			current := awaitStatus(t, api, run.ID, want)
			retained := controllerGates(t, api, run.ID)[0]
			state, err := store.InputControl(ctx, run.ID)
			if err != nil || current.Generation != run.Generation || retained.ID != gate.ID || retained.State != wantGate || retained.Answer == nil || retained.Answer.AnswerID != "shutdown-answer" || state.Canceled != (control == "cancel") || state.Paused != (control == "interrupt") || agent.preparation.Load() != 1 || agent.dependent.Load() != 0 {
				t.Fatal("shutdown changed explicit control", current, retained, state, err)
			}
			stopRestart()
			if control == "interrupt" {
				protectShutdownInputRetention(t, store, s.Executor.WorkspaceDir, run.ID)
			} else if err = store.DeleteOrigin(ctx, run.ID); err != nil {
				t.Fatal("explicit cancellation no longer permits safe retention", err)
			}
		})
	}
}

func TestInputControllerShutdownCannotBypassUncertaintyOrCleanup(t *testing.T) {
	for _, hazard := range []string{"uncertain", "cleanup"} {
		t.Run(hazard, func(t *testing.T) {
			ctx := context.Background()
			source := shutdownInputSource
			if hazard == "uncertain" {
				source += "  failure: {continue-on-error: true, steps: [{id: lost, uses: agent/a, with: {prompt: failure}}]}\n"
			}
			s, c, stop := inputLifecycleFixture(t, source, hazard == "cleanup")
			agent := &shutdownInputAgent{}
			s.mu.Lock()
			s.Executor.Agents = agent
			s.mu.Unlock()
			barrier := shutdownInputBarrier(t, s, "gate")
			run := startInput(t, c)
			awaitShutdownBoundary(t, barrier.reached)
			gate := controllerGates(t, c, run.ID)[0]
			answerShutdownInput(t, c, run.ID, gate)
			if hazard == "uncertain" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					current, _, err := s.Store.Load(ctx, run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Jobs["failure"].Status == workflows.RunStatusFailed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("real failed attempt did not checkpoint")
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				// Fail an actual StopRun cleanup write, then remove the injected fault
				// before restart. Persisted cleanup uncertainty must still prevent replay.
				db, err := sql.Open("sqlite", filepath.Join(s.Dir, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				worker := workflows.Worker{ID: "shutdown-cleanup", RunID: run.ID, Role: "fixture", Status: "closed"}
				raw, _ := json.Marshal(worker)
				if _, err = db.Exec("INSERT INTO workers(id,run_id,parent_id,role,status,record) VALUES(?,?,'',?,'closed',?)", worker.ID, run.ID, worker.Role, raw); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`CREATE TRIGGER fail_shutdown_cleanup BEFORE UPDATE ON workers WHEN OLD.id='shutdown-cleanup' BEGIN SELECT RAISE(ABORT,'shutdown cleanup remains uncertain'); END`); err != nil {
					t.Fatal(err)
				}
			}
			shutdownHeldInput(t, c, barrier, stop)
			if hazard == "cleanup" {
				db, err := sql.Open("sqlite", filepath.Join(s.Dir, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("DROP TRIGGER fail_shutdown_cleanup")
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			store, err := workflows.OpenSQLite(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			protectShutdownInputRetention(t, store, s.Executor.WorkspaceDir, run.ID)
			restarted, api, stopRestart := restartShutdownInput(t, s, store, agent)
			if err = restarted.queueInputContinuations(ctx); err != nil {
				t.Fatal(err)
			}
			current, _, err := store.Load(ctx, run.ID)
			retained := controllerGates(t, api, run.ID)[0]
			if err != nil || current.Status == workflows.RunStatusRunning || current.Status == workflows.RunStatusQueued || current.Status == workflows.RunStatusSucceeded || current.Generation != run.Generation || retained.ID != gate.ID || retained.State != "accepted" || retained.Answer == nil || agent.preparation.Load() != 1 || agent.dependent.Load() != 0 {
				t.Fatal("shutdown erased uncertainty or consumed answer", current, retained, err)
			}
			if hazard == "uncertain" {
				attempts, err := store.Attempts(ctx, run.ID)
				found := false
				for _, a := range attempts {
					found = found || a.StepID == "lost" && a.Status == workflows.RunStatusFailed
				}
				if err != nil || !found || agent.failure.Load() != 1 {
					t.Fatal("real uncertain attempt lost", attempts, err)
				}
			} else if current.FailureClass != "needs_attention" {
				t.Fatal("cleanup uncertainty lost", current)
			}
			stopRestart()
			protectShutdownInputRetention(t, store, s.Executor.WorkspaceDir, run.ID)
		})
	}
}

// The public Cancel handler commits cancellation before invoking the active
// owner cancellation function. Wrap that function locally to hold this exact
// interval while the accepted nested ConsumeInput executes with a healthy ctx.
type inputCancelBeforeOwnerJournal struct {
	*workflows.SQLiteStore
	origin        string
	committed     <-chan struct{}
	entered       chan struct{}
	denied        chan struct{}
	published     chan struct{}
	healthy       atomic.Bool
	publishedOnce atomic.Bool
	denial        error
}

func (j *inputCancelBeforeOwnerJournal) ConsumeInput(ctx context.Context, id string) (workflows.ExternalInputGate, error) {
	close(j.entered)
	select {
	case <-j.committed:
	case <-ctx.Done():
		return workflows.ExternalInputGate{}, ctx.Err()
	}
	j.healthy.Store(ctx.Err() == nil)
	gate, err := j.SQLiteStore.ConsumeInput(ctx, id)
	j.healthy.Store(j.healthy.Load() && ctx.Err() == nil)
	j.denial = err
	close(j.denied)
	return gate, err
}
func (j *inputCancelBeforeOwnerJournal) Save(ctx context.Context, run *workflows.Run) error {
	if err := j.SQLiteStore.Save(ctx, run); err != nil {
		return err
	}
	if run.ID == j.origin && run.Status == workflows.RunStatusCanceled && j.publishedOnce.CompareAndSwap(false, true) {
		close(j.published)
	}
	return nil
}

func TestInputControllerNestedCancelBeforeOwnerContextCancellation(t *testing.T) {
	ctx := context.Background()
	source := `jobs:
  review: {uses: workflows/child.yml, for-each: {items: [{id: a}, {id: b}], key: item.id}}
  dependent: {needs: review, steps: [{uses: agent/a, with: {prompt: dependent}}]}
`
	s, c, stop := inputLifecycleFixture(t, source, false)
	if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", "child.yml"), []byte("on: {workflow_call: {}}\n"+shutdownInputSource), 0600); err != nil {
		t.Fatal(err)
	}
	agent := &shutdownInputAgent{}
	committed, allowCancel := make(chan struct{}), make(chan struct{})
	var commitOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowCancel) }) }
	t.Cleanup(release)
	journal := &inputCancelBeforeOwnerJournal{SQLiteStore: s.Store, committed: committed, entered: make(chan struct{}), denied: make(chan struct{}), published: make(chan struct{})}
	s.mu.Lock()
	s.Executor.Store = journal
	s.Executor.Agents = agent
	s.mu.Unlock()
	run := startInput(t, c)
	awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
	awaitInputOwnerRelease(t, s, run.ID)
	gate := controllerGates(t, c, run.ID)[0]
	if gate.RunID == run.ID {
		t.Fatal("missing ordered physical child")
	}
	journal.origin = run.ID
	answerShutdownInput(t, c, run.ID, gate)
	awaitShutdownBoundary(t, journal.entered)
	s.mu.Lock()
	ownerCancel, active := s.active[run.ID]
	if active {
		s.active[run.ID] = func() { commitOnce.Do(func() { close(committed) }); <-allowCancel; ownerCancel() }
	}
	s.mu.Unlock()
	if !active {
		t.Fatal("accepted child lost active owner")
	}
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- c.Call(ctx, "POST", "/runs/"+run.ID+"/cancel", nil, nil) }()
	awaitShutdownBoundary(t, committed)
	awaitShutdownBoundary(t, journal.denied)
	awaitShutdownBoundary(t, journal.published)
	if !journal.healthy.Load() || !errors.Is(journal.denial, workflows.ErrExternalInputCanceled) {
		t.Fatal("public cancel interval did not deny consumption under healthy context", journal.denial)
	}
	// Owner context cancellation is still held, so these assertions cannot be
	// satisfied merely by ordinary cancellation checks on later dispatch.
	retained := controllerGates(t, c, run.ID)
	state, err := s.Store.InputControl(ctx, run.ID)
	if err != nil || !state.Canceled || len(retained) != 1 || retained[0].ID != gate.ID || retained[0].State != "canceled" || retained[0].Answer == nil || retained[0].Answer.AnswerID != "shutdown-answer" || len(retained[0].History) != 3 || retained[0].History[2].State != "canceled" || agent.preparation.Load() != 1 || agent.dependent.Load() != 0 {
		t.Fatal("nested public cancel consumed answer or dispatched work", retained, state, err)
	}
	items, err := s.Store.IterationAttempts(ctx, run.ID, "review")
	if err != nil || len(items) < 2 {
		t.Fatal(items, err)
	}
	for _, item := range items {
		if item.Key != "a" || item.Attempt != 1 || item.ChildID != gate.RunID || (item.Status != "pending" && item.Status != "waiting") {
			t.Fatal("cancel denial completed first item or advanced ordered work", items)
		}
	}
	release()
	select {
	case err := <-cancelResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("public Cancel did not finish")
	}
	awaitInputOwnerRelease(t, s, run.ID)
	canceled := awaitStatus(t, c, run.ID, workflows.RunStatusCanceled)
	if err = s.queueInputContinuations(ctx); err != nil {
		t.Fatal(err)
	}
	if err = c.Call(ctx, "POST", "/runs/"+run.ID+"/resume", nil, nil); err == nil {
		t.Fatal("public Resume revived canceled input")
	}
	current, _, err := s.Store.Load(ctx, run.ID)
	if err != nil || current.Generation != canceled.Generation || agent.preparation.Load() != 1 || agent.dependent.Load() != 0 || !reflect.DeepEqual(controllerGates(t, c, run.ID), retained) {
		t.Fatal("canceled nested continuation changed state/effects", current, err)
	}
	stop()
}
