//go:build linux

package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// This test-only runner reaches the production managed transport while its
// canonical profile is disabled. It does not expose a runtime admission bypass.
type managedEngineFixture struct {
	runner     *Runner
	identity   *ManagedCodexBuildIdentity
	pool       *workflows.Capacity
	children   atomic.Int32
	gatewayURL string
}

func (f *managedEngineFixture) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, ManagedPolicy: true}, nil
}
func (f *managedEngineFixture) UsageProvider(role, model string) (string, string) {
	if role == "child" {
		return "fixture", model
	}
	return "codex-app-server", model
}
func (f *managedEngineFixture) ToolPolicy(role string) ([]workflows.ToolGrant, []workflows.ToolDefinition) {
	if role == "child" || role == "empty" {
		return []workflows.ToolGrant{}, nil
	}
	if role == "effects" {
		return []workflows.ToolGrant{{Server: "fixture", Tool: "a"}, {Server: "fixture", Tool: "b"}}, managedEffectDefinitions()
	}
	return []workflows.ToolGrant{{Server: "engine", Tool: "worker_spawn"}, {Server: "engine", Tool: "worker_wait"}}, workflows.EngineTools()
}
func (f *managedEngineFixture) RunTurn(ctx context.Context, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if req.AgentID == "child" {
		if f.pool.Suspended() != 1 {
			return workflows.SessionResult{}, fmt.Errorf("child started without attested parent park")
		}
		f.children.Add(1)
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return workflows.SessionResult{}, ctx.Err()
		}
		session := "child-" + req.ParentWorkerID
		if emit != nil {
			emit(workflows.ProviderEvent{Type: "session", SessionID: session})
		}
		return workflows.SessionResult{SessionID: session, Status: workflows.RunStatusSucceeded, Outputs: map[string]any{"text": "CHILD_JOINED", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}, nil
	}
	// Exactly the production transport, including owner/admission checks, held
	// executable verification, native protocol, batch controller and accounting.
	agent, req := isolateRepairTurn(f.runner.Config.Agents[req.AgentID], req)
	if req.Input != nil {
		var err error
		req.Prompt, err = workflows.AgentTaskPrompt(req.Prompt, req.Input)
		if err != nil {
			return workflows.SessionResult{}, err
		}
	}
	if f.gatewayURL != "" {
		state, err := providerStateDir(f.runner.Workspace, "codex", req)
		if err != nil {
			return workflows.SessionResult{}, err
		}
		config := fmt.Sprintf("model_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", f.gatewayURL)
		if err = os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0600); err != nil {
			return workflows.SessionResult{}, err
		}
	}
	return f.runner.codexManagedTransport(ctx, agent, req, emit, f.identity)
}
func managedNativeFixtureIdentity(t *testing.T) *ManagedCodexBuildIdentity {
	t.Helper()
	path := os.Getenv("AGENT_WORKFLOW_TEST_MANAGED_CODEX_MANIFEST")
	if path == "" {
		t.Skip("owned compiled managed app-server manifest required for local deterministic native proof")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m managedCodexBinaryManifest
	if err = strictCodexDecode(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Use the frozen source tuple supplied by the owned build receipt. Test
	// selection never changes the embedded canonical disabled descriptor.
	source := managedCodexDescriptor{UpstreamSHA: m.Inputs.UpstreamSHA, PatchSHA256: m.Inputs.PatchSHA256, RecipeSHA256: m.Inputs.RecipeSHA256, Protocol: m.Inputs.Protocol, RustToolchain: m.Inputs.RustToolchain, OfflineWrapperSHA256: m.Inputs.OfflineWrapperSHA256, SourceSHA256: m.SourceSHA256}
	identity, err := captureManagedBuild(path, source)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
func TestManagedCodexProductionEngineCapacityOne(t *testing.T) {
	identity := managedNativeFixtureIdentity(t)
	root := t.TempDir()
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: "managed-capacity-original", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	spawnName := workflows.ToolWireName(workflows.ToolDefinition{Server: "engine", Name: "worker_spawn"})
	waitName := workflows.ToolWireName(workflows.ToolDefinition{Server: "engine", Name: "worker_wait"})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var body struct {
			Tools []map[string]any `json:"tools"`
			Input []map[string]any `json:"input"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		number := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(value map[string]any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value["type"], data)
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
		}
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprint("engine-response-", number)}})
		call := func(id, name string, args map[string]any) {
			raw, _ := json.Marshal(args)
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc-" + id, "call_id": id, "name": name, "arguments": string(raw)}})
		}
		switch number {
		case 1:
			if len(body.Tools) != 2 {
				t.Errorf("exact managed catalog differs: %s", raw)
			}
			for _, tool := range body.Tools {
				if tool["name"] != spawnName && tool["name"] != waitName {
					t.Errorf("native/ungranted tool escaped: %+v", tool)
				}
			}
			call("spawn-a", spawnName, map[string]any{"role": "child", "turns": 1, "prompt": "a"})
			call("spawn-b", spawnName, map[string]any{"role": "child", "turns": 1, "prompt": "b"})
		case 2:
			outputs := 0
			for _, item := range body.Input {
				if item["type"] != "function_call_output" {
					continue
				}
				outputs++
				raw, _ := json.Marshal(item["output"])
				var originalText string
				if err := json.Unmarshal(raw, &originalText); err != nil {
					t.Errorf("original spawn result is not the native text body: %s", raw)
					continue
				}
				var result struct {
					Worker  workflows.Worker        `json:"worker"`
					Message workflows.WorkerMessage `json:"message"`
				}
				if err := json.Unmarshal([]byte(originalText), &result); err != nil || result.Worker.ID == "" || result.Message.ID == "" {
					t.Errorf("spawn receipt invalid %s", originalText)
					continue
				}
				call("wait-"+item["call_id"].(string), waitName, map[string]any{"worker_id": result.Worker.ID, "message_id": result.Message.ID})
			}
			if outputs != 2 {
				t.Errorf("original spawn output list count %d", outputs)
			}
		case 3:
			outputs := 0
			for _, item := range body.Input {
				if item["type"] == "function_call_output" {
					outputs++
				}
			}
			if outputs != 4 {
				t.Errorf("full original outputs missing: %d", outputs)
			}
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "final", "content": []any{map[string]any{"type": "output_text", "text": "PARENT_AND_CHILDREN_JOINED"}}}})
		default:
			t.Errorf("unexpected replacement API request %d", number)
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("engine-response-", number), "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
	}))
	defer gateway.Close()
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 8}
	t.Setenv("MANAGED_FIXTURE_KEY", "synthetic-no-provider-key")
	fixture := &managedEngineFixture{identity: identity, pool: pool, runner: &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"parent": {Provider: "codex-app-server", Model: "gpt-5.4", Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_FIXTURE_KEY"}}}}}}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: func(ctx context.Context, _, role string, _ []string) (workflows.SessionRunner, []string, map[string]string, error) {
		if role == "child" {
			report, err := store.Usage(ctx, run.ID)
			if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens < 11 {
				return nil, nil, nil, fmt.Errorf("effect preceded durable original usage: %+v %v", report, err)
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
	message, err := manager.Send(t.Context(), run.ID, parent.ID, "original-request", "Run the admitted child tasks")
	if err != nil {
		t.Fatal(err)
	}
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: run.ID, ParentWorkerID: parent.ID, InvocationID: message.ID, AgentID: "parent"}}
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", gateway.URL)
	if err = os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { manager.Serve(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("owned engine workers did not join")
		}
	}()
	var completed workflows.WorkerMessage
	for ctx.Err() == nil {
		messages, err := manager.Messages(ctx, run.ID, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Status != workflows.RunStatusQueued && messages[0].Status != workflows.RunStatusRunning {
			completed = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Status != workflows.RunStatusSucceeded || completed.Result.Outputs["text"] != "PARENT_AND_CHILDREN_JOINED" || requests.Load() != 3 || fixture.children.Load() != 2 {
		t.Fatalf("parent=%+v APIs=%d children=%d", completed, requests.Load(), fixture.children.Load())
	}
	current, err := manager.Get(ctx, run.ID, parent.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal("original consumed allowance changed", current, err)
	}
	calls, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(calls) != 4 {
		t.Fatal(calls, err)
	}
	for _, call := range calls {
		if call.Status != "completed" || call.InvocationID != message.ID {
			t.Fatal("original receipt identity changed", call)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range report.Invocations {
		if inv.ID == message.ID {
			final := inv.Observations["final"]
			if len(inv.Observations) != 5 || !inv.Finished || final.InputTokens == nil || *final.InputTokens != 33 || final.OutputTokens == nil || *final.OutputTokens != 9 || !final.Complete {
				t.Fatalf("native response accounting absent %+v", inv)
			}
		}
	}
	if report.Summary.InputTokens == nil || *report.Summary.InputTokens != 35 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 11 {
		t.Fatal("final aggregate repeated original response charges", report.Summary)
	}
	t.Logf("actual binary=%s build=%s original=%s APIs=%d children=%d capacity=1 original-turn-remaining=0 receipts=%d", identity.BinarySHA256, identity.BuildID, message.ID, requests.Load(), fixture.children.Load(), len(calls))
}
