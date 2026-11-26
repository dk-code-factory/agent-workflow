package clirunner

import (
	"context"
	"encoding/json"
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

// Exercise the locked SDK, including a provider-forced tool call. Prompt-only
// assertions against a fake bridge cannot establish tool-free repair.
func testSDKRepairAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; local fake API only")
	}
	const task = "ORIGINAL_TASK_PRIVATE_CONTEXT_134"
	const input = "ORIGINAL_TYPED_INPUT_134"
	const instructions = "ORIGINAL_ROLE_INSTRUCTIONS_134"
	const repair = "REPAIR_DATA_ONLY_134"
	var calls atomic.Int32
	root := t.TempDir()
	marker := filepath.Join(root, "denied-tool-ran")
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
		wire, _ := json.Marshal(payload)
		if stage == 2 || stage == 3 {
			for _, nonce := range []string{task, input, instructions, "ORIGINAL_DEFINITION_134"} {
				if strings.Contains(string(wire), nonce) {
					t.Errorf("repair received task context %q", nonce)
				}
			}
			if !strings.Contains(string(wire), repair) {
				t.Error("repair data missing")
			}
			if tools, _ := payload["tools"].([]any); len(tools) != 0 {
				t.Errorf("repair advertised tools: %v", tools)
			}
		}
		model, _ := payload["model"].(string)
		if stage == 2 {
			writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "denied-repair-call", "name": "Bash", "input": map[string]string{"command": "touch " + marker}}}, "tool_use")
			return
		}
		if stage == 4 && (!strings.Contains(string(wire), task) || strings.Contains(string(wire), repair)) {
			t.Error("repair replaced or contaminated the original session")
		}
		writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": `{"ok":true}`}}, "end_turn")
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDK_REPAIR_KEY", "fake-gateway-only")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"role": {
		Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", InstructionText: instructions, Tools: []string{"Bash"},
		Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_REPAIR_KEY"},
	}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", RunID: "repair-fixture", InvocationID: "task", ParentWorkerID: "worker", Prompt: task, Input: map[string]string{"private": input}}}
	original, err := runner.RunTurn(ctx, req, nil)
	if err != nil || original.SessionID == "" {
		t.Fatalf("original turn failed: %+v %v", original, err)
	}
	req.SessionID = original.SessionID
	req.InvocationID = "repair"
	req.RepairOnly = true
	req.Prompt = repair
	// Retained task fields must not reach the SDK, even for callers that pass
	// the whole durable task request into the repair adapter.
	req.Definition = &workflows.AgentDefinition{DeveloperInstructions: "ORIGINAL_DEFINITION_134"}
	req.Output = &workflows.AgentOutputContract{Schema: map[string]any{"type": "object"}}
	fixed, err := runner.RunTurn(ctx, req, nil)
	if err != nil || fixed.SessionID == "" || fixed.SessionID == original.SessionID || fixed.Outputs["text"] != `{"ok":true}` {
		t.Fatalf("repair context failed: %+v %v", fixed, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied repair tool executed: %v", err)
	}
	req = workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", RunID: "repair-fixture", InvocationID: "continue", ParentWorkerID: "worker", Prompt: "continue"}, SessionID: original.SessionID}
	continued, err := runner.RunTurn(ctx, req, nil)
	if err != nil || continued.SessionID != original.SessionID || calls.Load() != 4 {
		t.Fatalf("continuation failed: %+v %v calls=%d", continued, err, calls.Load())
	}
}

func TestSelectedSkillRepairRejectsBeforeProviderSetup(t *testing.T) {
	runner := &Runner{}
	_, err := runner.RunTurn(context.Background(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RepairOnly: true, Skill: &workflows.SkillInvocation{}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "selected skill formatting repair") {
		t.Fatalf("selected repair reached runtime role/provider setup: %v", err)
	}
}
