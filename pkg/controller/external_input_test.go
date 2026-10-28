package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	_, c := inputFixture(t, `jobs: {x: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
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
	// A scheduler pass cannot clear an explicit interruption.
	time.Sleep(30 * time.Millisecond)
	awaitStatus(t, c, r.ID, workflows.RunStatusInterrupted)
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
	for _, cancelOwner := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelOwner), func(t *testing.T) {
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
			if cancelOwner {
				if err = c.Call(context.Background(), "POST", "/runs/"+run.ID+"/cancel", nil, nil); err != nil {
					t.Fatal(err)
				}
				if controllerGates(t, c, run.ID)[0].State != "canceled" {
					t.Fatal("active-owner cancellation lost gate")
				}
			}
			unblock()
			want := workflows.RunStatusSucceeded
			if cancelOwner {
				want = workflows.RunStatusCanceled
			}
			completed := awaitStatus(t, c, run.ID, want)
			wantGeneration := run.Generation + 1
			if cancelOwner {
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
