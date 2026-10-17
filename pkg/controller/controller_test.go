package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestResumeAdmissionRecordsUnavailableCredentials(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, client, _, _ := fixture(t, agent)
	ctx := context.Background()
	run, err := s.Executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml", SecretRefs: map[string]string{"key": "WORKFLOW_RESUME_MISSING_TEST_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusFailed
	if err = s.Store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	var handle workflows.Run
	if err = client.Call(ctx, "POST", "/runs/"+run.ID+"/resume?request_id=credential-failure", nil, &handle); err != nil {
		t.Fatal(err)
	}
	if handle.Generation != 2 || handle.Status != workflows.RunStatusQueued {
		t.Fatalf("resume was not durable before acknowledgement: %+v", handle)
	}
	result := awaitStatus(t, client, run.ID, workflows.RunStatusNeedsAttention)
	if result.Error == "" || result.FailureClass != "authentication" {
		t.Fatalf("credential failure lost: %+v", result)
	}
	if agent.calls.Load() != 0 {
		t.Fatal("effect ran without prerequisites")
	}
}

func TestCancellationBeforeClaimRetainsCanceledOutcome(t *testing.T) {
	store, err := workflows.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "0123456789abcdef0123456789abcdef"
	run := &workflows.Run{ID: id, Status: workflows.RunStatusQueued}
	if err = store.Admit(context.Background(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err = store.RequestCancelGeneration(context.Background(), id, run.Generation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{Store: store, active: map[string]context.CancelFunc{id: cancel}, wake: make(chan struct{}, 1)}
	s.wg.Add(1)
	s.executeOwner(ctx, cancel, id, run.Generation)
	s.wg.Wait()
	result, _, err := store.Load(context.Background(), id)
	if err != nil || result.Status != workflows.RunStatusCanceled || result.FailureClass != "canceled" {
		t.Fatalf("pre-claim cancellation misclassified: %+v %v", result, err)
	}
}

func TestSimultaneousResumeRequestsShareOneExecution(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, client, _, _ := fixture(t, agent)
	ctx := context.Background()
	run, err := s.Executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusFailed
	if err = s.Store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var handle workflows.Run
			err := client.Call(ctx, "POST", "/runs/"+run.ID+"/resume?request_id=one", nil, &handle)
			if err == nil && handle.Generation != 2 {
				t.Errorf("wrong generation: %+v", handle)
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	close(agent.release)
	awaitStatus(t, client, run.ID, workflows.RunStatusSucceeded)
	if agent.calls.Load() != 1 {
		t.Fatal("resume launched duplicate effects")
	}
}

type fakeAgent struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (f *fakeAgent) RunAgent(ctx context.Context, _ workflows.AgentRequest) (map[string]any, error) {
	f.calls.Add(1)
	select {
	case f.started <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		return map[string]any{"text": "ok"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func fixture(t *testing.T, agent workflows.AgentRunner) (*Server, *Client, context.CancelFunc, <-chan error) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {x: {steps: [{uses: agent/a, with: {prompt: hello}}]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".agent-workflow")
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Dir: dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Agents: agent, Store: store, MaxParallel: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("controller did not stop")
		}
		_ = store.Close()
	})
	client := NewClient(dir)
	deadline := time.Now().Add(5 * time.Second)
	for client.Call(ctx, "GET", "/health", nil, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("controller did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return server, client, cancel, done
}
func awaitStatus(t *testing.T, c *Client, id, status string) workflows.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var r workflows.Run
		if err := c.Call(context.Background(), "GET", "/runs/"+id, nil, &r); err != nil {
			t.Fatal(err)
		}
		if r.Status == status {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status %s; want %s: %+v", r.Status, status, r)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestDurableAdmissionSurvivesRequestCancellation(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, c, _, _ := fixture(t, agent)
	ctx, cancel := context.WithCancel(context.Background())
	var admitted workflows.Run
	if err := c.Call(ctx, "POST", "/runs", StartRequest{Ref: "workflows/main.yml"}, &admitted); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-agent.started:
	case <-time.After(5 * time.Second):
		t.Fatal("work did not start")
	}
	if _, _, err := s.Store.Load(context.Background(), admitted.ID); err != nil {
		t.Fatal("acknowledged run was lost")
	}
	close(agent.release)
	awaitStatus(t, c, admitted.ID, workflows.RunStatusSucceeded)
	if agent.calls.Load() != 1 {
		t.Fatal("duplicate execution")
	}
}
func TestCancellationAndCrossRunArtifactScope(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	_, c, _, _ := fixture(t, agent)
	var admitted workflows.Run
	if err := c.Call(context.Background(), "POST", "/runs", StartRequest{Ref: "workflows/main.yml"}, &admitted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-agent.started:
	case <-time.After(5 * time.Second):
		t.Fatal("work did not start")
	}
	if err := c.Call(context.Background(), "POST", "/runs/"+admitted.ID+"/cancel", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, admitted.ID, workflows.RunStatusCanceled)
	if err := c.Call(context.Background(), "GET", "/runs/"+admitted.ID+"/artifacts/not-owned", nil, nil); err == nil {
		t.Fatal("unscoped artifact returned")
	}
}
func TestExclusiveOwnerAndPrivateSocket(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, _, _, _ := fixture(t, agent)
	if lock, err := Own(s.Dir); err == nil {
		_ = lock.Close()
		t.Fatal("competing owner admitted")
	}
	address, err := socketAddress(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(address)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("socket not private")
	}
}
func TestControllerRejectsUnavailableSecretBeforeAdmission(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, c, _, _ := fixture(t, agent)
	if err := c.Call(context.Background(), "POST", "/runs", StartRequest{Ref: "workflows/main.yml", SecretRefs: map[string]string{"token": "AGENT_WORKFLOW_NONEXISTENT_TEST_SECRET"}}, nil); err == nil {
		t.Fatal("missing secret admitted")
	}
	runs, err := s.Store.List(context.Background())
	if err != nil || len(runs) != 0 {
		b, _ := json.Marshal(runs)
		t.Fatalf("invalid admission persisted %s %v", b, err)
	}
}

func TestAdmissionRequestIDDeduplication(t *testing.T) {
	agent := &fakeAgent{started: make(chan struct{}, 2), release: make(chan struct{})}
	_, c, _, _ := fixture(t, agent)
	input := StartRequest{RequestID: "same-request", Ref: "workflows/main.yml"}
	var one, two workflows.Run
	if err := c.Call(context.Background(), "POST", "/runs", input, &one); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "POST", "/runs", input, &two); err != nil {
		t.Fatal(err)
	}
	if one.ID != two.ID {
		t.Fatal("acknowledged admission was duplicated")
	}
	input.Inputs = map[string]any{"changed": true}
	if err := c.Call(context.Background(), "POST", "/runs", input, nil); err == nil {
		t.Fatal("request ID content conflict accepted")
	}
	close(agent.release)
	awaitStatus(t, c, one.ID, workflows.RunStatusSucceeded)
	if agent.calls.Load() != 1 {
		t.Fatal("duplicate execution")
	}
}
