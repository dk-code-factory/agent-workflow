package clirunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestNativeTaskReadOnlyAdmission(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk/package.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"version":"0.3.322"}`), 0600); err != nil {
		t.Fatal(err)
	}
	role := Agent{Provider: "claude-sdk", Bridge: filepath.Join(root, "bridge.mjs"), Tools: []string{"Read", "Bash", "Write", "WebFetch", "Grep", "Read"}}
	runner := &Runner{Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": role}}}
	definition := &workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only"}
	admitted, err := runner.AdmitNativeTask(definition)
	if err != nil || admitted.NativePolicy == nil || !reflect.DeepEqual(admitted.NativePolicy.NativeTools, []string{"Grep", "Read"}) || definition.NativePolicy != nil {
		t.Fatal("policy did not narrow the role without mutating input", admitted, err)
	}
	if admitted.NativePolicy.Profile != claudeNativeReadOnlyProfile || admitted.NativePolicy.Provider != "claude-sdk" {
		t.Fatal(admitted.NativePolicy)
	}
	for _, variant := range []string{"codex", "container", "unknown", "profile", "ceiling", "omitted", "new-grant"} {
		t.Run(variant, func(t *testing.T) {
			copy := *admitted
			policy := *admitted.NativePolicy
			copy.NativePolicy = &policy
			cfg := runner.Config
			current := role
			switch variant {
			case "codex":
				current.Provider = "codex-app-server"
				current.Tools = nil
			case "container":
				current.Execution.Mode = "container"
			case "unknown":
				copy.ToolPolicy = "unrestricted"
			case "profile":
				copy.NativePolicy.Profile = "future"
			case "ceiling":
				copy.NativePolicy.NativeTools = []string{"Bash"}
			case "omitted":
				copy.ToolPolicy = ""
			case "new-grant":
				current.Tools = append(append([]string{}, role.Tools...), "Glob")
			}
			cfg.Agents = map[string]Agent{"native": current}
			if _, err := (&Runner{Config: cfg}).AdmitNativeTask(&copy); err == nil {
				t.Fatal("changed policy accepted", variant)
			}
		})
	}
	role.Tools = nil
	runner.Config.Agents["native"] = role
	empty, err := runner.AdmitNativeTask(definition)
	if err != nil || empty.NativePolicy.NativeTools == nil || len(empty.NativePolicy.NativeTools) != 0 {
		t.Fatal("policy added Read to an ungranted role", empty, err)
	}
}

// Run under the same CI selector as the other actual pinned-SDK fake gateways.
func TestSDKNativeAgainstFakeGatewayReadOnlyPolicy(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; loopback fake API only")
	}
	root := t.TempDir()
	proposal := filepath.Join(root, "proposal.json")
	const original = `{"proposal":"READ_ONLY_PROPOSAL_151"}`
	if err := os.WriteFile(proposal, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "forbidden-write")
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"input_tokens":16}`))
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		stage := calls.Add(1)
		tools, _ := payload["tools"].([]any)
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			if tool["name"] != "Read" {
				t.Errorf("read-only query advertised extra tool: %v", tool["name"])
			}
		}
		if len(tools) != 1 {
			t.Errorf("read-only query lost Read: %v", tools)
		}
		model, _ := payload["model"].(string)
		switch stage {
		case 1:
			writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "read-proposal", "name": "Read", "input": map[string]string{"file_path": proposal}}}, "tool_use")
		case 2:
			history, _ := json.Marshal(payload["messages"])
			if !strings.Contains(string(history), "READ_ONLY_PROPOSAL_151") {
				t.Error("actual Read did not return the proposal")
			}
			// Force an unadvertised native effect; prompts and allowedTools alone
			// would not prove that the read-only grant is enforced.
			writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "denied-write", "name": "Bash", "input": map[string]string{"command": "printf forbidden > " + marker}}}, "tool_use")
		default:
			writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "READ_ONLY_OK"}}, "end_turn")
		}
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("READONLY_151_KEY", "fake-loopback-key-only")
	runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Tools: []string{"Read", "Bash", "Write", "WebFetch"}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "READONLY_151_KEY"}}}}}
	definition, err := runner.AdmitNativeTask(&workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only", DeveloperInstructions: "Validate the supplied proposal."})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "native", ParentWorkerID: "agent_151", InvocationID: "original151", Definition: definition, Prompt: "Validate the proposal."}}
	result, err := runner.RunTurn(ctx, req, nil)
	if err != nil || result.Outputs["text"] != "READ_ONLY_OK" || result.TaskPolicy == nil || calls.Load() != 3 {
		t.Fatalf("native policy failed: %+v err=%v calls=%d", result, err, calls.Load())
	}
	if result.TaskPolicy.Agent != req.ParentWorkerID || result.TaskPolicy.InvocationID != req.InvocationID || result.TaskPolicy.SessionFingerprint != workflows.NativeTaskSessionFingerprint(result.SessionID) || !reflect.DeepEqual(result.TaskPolicy.Grant.NativeTools, []string{"Read"}) {
		t.Fatal("grant evidence lost original identity", result.TaskPolicy)
	}
	if content, err := os.ReadFile(proposal); err != nil || string(content) != original {
		t.Fatal("proposal changed", string(content), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("forbidden write executed", err)
	}
	req.SessionID = result.SessionID
	req.InvocationID = "continuation151"
	req.Prompt = "Continue the same validation."
	continued, err := runner.RunTurn(ctx, req, nil)
	if err != nil || continued.SessionID != result.SessionID || continued.TaskPolicy == nil || continued.TaskPolicy.SessionFingerprint != result.TaskPolicy.SessionFingerprint || continued.TaskPolicy.InvocationID == result.TaskPolicy.InvocationID || calls.Load() != 4 {
		t.Fatal("continuation changed session grant", continued, err, calls.Load())
	}
}

func TestSDKNativeAgainstFakeGatewayReadOnlyFormatting(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; loopback fake API only")
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"input_tokens":16}`))
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		tools, _ := payload["tools"].([]any)
		seen := map[string]bool{}
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			name, _ := tool["name"].(string)
			seen[name] = true
		}
		if !reflect.DeepEqual(seen, map[string]bool{"Read": true, "StructuredOutput": true}) {
			t.Errorf("unexpected formatting tool grant: %v", seen)
		}
		model, _ := payload["model"].(string)
		writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "format-verdict", "name": "StructuredOutput", "input": map[string]any{"approved": true}}}, "tool_use")
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("READONLY_FORMAT_KEY", "fake-loopback-key-only")
	runner := &Runner{Workspace: t.TempDir(), Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Tools: []string{"Read", "Bash"}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "READONLY_FORMAT_KEY"}}}}}
	definition, err := runner.AdmitNativeTask(&workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "native", ParentWorkerID: "agent-format", InvocationID: "original-format", Definition: definition, Prompt: "Format the validation verdict.", Output: &workflows.AgentOutputContract{Format: "json", Schema: map[string]any{"type": "object", "required": []string{"approved"}, "additionalProperties": false, "properties": map[string]any{"approved": map[string]any{"type": "boolean"}}}}}}, nil)
	if err != nil || result.TaskPolicy == nil || !result.TaskPolicy.FormattingTool || calls.Load() != 1 {
		t.Fatal("formatting-only grant failed", result, err, calls.Load())
	}
	structured, _ := result.Outputs["structured"].(map[string]any)
	if structured["approved"] != true || result.Outputs["text"] != `{"approved":true}` {
		t.Fatal("native formatter did not return its schema result", result)
	}
}

type nativePolicyBrokerProbe struct{ calls atomic.Int32 }

func (b *nativePolicyBrokerProbe) Catalog() []workflows.ToolDefinition {
	return []workflows.ToolDefinition{{Server: "fixture", Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}
func (b *nativePolicyBrokerProbe) Call(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
	b.calls.Add(1)
	return json.RawMessage(`{"forbidden":true}`), nil
}

func TestSDKNativeAgainstFakeGatewayReadOnlyDeniedEffects(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; loopback fake API only")
	}
	for _, kind := range []string{"Write", "WebFetch", "MCP"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			proposal := filepath.Join(root, "proposal")
			if err := os.WriteFile(proposal, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			var network, calls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { network.Add(1); _, _ = w.Write([]byte("forbidden")) }))
			defer target.Close()
			probe := &nativePolicyBrokerProbe{}
			name := kind
			input := map[string]any{"file_path": proposal, "content": "changed"}
			if kind == "WebFetch" {
				input = map[string]any{"url": target.URL, "prompt": "Read this page"}
			}
			if kind == "MCP" {
				name = "mcp__broker__" + workflows.ToolWireName(probe.Catalog()[0])
				input = map[string]any{}
			}
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[],"has_more":false,"input_tokens":16}`))
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				tools, _ := payload["tools"].([]any)
				if len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
					t.Errorf("query widened its grant: %v", tools)
				}
				model, _ := payload["model"].(string)
				if calls.Add(1) == 1 {
					writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "forced-forbidden", "name": name, "input": input}}, "tool_use")
				} else {
					writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "DENIED"}}, "end_turn")
				}
			}))
			defer gateway.Close()
			bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("READONLY_DENIAL_KEY", "fake-loopback-key-only")
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Tools: []string{"Read", "Write", "WebFetch"}, MCPGrants: []workflows.ToolGrant{{Server: "fixture", Tool: "write"}}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "READONLY_DENIAL_KEY"}}}}}
			definition, err := runner.AdmitNativeTask(&workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "native", ParentWorkerID: "agent-denial", InvocationID: kind, Definition: definition, Prompt: "Validate the proposal.", Tools: &workflows.InvocationTools{Binding: probe}}}, nil)
			if err != nil || result.TaskPolicy == nil || calls.Load() != 2 || probe.calls.Load() != 0 || network.Load() != 0 {
				t.Fatal("forbidden effect escaped read-only grant", result, err, calls.Load(), probe.calls.Load(), network.Load())
			}
			content, err := os.ReadFile(proposal)
			if err != nil || string(content) != "original" {
				t.Fatal("forbidden write modified proposal", string(content), err)
			}
		})
	}
}

func TestSDKNativeAgainstFakeGatewayReadOnlyWorkflow(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; loopback fake API only")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"main.yml": `jobs:
  validate:
    steps:
      - id: original
        uses: agent/run
        with: {agent: validator.yaml, prompt: Validate, turns: 3}
      - id: continuation
        uses: agent/run
        with: {agent: '${{ steps.original.outputs.agent }}', prompt: Explain}
      - id: verify
        argv: [python3, -c, "import sys; assert sys.argv[1]=='read-only'; assert sys.argv[2]=='claude-sdk'; assert sys.argv[3]==sys.argv[4]; print('NATIVE_GRANT_VERIFIED')", '${{ steps.original.outputs.tool_policy.grant.mode }}', '${{ steps.original.outputs.tool_policy.grant.provider }}', '${{ steps.original.outputs.tool_policy.session_fingerprint }}', '${{ steps.continuation.outputs.tool_policy.session_fingerprint }}']
`,
		"validator.yaml": "name: validator\ndescription: Validate proposal.\ninstructions: Return a verdict.\ntool-policy: read-only\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"input_tokens":16}`))
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		model, _ := payload["model"].(string)
		writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "NATIVE_VERDICT"}}, "end_turn")
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("READONLY_WORKFLOW_KEY", "fake-loopback-key-only")
	runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Tools: []string{"Read", "Bash"}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "READONLY_WORKFLOW_KEY"}}}}}
	store, err := workflows.OpenSQLite(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	slots := make(chan struct{}, 1)
	workers := &workflows.WorkerManager{Store: store, Slots: slots, Factory: func(_ context.Context, run, role string, requested []string) (workflows.SessionRunner, []string, map[string]string, error) {
		_, snapshot, err := store.Load(ctx, run)
		if err != nil {
			return nil, nil, nil, err
		}
		var cfg Config
		if err := json.Unmarshal(snapshot.Roles, &cfg); err != nil {
			return nil, nil, nil, err
		}
		agent := cfg.Agents[role]
		if requested != nil {
			agent.Tools = requested
			cfg.Agents[role] = agent
		}
		return &Runner{Workspace: root, Config: cfg}, agent.Tools, nil, nil
	}}
	if err := workers.Init(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); workers.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("native policy workers did not stop")
		}
	}()
	executor := &workflows.Executor{WorkspaceDir: root, Agents: runner, Store: store, Workers: workers, Slots: slots, BeforeFinalize: workers.StopRun}
	run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != workflows.RunStatusSucceeded || calls.Load() != 2 {
		t.Fatal("actual native workflow grant failed", run, err, calls.Load())
	}
	steps := run.Jobs["validate"].Steps
	for _, name := range []string{"original", "continuation"} {
		proof, _ := steps[name].Outputs["tool_policy"].(map[string]any)
		if proof["agent"] != steps[name].Outputs["agent"] || proof["task_attempt_id"] != steps[name].AttemptID || proof["invocation_id"] == "" {
			t.Fatal("native grant lost controller binding", name, proof, steps[name])
		}
	}
}
