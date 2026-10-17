package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapacityLeaseParallelReacquisitionCancellationAndRelease(t *testing.T) {
	ctx := context.Background()
	pool := &Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 1}
	parent, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = parent.Suspend(); err != nil {
		t.Fatal(err)
	}
	child, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Suspend(); err == nil {
		t.Fatal("suspended limit not enforced")
	}
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- parent.Reacquire(waitCtx) }()
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reacquisition ignored cancellation")
	}
	child.Release()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := parent.Reacquire(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(pool.Slots) != 1 || pool.Suspended() != 0 {
		t.Fatal("parallel reacquisition leaked capacity")
	}
	if err = parent.Suspend(); err != nil {
		t.Fatal(err)
	}
	child, _ = pool.Acquire(ctx)
	done = make(chan error, 1)
	go func() { done <- parent.Reacquire(ctx) }()
	parent.Release() // Release must also cancel an in-progress reacquisition.
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("released lease reacquired")
		}
	case <-time.After(time.Second):
		t.Fatal("release deadlocked with reacquisition")
	}
	child.Release()
	parent.Release()
	if len(pool.Slots) != 0 || pool.Suspended() != 0 {
		t.Fatal("leases leaked")
	}
}

func brokerFixture(t *testing.T, dispatch ExternalToolDispatcher) (*Broker, ToolBinding, *Run) {
	t.Helper()
	store, run := executionStore(t)
	run.Status = RunStatusRunning
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	broker := &Broker{Store: store, External: dispatch}
	definition := ToolDefinition{Server: "external", Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}
	binding, err := broker.Bind(context.Background(), ToolInvocation{RunID: run.ID, Generation: run.Generation, InvocationID: "task", Role: "test", Grants: []ToolGrant{{Server: "external", Tool: "lookup"}}, Catalog: []ToolDefinition{definition}, Secrets: map[string]string{"token": "PRIVATE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	return broker, binding, run
}

func TestBrokerDurableDeduplicationRedactionAndUncertainEffects(t *testing.T) {
	var calls atomic.Int32
	broker, binding, run := brokerFixture(t, func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"text":"PRIVATE_TOKEN"}`), nil
	})
	req := ToolRequest{CallID: "one", Server: "external", Tool: "lookup", Arguments: json.RawMessage(`{"query":"PRIVATE_TOKEN"}`)}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := binding.Call(context.Background(), req)
			if err != nil || strings.Contains(string(result), "PRIVATE_TOKEN") {
				t.Errorf("bad result %s %v", result, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate external effects: %d", calls.Load())
	}
	records, err := broker.Store.ToolCalls(context.Background(), run.ID, false)
	if err != nil || len(records) != 1 {
		t.Fatal(records, err)
	}
	audit, _ := json.Marshal(records)
	if strings.Contains(string(audit), "PRIVATE_TOKEN") {
		t.Fatal("audit leaked credential")
	}
	req.Arguments = json.RawMessage(`{"query":"different"}`)
	if _, err = binding.Call(context.Background(), req); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	req.CallID = "spoof"
	req.Arguments = json.RawMessage(`{"query":"x","run_id":"another-owner"}`)
	if _, err = binding.Call(context.Background(), req); err == nil {
		t.Fatal("identity spoofing bypassed schema")
	}
	broker.External = func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return nil, fmt.Errorf("lost response")
	}
	req.CallID = "uncertain"
	req.Arguments = json.RawMessage(`{"query":"x"}`)
	if _, err = binding.Call(context.Background(), req); !errors.Is(err, ErrUncertainTool) {
		t.Fatal(err)
	}
	if _, err = binding.Call(context.Background(), req); !errors.Is(err, ErrUncertainTool) {
		t.Fatal("uncertain call replayed", err)
	}
	if calls.Load() != 2 {
		t.Fatal("uncertain effect was replayed")
	}
	req.CallID = "retry-with-new-identity"
	if _, err = binding.Call(context.Background(), req); !errors.Is(err, ErrUncertainTool) || calls.Load() != 2 {
		t.Fatal("model replayed an uncertain effect using a new call ID", err)
	}
	unresolved, err := broker.Store.ToolCalls(context.Background(), run.ID, true)
	if err != nil || len(unresolved) != 1 || unresolved[0].Status != "uncertain" {
		t.Fatal(unresolved, err)
	}
}

type nestedToolRunner struct{}

func (*nestedToolRunner) Capabilities(string) (ProviderCapabilities, error) {
	return ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (*nestedToolRunner) ToolPolicy(string) ([]ToolGrant, []ToolDefinition) {
	defs := EngineTools()
	grants := []ToolGrant{}
	for _, d := range defs {
		grants = append(grants, ToolGrant{Server: d.Server, Tool: d.Name})
	}
	return grants, defs
}
func (r *nestedToolRunner) RunAgent(ctx context.Context, req AgentRequest) (map[string]any, error) {
	result, err := r.RunTurn(ctx, SessionRequest{AgentRequest: req}, nil)
	return result.Outputs, err
}
func (*nestedToolRunner) RunTurn(ctx context.Context, req SessionRequest, _ func(ProviderEvent)) (SessionResult, error) {
	if req.Prompt == "leaf" {
		return SessionResult{Status: RunStatusSucceeded, Outputs: map[string]any{"text": "leaf finished"}}, nil
	}
	prompt := "child"
	turns := 3
	if req.Prompt == "child" {
		prompt = "leaf"
		turns = 1
	}
	args, _ := json.Marshal(map[string]any{"role": "child", "turns": turns, "prompt": prompt})
	data, err := req.Tools.Binding.Call(ctx, ToolRequest{CallID: "spawn", Server: "engine", Tool: "worker_spawn", Arguments: args})
	if err != nil {
		return SessionResult{}, err
	}
	var admitted struct {
		Worker  Worker        `json:"worker"`
		Message WorkerMessage `json:"message"`
	}
	if err = json.Unmarshal(data, &admitted); err != nil {
		return SessionResult{}, err
	}
	if err = req.Tools.Lease.Suspend(); err != nil {
		return SessionResult{}, err
	}
	args, _ = json.Marshal(map[string]any{"worker_id": admitted.Worker.ID, "message_id": admitted.Message.ID})
	data, err = req.Tools.Binding.Call(ctx, ToolRequest{CallID: "wait", Server: "engine", Tool: "worker_wait", Arguments: args})
	if acquireErr := req.Tools.Lease.Reacquire(ctx); acquireErr != nil {
		return SessionResult{}, acquireErr
	}
	if err != nil {
		return SessionResult{}, err
	}
	return SessionResult{Status: RunStatusSucceeded, Outputs: map[string]any{"text": string(data)}}, nil
}

func TestWorkflowToChildToGrandchildAtCapacityOne(t *testing.T) {
	runner := &nestedToolRunner{}
	e, store := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/parent, with: {prompt: root}}]}}", runner)
	pool := &Capacity{Slots: make(chan struct{}, 1)}
	e.Slots = pool.Slots
	e.Capacity = pool
	workers := &WorkerManager{Store: store, Slots: pool.Slots, Capacity: pool, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return runner, []string{"Read"}, nil, nil
	}}
	if err := workers.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	broker := &Broker{Store: store, Workers: workers}
	binder := func(ctx context.Context, in ToolInvocation) (ToolBinding, error) {
		grants, catalog := runner.ToolPolicy(in.Role)
		if in.Grants == nil {
			in.Grants = grants
		}
		in.Catalog = catalog
		in.NativeTools = []string{"Read"}
		in.Access = WorkerAccess{Writable: true}
		in.Limits = WorkerLimits{MaxTurns: 8, MaxDepth: 4}
		return broker.Bind(ctx, in)
	}
	e.BindTools = binder
	workers.BindTools = binder
	e.BeforeFinalize = workers.StopRun
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); workers.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatalf("nested workflow failed: %+v %v", run, err)
	}
	if run.Status != RunStatusSucceeded {
		t.Fatal(run)
	}
	children, err := workers.List(context.Background(), run.ID)
	if err != nil || len(children) != 2 {
		t.Fatal(children, err)
	}
	if len(pool.Slots) != 0 || pool.Suspended() != 0 {
		t.Fatal("nested workflow leaked leases")
	}
	usage, usageErr := store.Usage(context.Background(), run.ID)
	if usageErr != nil || usage.Summary.Invocations != 3 {
		t.Fatal("nested worker turns missing from originating usage", usage, usageErr)
	}
	var remaining int
	if err = store.db.QueryRow("SELECT remaining FROM invocation_grants WHERE run_id=?", run.ID).Scan(&remaining); err != nil || remaining != 5 {
		t.Fatal("parent grant reserved incorrectly", remaining, err)
	}
}

func TestStableWorkerSpawnReservesParentGrantOnce(t *testing.T) {
	m, run := workerFixture(t, &fakeSession{})
	boundary := &ToolInvocation{RunID: run, Generation: 1, InvocationID: "workflow-parent", NativeTools: []string{"Read", "Bash"}, Access: WorkerAccess{Writable: true}, Limits: WorkerLimits{MaxTurns: 8}}
	spec := WorkerSpec{RunID: run, Role: "child", Turns: 3, StableID: "0123456789abcdef0123456789abcdef", Boundary: boundary}
	one, err := m.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.Spawn(context.Background(), spec)
	if err != nil || one.ID != two.ID {
		t.Fatal(one, two, err)
	}
	var remaining int
	if err = m.Store.db.QueryRow("SELECT remaining FROM invocation_grants WHERE run_id=? AND invocation_id=?", run, boundary.InvocationID).Scan(&remaining); err != nil || remaining != 5 {
		t.Fatal("duplicate spawn reserved grant twice", remaining, err)
	}
	workers, err := m.List(context.Background(), run)
	if err != nil || len(workers) != 1 {
		t.Fatal(workers, err)
	}
	boundary.NativeTools = []string{"Read"}
	spec.StableID = "11111111111111111111111111111111"
	if _, err = m.Spawn(context.Background(), spec); err == nil {
		t.Fatal("child widened native tool policy")
	}
	if err = m.Store.db.QueryRow("SELECT remaining FROM invocation_grants WHERE run_id=? AND invocation_id=?", run, boundary.InvocationID).Scan(&remaining); err != nil || remaining != 5 {
		t.Fatal("rejected child consumed grant", remaining, err)
	}
}

func TestChildAccessCannotWidenNativeWorkspaceOrContainerBoundary(t *testing.T) {
	for _, test := range []struct {
		parent, child WorkerAccess
		allowed       bool
	}{
		{WorkerAccess{Writable: false, Workspace: "/repo"}, WorkerAccess{Writable: true, Workspace: "/repo"}, false},
		{WorkerAccess{Writable: true, Workspace: "/repo"}, WorkerAccess{Writable: true, Workspace: "/another"}, false},
		{WorkerAccess{Container: true, Writable: true, Workspace: "/repo"}, WorkerAccess{Writable: true, Workspace: "/repo"}, false},
		{WorkerAccess{Writable: true, Workspace: "/repo"}, WorkerAccess{Container: true, Writable: false, Workspace: "/repo"}, true},
	} {
		if got := childAccessAllowed(test.parent, test.child); got != test.allowed {
			t.Fatalf("incorrect access intersection %+v -> %+v: %v", test.parent, test.child, got)
		}
	}
}

func TestToolJournalKeepsExactIntegersAndDeniedIdentities(t *testing.T) {
	store, run := executionStore(t)
	run.Status = RunStatusRunning
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	broker := &Broker{Store: store, External: func(_ context.Context, _ ToolDefinition, args json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		return args, nil
	}}
	definition := ToolDefinition{Server: "external", Name: "integer", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)}
	binding, err := broker.Bind(context.Background(), ToolInvocation{RunID: run.ID, Generation: run.Generation, InvocationID: "integers", Grants: []ToolGrant{{Server: "external", Tool: "integer"}}, Catalog: []ToolDefinition{definition}, Secrets: map[string]string{"secret": "PRIVATE_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	req := ToolRequest{CallID: "integer", Server: "external", Tool: "integer", Arguments: json.RawMessage(`{"n":9007199254740992}`)}
	if _, err = binding.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Arguments = json.RawMessage(`{"n":9007199254740993}`)
	if _, err = binding.Call(context.Background(), req); err == nil {
		t.Fatal("float rounding collapsed distinct argument fingerprints")
	}
	req.CallID = "next"
	result, err := binding.Call(context.Background(), req)
	if err != nil || !strings.Contains(string(result), "9007199254740993") {
		t.Fatal("exact integer changed before dispatch", string(result), err)
	}
	req.CallID = "denied"
	req.Server = "PRIVATE_TOKEN"
	if _, err = binding.Call(context.Background(), req); err == nil {
		t.Fatal("ungranted tool admitted")
	}
	records, err := store.ToolCalls(context.Background(), run.ID, false)
	if err != nil || len(records) != 3 {
		t.Fatal(records, err)
	}
	encoded, _ := json.Marshal(records)
	if strings.Contains(string(encoded), "PRIVATE_TOKEN") {
		t.Fatal("denied identity leaked credential")
	}
	req.Server = "external"
	if _, err = binding.Call(context.Background(), req); err == nil {
		t.Fatal("denied call identity reused for a different effect")
	}
	if calls.Load() != 2 {
		t.Fatal("denied call dispatched")
	}
}
