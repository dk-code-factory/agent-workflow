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
