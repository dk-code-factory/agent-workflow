package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func inputFixture(t *testing.T, source string, agent workflows.AgentRunner) (*Server, *Client) {
	t.Helper()
	s, c, _, _ := fixture(t, agent)
	if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return s, c
}
func controllerGates(t *testing.T, c *Client, id string) []workflows.ExternalInputGate {
	t.Helper()
	var gates []workflows.ExternalInputGate
	if err := c.Call(context.Background(), "GET", "/runs/"+id+"/inputs", nil, &gates); err != nil {
		t.Fatal(err)
	}
	return gates
}
func startInput(t *testing.T, c *Client) workflows.Run {
	t.Helper()
	var r workflows.Run
	if err := c.Call(context.Background(), "POST", "/runs", StartRequest{Ref: "workflows/main.yml"}, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestInputControllerCapacityAndAutomaticContinuation(t *testing.T) {
	a := &fakeAgent{started: make(chan struct{}, 4), release: make(chan struct{})}
	close(a.release)
	s, c := inputFixture(t, `jobs:
  ask: {steps: [{id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}]}
  after: {needs: ask, steps: [{uses: agent/a, with: {prompt: after}}]}
`, a)
	// Configure before the first admission; the scheduler reads MaxRuns only when
	// scheduling candidates and fixture has not admitted any yet.
	s.mu.Lock()
	s.MaxRuns = 1
	s.mu.Unlock()
	r := startInput(t, c)
	awaitStatus(t, c, r.ID, workflows.RunStatusWaiting)
	// Another run can reach its own question while the first is suspended.
	other := startInput(t, c)
	awaitStatus(t, c, other.ID, workflows.RunStatusWaiting)
	gates := controllerGates(t, c, r.ID)
	if len(gates) != 1 || a.calls.Load() != 0 {
		t.Fatal(gates, a.calls.Load())
	}
	var health struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := c.Call(context.Background(), "GET", "/health", nil, &health); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range health.Capabilities {
		found = found || v == workflows.DurableExternalInputV1
	}
	if !found {
		t.Fatal(health)
	}
	answer := workflows.ExternalInputAnswer{AnswerID: "a", EvidenceID: "e", Value: json.RawMessage(`{"policy":"approve"}`)}
	var accepted workflows.ExternalInputGate
	path := "/runs/" + r.ID + "/inputs/" + gates[0].ID + "/answer"
	if err := c.Call(context.Background(), "POST", path, answer, &accepted); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "POST", path, answer, nil); err == nil {
		t.Fatal("duplicate answer accepted")
	}
	done := awaitStatus(t, c, r.ID, workflows.RunStatusSucceeded)
	if done.Generation != r.Generation+1 || a.calls.Load() != 1 {
		t.Fatal(done, a.calls.Load())
	}
	if controllerGates(t, c, r.ID)[0].State != "consumed" {
		t.Fatal("answer not observable")
	}
	if err := c.Call(context.Background(), "POST", "/runs/"+other.ID+"/cancel", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, other.ID, workflows.RunStatusCanceled)
	if controllerGates(t, c, other.ID)[0].State != "canceled" {
		t.Fatal("gate survived cancellation")
	}
}
func TestInputControllerInterruptResumeAndAnswerRaces(t *testing.T) {
	s, c := inputFixture(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	r := startInput(t, c)
	awaitStatus(t, c, r.ID, workflows.RunStatusWaiting)
	g := controllerGates(t, c, r.ID)[0]
	if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/interrupt", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, r.ID, workflows.RunStatusInterrupted)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/inputs/"+g.ID+"/answer", workflows.ExternalInputAnswer{AnswerID: fmt.Sprint(i), EvidenceID: "e", Value: json.RawMessage(`false`)}, nil); err == nil {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatal(accepted.Load())
	}
	// Complete the real admission boundary before checking pause protection.
	if err := s.queueInputContinuations(context.Background()); err != nil {
		t.Fatal(err)
	}
	paused := awaitStatus(t, c, r.ID, workflows.RunStatusInterrupted)
	control, err := s.Store.InputControl(context.Background(), r.ID)
	if err != nil || !control.Paused || paused.Generation != r.Generation {
		t.Fatal("admission changed interrupted execution", paused, control, err)
	}
	if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, r.ID, workflows.RunStatusSucceeded)
}
func TestInputControllerUnansweredResumeAndInvalidAnswers(t *testing.T) {
	_, c := inputFixture(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	r := startInput(t, c)
	awaitStatus(t, c, r.ID, workflows.RunStatusWaiting)
	g := controllerGates(t, c, r.ID)[0]
	for _, a := range []any{map[string]any{"answer_id": "a", "evidence_id": "e", "value": nil}, map[string]any{"answer_id": "a", "evidence_id": "wrong", "value": true}, map[string]any{"answer_id": "a", "evidence_id": "e", "value": true, "extra": true}} {
		if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/inputs/"+g.ID+"/answer", a, nil); err == nil {
			t.Fatal("invalid answer accepted", a)
		}
	}
	if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/interrupt", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "POST", "/runs/"+r.ID+"/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	current := awaitStatus(t, c, r.ID, workflows.RunStatusWaiting)
	if current.Generation != r.Generation {
		t.Fatal("unanswered resume allocated owner", current)
	}
}

// Block after the executor publishes its wait, while executeOwner still owns
// its controller entry. This models cleanup after the journal releases owner.
type inputOwnerCleanupJournal struct {
	*workflows.SQLiteStore
	published chan string
	release   <-chan struct{}
	blocked   atomic.Bool
}

func (j *inputOwnerCleanupJournal) Save(ctx context.Context, r *workflows.Run) error {
	if err := j.SQLiteStore.Save(ctx, r); err != nil {
		return err
	}
	if r.Status == workflows.RunStatusWaiting && j.blocked.CompareAndSwap(false, true) {
		j.published <- r.ID
		<-j.release
	}
	return nil
}

func TestInputControllerContinuationWaitsForOwnerCleanup(t *testing.T) {
	for _, control := range []string{"continue", "cancel", "interrupt"} {
		t.Run(control, func(t *testing.T) {
			s, c := inputFixture(t, `jobs: {x: {steps: [{id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			published := make(chan string, 1)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			// Ensure the blocked owner can always finish before fixture shutdown.
			t.Cleanup(unblock)
			s.mu.Lock()
			s.Executor.Store = &inputOwnerCleanupJournal{SQLiteStore: s.Store, published: published, release: release}
			s.mu.Unlock()
			run := startInput(t, c)
			select {
			case id := <-published:
				if id != run.ID {
					t.Fatal(id)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not publish wait")
			}
			gate := controllerGates(t, c, run.ID)[0]
			if err := c.Call(context.Background(), "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", workflows.ExternalInputAnswer{AnswerID: "owner-answer", EvidenceID: "e", Value: json.RawMessage(`true`)}, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.queueInputContinuations(context.Background()); err != nil {
				t.Fatal(err)
			}
			current, _, err := s.Store.Load(context.Background(), run.ID)
			if err != nil || current.Generation != run.Generation || current.Status != workflows.RunStatusWaiting {
				t.Fatal("old owner replaced before cleanup", current, err)
			}
			s.mu.Lock()
			_, active := s.active[run.ID]
			s.mu.Unlock()
			if !active {
				t.Fatal("test lost old owner")
			}
			if control == "cancel" {
				if err = c.Call(context.Background(), "POST", "/runs/"+run.ID+"/cancel", nil, nil); err != nil {
					t.Fatal(err)
				}
				if controllerGates(t, c, run.ID)[0].State != "canceled" {
					t.Fatal("active-owner cancellation lost gate")
				}
			}
			if control == "interrupt" {
				if err = c.Call(context.Background(), "POST", "/runs/"+run.ID+"/interrupt", nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			want := workflows.RunStatusSucceeded
			if control == "cancel" {
				want = workflows.RunStatusCanceled
			}
			if control == "interrupt" {
				want = workflows.RunStatusInterrupted
			}
			completed := awaitStatus(t, c, run.ID, want)
			if control != "continue" {
				awaitInputOwnerRelease(t, s, run.ID)
				if err = s.queueInputContinuations(context.Background()); err != nil {
					t.Fatal(err)
				}
				completed = awaitStatus(t, c, run.ID, want)
			}
			if control == "interrupt" {
				state, err := s.Store.InputControl(context.Background(), run.ID)
				retained := controllerGates(t, c, run.ID)[0]
				if err != nil || !state.Paused || retained.State != "accepted" || retained.Answer.AnswerID != "owner-answer" || len(retained.History) != 2 {
					t.Fatal("pause lost accepted history", state, retained, err)
				}
			}
			wantGeneration := run.Generation + 1
			if control != "continue" {
				wantGeneration = run.Generation
			}
			if completed.Generation != wantGeneration {
				t.Fatal("wrong continuation generation", completed)
			}
		})
	}
}

func TestInputControllerLargeNumberAnswerOutput(t *testing.T) {
	s, c := inputFixture(t, `jobs: {x: {steps: [{id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
	run := startInput(t, c)
	awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
	gate := controllerGates(t, c, run.ID)[0]
	value := json.RawMessage(`{"large":1e400,"integer":9007199254740993}`)
	answer := workflows.ExternalInputAnswer{AnswerID: "large-answer", EvidenceID: "e", Value: value}
	var accepted workflows.ExternalInputGate
	if err := c.Call(context.Background(), "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", answer, &accepted); err != nil {
		t.Fatal(err)
	}
	if string(accepted.Answer.Value) != string(value) {
		t.Fatal("transport changed answer", accepted)
	}
	done := awaitStatus(t, c, run.ID, workflows.RunStatusSucceeded)
	if done.Generation != run.Generation+1 {
		t.Fatal("typed generation changed", done.Generation)
	}
	output, ok := done.Jobs["x"].Steps["q"].Outputs["answer"].(map[string]any)
	if !ok || output["large"] != json.Number("1e400") || output["integer"] != json.Number("9007199254740993") {
		t.Fatal("numeric output changed", output)
	}
	persisted, _, err := s.Store.Load(context.Background(), run.ID)
	if err != nil || persisted.Status != workflows.RunStatusSucceeded {
		t.Fatal(persisted, err)
	}
	consumed := controllerGates(t, c, run.ID)[0]
	if consumed.State != "consumed" || string(consumed.Answer.Value) != string(value) {
		t.Fatal("journal changed numeric answer", consumed)
	}
}

// Owner release happens after executeOwner's StopRun and outcome publication.
func awaitInputOwnerRelease(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		_, active := s.active[id]
		s.mu.Unlock()
		if !active {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("input owner did not finish cleanup")
		}
		time.Sleep(time.Millisecond)
	}
}

// Unlike the ordinary controller fixture, this owner has the real worker
// finalization path enabled before Serve starts. No provider is configured.
func inputLifecycleFixture(t *testing.T, source string, workers bool) (*Server, *Client, func()) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Dir: store.Dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Store: store}}
	if workers {
		s.Workers = &workflows.WorkerManager{Store: store, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
			return nil, nil, nil, errors.New("unexpected provider access")
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("input controller did not stop")
			}
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	c := NewClient(store.Dir)
	for c.Call(ctx, "GET", "/health", nil, nil) != nil {
		if ctx.Err() != nil {
			t.Fatal("worker controller did not start", ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	return s, c, stop
}

func TestInputControllerLateStopRunFailurePreventsContinuation(t *testing.T) {
	s, c, _ := inputLifecycleFixture(t, `jobs: {x: {steps: [{id: q, uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, true)
	published, release := make(chan string, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.mu.Lock()
	s.Executor.Store = &inputOwnerCleanupJournal{SQLiteStore: s.Store, published: published, release: release}
	s.mu.Unlock()
	run := startInput(t, c)
	select {
	case id := <-published:
		if id != run.ID {
			t.Fatal(id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not commit waiting after successful executor cleanup")
	}
	gate := controllerGates(t, c, run.ID)[0]
	answer := workflows.ExternalInputAnswer{AnswerID: "late-cleanup-answer", EvidenceID: "e", Value: json.RawMessage(`true`)}
	if err := c.Call(context.Background(), "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", answer, nil); err != nil {
		t.Fatal(err)
	}
	// Model a retained closed worker. StopRun still verifies/closes its admission
	// after the wait is durable; fail that exact write on this disposable DB.
	db, err := sql.Open("sqlite", filepath.Join(s.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	worker := workflows.Worker{ID: "closed-worker", RunID: run.ID, Status: "closed", Role: "fixture"}
	raw, err := json.Marshal(worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO workers(id,run_id,parent_id,role,status,record) VALUES(?,?,'',?,'closed',?)", worker.ID, run.ID, worker.Role, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_late_worker_cleanup BEFORE UPDATE ON workers WHEN OLD.id='closed-worker' BEGIN SELECT RAISE(ABORT, 'fixture late StopRun write'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.queueInputContinuations(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.Store.Load(context.Background(), run.ID)
	if err != nil || current.Status != workflows.RunStatusWaiting || current.Generation != run.Generation {
		t.Fatal("admitted before late cleanup", current, err)
	}
	unblock()
	awaitInputOwnerRelease(t, s, run.ID)
	failed := awaitStatus(t, c, run.ID, workflows.RunStatusNeedsAttention)
	if failed.Generation != run.Generation || failed.FailureClass != "needs_attention" || !strings.Contains(failed.Error, "fixture late StopRun write") {
		t.Fatal("late StopRun failure lost", failed)
	}
	if _, err = db.Exec("DROP TRIGGER fail_late_worker_cleanup"); err != nil {
		t.Fatal(err)
	}
	if err = s.queueInputContinuations(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _, err = s.Store.Load(context.Background(), run.ID)
	retained := controllerGates(t, c, run.ID)[0]
	if err != nil || current.Status != workflows.RunStatusNeedsAttention || current.Generation != run.Generation || retained.State != "accepted" || retained.Answer.AnswerID != answer.AnswerID || len(retained.History) != 2 {
		t.Fatal("cleanup failure admitted continuation or lost answer history", current, retained, err)
	}
}

func TestInputControllerCanceledOrderedClosureRetention(t *testing.T) {
	for _, protection := range []string{"none", "active", "uncertain", "cleanup"} {
		t.Run(protection, func(t *testing.T) {
			ctx := context.Background()
			s, c, stop := inputLifecycleFixture(t, `jobs: {review: {uses: workflows/child.yml, for-each: {items: [{id: a}, {id: b}], key: item.id}}}`, false)
			childSource := `on: {workflow_call: {}}
jobs:
  accepted: {steps: [{id: q, uses: input/wait, with: {prompt: accepted, evidence-id: e}}]}
  pending: {steps: [{id: q, uses: input/wait, with: {prompt: pending, evidence-id: e}}]}
`
			if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", "child.yml"), []byte(childSource), 0600); err != nil {
				t.Fatal(err)
			}
			run := startInput(t, c)
			awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
			awaitInputOwnerRelease(t, s, run.ID)
			gates := controllerGates(t, c, run.ID)
			if len(gates) != 2 || gates[0].RunID == run.ID || gates[0].RunID != gates[1].RunID {
				t.Fatal("missing ordered suspended child", gates)
			}
			gate := gates[0]
			answer := workflows.ExternalInputAnswer{AnswerID: "retained-answer", EvidenceID: "e", Value: json.RawMessage(`{"approved":true}`)}
			if err := c.Call(ctx, "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", answer, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.DeleteOrigin(ctx, run.ID); err == nil {
				t.Fatal("live ordered wait was not protected")
			}
			child, _, err := s.Store.Load(ctx, gate.RunID)
			if err != nil {
				t.Fatal(err)
			}
			wantChild := workflows.RunStatusCanceled
			if protection == "active" {
				wantChild = workflows.RunStatusRunning
			}
			if protection == "uncertain" {
				wantChild = workflows.RunStatusNeedsAttention
			}
			if protection == "active" || protection == "uncertain" {
				child.Status = wantChild
				if protection == "uncertain" {
					child.FailureClass = "owner_loss"
				}
				if err = s.Store.Save(ctx, child); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.Call(ctx, "POST", "/runs/"+run.ID+"/cancel", nil, nil); err != nil {
				t.Fatal(err)
			}
			canceled := awaitStatus(t, c, run.ID, workflows.RunStatusCanceled)
			if canceled.Generation != run.Generation {
				t.Fatal("cancel allocated continuation", canceled)
			}
			retained := controllerGates(t, c, run.ID)
			for _, g := range retained {
				if g.State != "canceled" {
					t.Fatal("outstanding ordered gate survived cancel", g)
				}
				if g.ID == gate.ID && (g.Answer == nil || string(g.Answer.Value) != string(answer.Value) || len(g.History) != 3 || g.History[0].State != "pending" || g.History[1].State != "accepted" || g.History[2].State != "canceled") {
					t.Fatal("cancel lost accepted history", g)
				}
			}
			// Reopen only after controller and all its owners have stopped.
			stop()
			store, err := workflows.OpenSQLite(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			origin, _, err := store.Load(ctx, run.ID)
			if err != nil || origin.Status != workflows.RunStatusCanceled || origin.Generation != run.Generation {
				t.Fatal("origin retirement was not durable", origin, err)
			}
			child, _, err = store.Load(ctx, gate.RunID)
			if err != nil || child.Status != wantChild {
				t.Fatal("child retirement/protection was not durable", child, err)
			}
			reopened, err := store.Inputs(ctx, run.ID)
			if err != nil || len(reopened) != len(retained) {
				t.Fatal(reopened, err)
			}
			for i, g := range reopened {
				before, _ := json.Marshal(retained[i])
				after, _ := json.Marshal(g)
				if string(before) != string(after) {
					t.Fatal("reopen changed gate history", g)
				}
			}
			items, err := store.IterationAttempts(ctx, run.ID, "review")
			if err != nil || len(items) != 2 {
				t.Fatal("cancel changed ordered item observations", items, err)
			}
			for _, item := range items {
				if item.Key != "a" || item.Attempt != 1 || item.ChildID != gate.RunID {
					t.Fatal("cancel advanced ordered items", item)
				}
			}
			if protection == "cleanup" {
				// A synthetic unresolved invocation has no physical runtime effect,
				// but must keep this canceled closure outside normal retention.
				db, err := sql.Open("sqlite", filepath.Join(s.Dir, "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err = db.Exec("INSERT INTO invocations(id,run_id,generation,state,record,updated) VALUES('unresolved-fixture',?,?,'uncertain','{}',0)", run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.DeleteOrigin(ctx, run.ID); (err != nil) != (protection != "none") {
				t.Fatal("retention did not respect closure protection", protection, err)
			}
			if protection == "none" {
				for _, id := range []string{run.ID, gate.RunID} {
					if _, _, err = store.Load(ctx, id); !errors.Is(err, sql.ErrNoRows) {
						t.Fatal("retention left retired run", id, err)
					}
				}
				remaining, err := store.Inputs(ctx, run.ID)
				if err != nil || len(remaining) != 0 {
					t.Fatal("retention left gate history", remaining, err)
				}
			}
		})
	}
}
