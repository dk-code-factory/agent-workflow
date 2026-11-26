package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type sdkManagedEngineFixture struct {
	runner   *Runner
	pool     *workflows.Capacity
	children atomic.Int32
}

func (f *sdkManagedEngineFixture) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, ManagedPolicy: true}, nil
}
func (f *sdkManagedEngineFixture) UsageProvider(role, model string) (string, string) {
	if role == "child" {
		return "fixture", model
	}
	return "claude-sdk", "claude-sonnet-4-6"
}
func (f *sdkManagedEngineFixture) ToolPolicy(role string) ([]workflows.ToolGrant, []workflows.ToolDefinition) {
	if role == "child" {
		return []workflows.ToolGrant{}, nil
	}
	return []workflows.ToolGrant{{Server: "engine", Tool: "worker_spawn"}, {Server: "engine", Tool: "worker_wait"}}, workflows.EngineTools()
}
func (f *sdkManagedEngineFixture) RunTurn(ctx context.Context, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if req.AgentID == "child" {
		if f.pool.Suspended() != 1 {
			return workflows.SessionResult{}, fmt.Errorf("SDK child lacked attested parent suspension")
		}
		f.children.Add(1)
		sid := "child-" + req.ParentWorkerID
		if emit != nil {
			emit(workflows.ProviderEvent{Type: "session", SessionID: sid})
		}
		return workflows.SessionResult{Status: workflows.RunStatusSucceeded, SessionID: sid, Outputs: map[string]any{"text": "CHILD_JOINED", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}, nil
	}
	return f.runner.RunTurn(ctx, req, emit)
}
func TestSDKManagedProductionEngineCapacityOne(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("locked SDK/local fake gateway only")
	}
	root := t.TempDir()
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: "sdk-managed-capacity", Version: 1, Status: workflows.RunStatusQueued, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	spawn := "mcp__broker__" + workflows.ToolWireName(workflows.ToolDefinition{Server: "engine", Name: "worker_spawn"})
	wait := "mcp__broker__" + workflows.ToolWireName(workflows.ToolDefinition{Server: "engine", Name: "worker_wait"})
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"input_tokens":16,"data":[],"has_more":false}`)
			return
		}
		var body map[string]any
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		stage := requests.Add(1)
		model, _ := body["model"].(string)
		w = skillChildSDKWriter{ResponseWriter: w, id: fmt.Sprint("sdk-engine-response-", stage)}
		call := func(id, name string, args map[string]any) map[string]any {
			return map[string]any{"type": "tool_use", "id": id, "name": name, "input": args}
		}
		switch stage {
		case 1:
			tools, _ := body["tools"].([]any)
			if len(tools) != 2 {
				t.Errorf("SDK exact broker catalog differs: %+v", tools)
			}
			writeSDKMessage(w, model, []map[string]any{call("spawn-a", spawn, map[string]any{"role": "child", "turns": 1, "prompt": "a"}), call("spawn-b", spawn, map[string]any{"role": "child", "turns": 1, "prompt": "b"})}, "tool_use")
		case 2:
			receipts, e := store.ToolCalls(t.Context(), run.ID, false)
			if e != nil {
				t.Error(e)
				return
			}
			var blocks []map[string]any
			history, _ := json.Marshal(body["messages"])
			for _, receipt := range receipts {
				if receipt.Tool != "worker_spawn" {
					continue
				}
				if receipt.Status != "completed" || !strings.Contains(string(history), receipt.CallID) {
					t.Errorf("SDK model resumed before original spawn result: %+v", receipt)
				}
				var result struct {
					Worker  workflows.Worker        `json:"worker"`
					Message workflows.WorkerMessage `json:"message"`
				}
				if e = json.Unmarshal(receipt.Result, &result); e != nil || result.Worker.ID == "" || result.Message.ID == "" {
					t.Errorf("invalid original spawn receipt %s", receipt.Result)
					return
				}
				blocks = append(blocks, call("wait-"+receipt.CallID, wait, map[string]any{"worker_id": result.Worker.ID, "message_id": result.Message.ID}))
			}
			if len(blocks) != 2 {
				t.Errorf("SDK incomplete original spawn receipts %d", len(blocks))
			}
			writeSDKMessage(w, model, blocks, "tool_use")
		case 3:
			writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "SDK_PARENT_AND_CHILDREN_JOINED"}}, "end_turn")
		default:
			t.Errorf("unplanned SDK model attempt %d", stage)
			w.WriteHeader(400)
		}
	}))
	defer gateway.Close()
	bridge, e := filepath.Abs("../../bridge/claude/bridge.mjs")
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("SDK_ENGINE_FIXTURE_KEY", "fake-only")
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 8}
	fixture := &sdkManagedEngineFixture{pool: pool, runner: &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"parent": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_ENGINE_FIXTURE_KEY"}}}}}}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: func(ctx context.Context, _, role string, _ []string) (workflows.SessionRunner, []string, map[string]string, error) {
		if role == "child" {
			report, e := store.Usage(ctx, run.ID)
			if e != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens < 16 {
				return nil, nil, nil, fmt.Errorf("SDK effect preceded durable usage: %+v %v", report, e)
			}
		}
		return fixture, []string{}, nil, nil
	}}
	broker := &workflows.Broker{Store: store, Workers: manager}
	manager.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		in.Catalog = workflows.EngineTools()
		return broker.Bind(ctx, in)
	}
	if err = manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	parent, err := manager.Spawn(t.Context(), workflows.WorkerSpec{RunID: run.ID, Role: "parent", Turns: 3})
	if err != nil {
		t.Fatal(err)
	}
	original, err := manager.Send(t.Context(), run.ID, parent.ID, "original", "Run two admitted child tasks")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Serve(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("owned SDK engine did not join")
		}
	}()
	var completed workflows.WorkerMessage
	for ctx.Err() == nil {
		messages, e := manager.Messages(ctx, run.ID, parent.ID)
		if e != nil {
			t.Fatal(e)
		}
		if len(messages) == 1 && messages[0].Status != workflows.RunStatusQueued && messages[0].Status != workflows.RunStatusRunning {
			completed = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != workflows.RunStatusSucceeded || completed.Result.Outputs["text"] != "SDK_PARENT_AND_CHILDREN_JOINED" || requests.Load() != 3 || fixture.children.Load() != 2 {
		t.Fatalf("SDK capacity1 original=%+v APIs=%d children=%d", completed, requests.Load(), fixture.children.Load())
	}
	current, err := manager.Get(ctx, run.ID, parent.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal("SDK allowance changed", current, err)
	}
	receipts, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(receipts) != 4 {
		t.Fatal(receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.Status != "completed" || receipt.InvocationID != original.ID {
			t.Fatal("SDK original receipt changed", receipt)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.InputTokens == nil || *report.Summary.InputTokens != 50 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 17 || !report.Summary.Complete {
		t.Fatalf("SDK aggregate repeated/omitted original responses: %+v", report)
	}
	t.Logf("locked SDK .322 native2.1.322 capacity1 APIs3 child2 receipts4 original=%s Remaining0 usage50/17", original.ID)
}
