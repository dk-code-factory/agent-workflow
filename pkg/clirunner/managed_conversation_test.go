//go:build linux

package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type managedConversationFixture struct {
	*managedEngineFixture
	mu       sync.Mutex
	sessions map[string][]string
}

func (f *managedConversationFixture) Preflight(ctx context.Context, roles []string) error {
	if f.identity != nil {
		return f.runner.Preflight(ctx, roles)
	}
	return nil
}
func (f *managedConversationFixture) HasAgent(id string) bool { return f.runner.HasAgent(id) }
func (f *managedConversationFixture) ValidateConversationOutput(id string, schema map[string]any) error {
	return f.runner.ValidateConversationOutput(id, schema)
}
func (f *managedConversationFixture) NativeTaskRole() string { return f.runner.NativeTaskRole() }
func (f *managedConversationFixture) AdmitNativeTask(d *workflows.AgentDefinition) (*workflows.AgentDefinition, error) {
	return f.runner.AdmitNativeTask(d)
}
func (f *managedConversationFixture) RunAgent(context.Context, workflows.AgentRequest) (map[string]any, error) {
	return nil, fmt.Errorf("conversation must use existing session runner")
}
func (f *managedConversationFixture) RunTurn(ctx context.Context, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	result, err := f.managedEngineFixture.RunTurn(ctx, req, emit)
	f.mu.Lock()
	f.sessions[req.AgentID] = append(f.sessions[req.AgentID], result.SessionID)
	f.mu.Unlock()
	return result, err
}

func TestManagedConversationNativeOriginalAttempts(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) { testManagedConversationNative(t, provider, false) })
	}
}
func TestManagedConversationNativeUnknownReplayHold(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) { testManagedConversationNative(t, provider, true) })
	}
}
func TestManagedConversationNativeDelegatedDenial(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) { testManagedConversationNative(t, provider, false, true) })
	}
}
func testManagedConversationNative(t *testing.T, provider string, unknown bool, delegatedDenial ...bool) {
	denial := len(delegatedDenial) == 1 && delegatedDenial[0]
	if provider == "claude-sdk" && os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("locked SDK and local fake API only")
	}
	var identity *ManagedCodexBuildIdentity
	if provider == "codex-app-server" {
		identity = managedNativeFixtureIdentity(t)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := `jobs: {solve: {steps: [{id: solution, conversation: [builder, reviewer], finishBy: builder, maxTurns: 3, prompt: task, with: {output: {schema: {type: object, properties: {answer: {type: string}}, required: [answer], additionalProperties: false}}}}]}}`
	if err := os.WriteFile(filepath.Join(root, "workflows/main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := "/messages"
		if provider == "codex-app-server" {
			path = "/responses"
		}
		if !strings.HasSuffix(r.URL.Path, path) {
			fmt.Fprint(w, `{"input_tokens":16,"data":[],"has_more":false}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		n := int(requests.Add(1))
		if unknown {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}

		if pool.Suspended() != 0 || len(pool.Slots) != 1 {
			t.Errorf("API%d began without original parent lease", n)
		}
		tools, _ := body["tools"].([]any)
		expectedTools := 2
		if denial {
			expectedTools++
		}
		if len(tools) != expectedTools {
			t.Errorf("conversation catalog=%d want %d", len(tools), expectedTools)
		}
		step := n
		if denial && n > 1 {
			step--
		}
		name := "send"
		args := map[string]any{"to": "reviewer", "message": "review this"}
		if step == 3 {
			args = map[string]any{"to": "builder", "message": "reviewed"}
		}
		if step == 5 {
			name = "finish"
			args = map[string]any{"answer": "approved"}
		}
		wire := workflows.ToolWireName(workflows.ToolDefinition{Server: "workflow-conversation", Name: name})
		toolResponse := step%2 == 1
		if denial && n == 1 {
			wire = workflows.ToolWireName(workflows.ToolDefinition{Server: "external", Name: "lookup"})
			args = map[string]any{"query": 7}
			toolResponse = true
		}
		if denial && n == 2 {
			raw, _ := json.Marshal(body)
			if !strings.Contains(string(raw), "original-call-1") {
				t.Error("original denied result missing before continuation")
			}
		}
		model, _ := body["model"].(string)
		if provider == "claude-sdk" {
			w = skillChildSDKWriter{ResponseWriter: w, id: fmt.Sprint("conversation-native-", n)}
			blocks := []map[string]any{{"type": "text", "text": "TURN_DONE"}}
			stop := "end_turn"
			if toolResponse {
				blocks = []map[string]any{{"type": "tool_use", "id": fmt.Sprint("original-call-", n), "name": "mcp__broker__" + wire, "input": args}}
				stop = "tool_use"
			}
			writeSDKMessage(w, model, blocks, stop)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(value any) { raw, _ := json.Marshal(value); fmt.Fprintf(w, "data: %s\n\n", raw) }
		if toolResponse {
			raw, _ := json.Marshal(args)
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": fmt.Sprint("fc-", n), "call_id": fmt.Sprint("original-call-", n), "name": wire, "arguments": string(raw)}})
		} else {
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprint("m-", n), "content": []any{map[string]any{"type": "output_text", "text": "TURN_DONE"}}}})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("conversation-native-", n), "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs("../../bridge/claude/bridge.mjs")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDK_CONVERSATION_NATIVE_KEY", "fake-only")
	agent := Agent{Provider: provider, Bridge: bridge, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_CONVERSATION_NATIVE_KEY"}}
	if provider == "codex-app-server" {
		agent.Model = "gpt-5.4"
		agent.Authentication = Authentication{Mode: "api-key", CredentialEnv: "SDK_CONVERSATION_NATIVE_KEY"}
		agent.ManagedBuild = &ManagedBuild{Manifest: os.Getenv("AGENT_WORKFLOW_TEST_MANAGED_CODEX_MANIFEST"), Identity: identity}
	}
	runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "builder", NativeTaskAdmission: 2, Agents: map[string]Agent{"builder": agent, "reviewer": agent}}}
	var agents workflows.AgentRunner = runner
	var fixture *managedConversationFixture
	if identity != nil {
		fixture = &managedConversationFixture{managedEngineFixture: &managedEngineFixture{runner: runner, identity: identity, pool: pool, gatewayURL: gateway.URL}, sessions: map[string][]string{}}
		agents = fixture
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor := &workflows.Executor{WorkspaceDir: root, Agents: agents, Store: store, Capacity: pool, Slots: pool.Slots}
	var delegatedEffects atomic.Int32
	if denial {
		broker := &workflows.Broker{Store: store, External: func(context.Context, workflows.ToolDefinition, json.RawMessage) (json.RawMessage, error) {
			delegatedEffects.Add(1)
			return json.RawMessage(`{}`), nil
		}}
		executor.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
			in.Grants = []workflows.ToolGrant{{Server: "external", Tool: "lookup"}}
			in.Catalog = []workflows.ToolDefinition{{Server: "external", Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}}
			return broker.Bind(ctx, in)
		}
	}
	expectedAPIs := int32(6)
	if denial {
		expectedAPIs++
	}

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	executor.RoleRunner = func(json.RawMessage) (workflows.AgentRunner, error) { return agents, nil }
	run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
	if unknown {
		if err == nil || requests.Load() != 1 {
			t.Fatalf("unknown attempt was retried: run=%+v API=%d err=%v", run, requests.Load(), err)
		}
		before, e := store.Attempts(ctx, run.ID)
		if e != nil {
			t.Fatal(e)
		}
		var original *workflows.ConversationState
		for _, a := range before {
			if a.Conversation != nil {
				original = a.Conversation
			}
		}
		if original == nil || original.Calls != 1 || original.Active == nil {
			t.Fatal("original active allowance lost", original)
		}
		_, resumeErr := executor.Resume(ctx, run.ID, nil, true)
		if resumeErr == nil || !errors.Is(resumeErr, workflows.ErrActionIntegrity) || requests.Load() != 1 {
			t.Fatalf("unknown conversation resumed provider: API=%d err=%v", requests.Load(), resumeErr)
		}
		after, e := store.Attempts(ctx, run.ID)
		if e != nil || len(after) != len(before) {
			t.Fatal("unknown retry admitted another turn", after, e)
		}
		for _, a := range after {
			if a.Conversation != nil && (a.Conversation.Calls != 1 || !a.Conversation.Deadline.Equal(original.Deadline) || a.Conversation.Active == nil) {
				t.Fatal("original allowance/deadline reset", a)
			}
		}
		t.Logf("%s actual first API lost before Completed: public retry remains original-attempt HOLD/API1/no new attempt/Calls1/same original deadline", provider)
		return
	}

	if err != nil || run.Status != workflows.RunStatusSucceeded {
		t.Fatalf("run=%+v APIs=%d err=%v", run, requests.Load(), err)
	}
	attempts, err := store.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var conversation *workflows.ConversationState
	turns := 0
	for _, a := range attempts {
		if a.Conversation != nil {
			conversation = a.Conversation
		}
		if strings.Contains(a.StepID, "/conversation/") {
			turns++
		}
	}
	if conversation == nil || !conversation.Finished || conversation.Calls != 3 || conversation.Turns != 3 || len(conversation.Receipts) != 3 || len(conversation.Sessions) != 2 || turns != 3 || requests.Load() != expectedAPIs {
		t.Fatalf("original conversation=%+v turns=%d APIs=%d", conversation, turns, requests.Load())
	}
	if fixture != nil {
		builder := fixture.sessions["builder"]
		if len(builder) != 2 || builder[0] == "" || builder[0] != builder[1] {
			t.Fatal("original participant session changed", builder)
		}
	}
	if denial {
		calls, err := store.ToolCalls(ctx, run.ID, false)
		if err != nil || len(calls) != 1 || calls[0].Status != "denied" || calls[0].CallID != "original-call-1" || delegatedEffects.Load() != 0 {
			t.Fatal("delegated original denial lost receipt/ran effects", calls, err)
		}
	}
	usage, err := store.Usage(ctx, run.ID)
	if err != nil || !usage.Summary.Complete || len(usage.Invocations) != 3 {
		t.Fatal("original task accounting incomplete", usage, err)
	}
	for _, inv := range usage.Invocations {
		if inv.Kind != "task" || !inv.Finished {
			t.Fatal("worker fiction or uncommitted task accounting", inv)
		}
	}
	for _, a := range attempts {
		if a.Conversation != nil {
			same, reused, e := store.BeginConversation(ctx, run.ID, a.JobID, a.StepID, a.Iteration, a.InputFingerprint, "")
			if e != nil || !reused || same.ID != a.ID || requests.Load() != expectedAPIs {
				t.Fatal("completed original conversation journal not reused", same, reused, e)
			}
		}
	}
	t.Logf("%s original public conversation: 3 turns/%d APIs/3 durable receipts/2 retained participant sessions/task accounting atomically finished", provider, requests.Load())
}

func TestManagedPublicAgentTaskNativeContinuation(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "claude-sdk" && os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
				t.Skip("locked SDK/local fake API only")
			}
			var identity *ManagedCodexBuildIdentity
			if provider == "codex-app-server" {
				identity = managedNativeFixtureIdentity(t)
			}
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			source := `jobs:
  solve:
    steps:
      - id: first
        uses: agent/run
        with: {agent: worker.yaml, prompt: original, turns: 2}
      - id: continued
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: continue}
`
			for name, text := range map[string]string{"main.yml": source, "worker.yaml": "name: worker\ndescription: A deterministic fixture.\ninstructions: Return the admitted text.\n"} {
				if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := "/messages"
				if provider == "codex-app-server" {
					path = "/responses"
				}
				if !strings.HasSuffix(r.URL.Path, path) {
					fmt.Fprint(w, `{"input_tokens":16,"data":[],"has_more":false}`)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				n := requests.Add(1)
				if tools, _ := body["tools"].([]any); len(tools) != 0 {
					t.Errorf("empty native task exposed %d tools", len(tools))
				}
				if provider == "claude-sdk" {
					model, _ := body["model"].(string)
					writeSDKMessage(skillChildSDKWriter{ResponseWriter: w, id: fmt.Sprint("public-task-", n)}, model, []map[string]any{{"type": "text", "text": "ORIGINAL_TASK"}}, "end_turn")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(value any) { raw, _ := json.Marshal(value); fmt.Fprintf(w, "data: %s\n\n", raw) }
				send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprint("m-", n), "content": []any{map[string]any{"type": "output_text", "text": "ORIGINAL_TASK"}}}})
				send(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprint("public-task-", n), "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
			}))
			defer gateway.Close()
			bridge, err := filepath.Abs("../../bridge/claude/bridge.mjs")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("MANAGED_PUBLIC_TASK_KEY", "fake-only")
			agent := Agent{Provider: provider, Bridge: bridge, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "MANAGED_PUBLIC_TASK_KEY"}}
			if provider == "codex-app-server" {
				agent.Model = "gpt-5.4"
				agent.Authentication = Authentication{Mode: "api-key", CredentialEnv: "MANAGED_PUBLIC_TASK_KEY"}
				agent.ManagedBuild = &ManagedBuild{Manifest: os.Getenv("AGENT_WORKFLOW_TEST_MANAGED_CODEX_MANIFEST"), Identity: identity}
			}
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "builder", NativeTaskAdmission: 2, Agents: map[string]Agent{"builder": agent}}}
			var agents workflows.AgentRunner = runner
			var sessions workflows.SessionRunner = runner
			if identity != nil {
				fixture := &managedConversationFixture{managedEngineFixture: &managedEngineFixture{runner: runner, identity: identity, gatewayURL: gateway.URL}, sessions: map[string][]string{}}
				agents = fixture
				sessions = fixture
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			slots := make(chan struct{}, 1)
			manager := &workflows.WorkerManager{Store: store, Slots: slots, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
				return sessions, []string{}, nil, nil
			}}
			if err = manager.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); manager.Serve(ctx) }()
			defer func() { cancel(); awaitManagedServe(t, done) }()
			executor := &workflows.Executor{WorkspaceDir: root, Agents: agents, Store: store, Workers: manager, Slots: slots, BeforeFinalize: manager.StopRun}
			run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
			if err != nil || run.Status != workflows.RunStatusSucceeded || requests.Load() != 2 {
				t.Fatalf("public task=%+v API=%d err=%v", run, requests.Load(), err)
			}
			workers, err := manager.List(ctx, run.ID)
			if err != nil || len(workers) != 1 || workers[0].SessionID == "" {
				t.Fatal("original task worker/session lost", workers, err)
			}
			messages, err := manager.Messages(ctx, run.ID, workers[0].ID)
			if err != nil || len(messages) != 2 {
				t.Fatal(messages, err)
			}
			for _, msg := range messages {
				if msg.Status != workflows.RunStatusSucceeded || msg.Result.SessionID != workers[0].SessionID {
					t.Fatal("continuation replaced original session", msg)
				}
			}
			usage, err := store.Usage(ctx, run.ID)
			if err != nil || !usage.Summary.Complete || len(usage.Invocations) != 2 {
				t.Fatal("public task accounting", usage, err)
			}
			if provider == "claude-sdk" && (usage.Summary.KnownCostUSD == nil || math.Abs(*usage.Summary.KnownCostUSD-0.000246) > 1e-12) {
				t.Fatalf("cumulative cost double charged: %+v", usage.Summary)
			}
			t.Logf("native original costs first=%v continuation=%v durableKnown=%v", messages[0].Result.Outputs["cost_usd"], messages[1].Result.Outputs["cost_usd"], usage.Summary.KnownCostUSD)
			t.Logf("%s public YAML agent/run+original reference continuation: one worker/same native session/2 original messages/2 API/complete usage", provider)
		})
	}
}
