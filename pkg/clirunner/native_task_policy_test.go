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
