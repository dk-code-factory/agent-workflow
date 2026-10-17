package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

// Uses the actual pinned SDK against a local fake Messages API. No upstream
// credentials or model calls are involved. CI enables this after npm ci.
func TestSDKBridgeAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("enable after npm ci --prefix bridge/claude")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(bridge), "node_modules", "@anthropic-ai", "claude-agent-sdk")); err != nil {
		t.Fatal("install pinned SDK first", err)
	}
	root := t.TempDir()
	marker := "NATIVE_MEMORY_MUST_NOT_LOAD_091c7f"
	if err = os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(filepath.Join(root, ".claude"), 0700)
	hook := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch NATIVE_HOOK_RAN"}]}]}}`
	_ = os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(hook), 0600)
	var messages atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":16}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		current := messages.Add(1)
		if current == 2 {
			history, _ := json.Marshal(payload["messages"])
			if !strings.Contains(string(history), "FIRST_TURN_PUBLIC_NONCE_d41d") {
				t.Error("resumed SDK session lost prior context")
			}
		}
		system, _ := json.Marshal(payload["system"])
		if strings.Contains(string(system), marker) {
			t.Error("native memory was discovered")
		}
		if !strings.Contains(string(system), "ENGINE_ONLY_FIXTURE") {
			t.Error("engine instructions missing")
		}
		reply := "SDK_OK"
		nativeStructured := false
		body, _ := json.Marshal(payload["messages"])
		if strings.Contains(string(body), "VALID_STRUCTURED_FIXTURE") {
			reply = `{"ok":true}`
			nativeStructured = true
		}
		model, _ := payload["model"].(string)
		message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 16, "output_tokens": 0}}
		if stream, _ := payload["stream"].(bool); !stream {
			message["content"] = []any{map[string]string{"type": "text", "text": reply}}
			message["stop_reason"] = "end_turn"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(message)
			return
		}
		block := map[string]any{"type": "text", "text": ""}
		delta := map[string]any{"type": "text_delta", "text": reply}
		stop := "end_turn"
		if nativeStructured {
			block = map[string]any{"type": "tool_use", "id": "tool_fixture", "name": "StructuredOutput", "input": map[string]any{}}
			delta = map[string]any{"type": "input_json_delta", "partial_json": reply}
			stop = "tool_use"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(kind string, data any) {
			encoded, _ := json.Marshal(data)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": message})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": 3}})
		emit("message_stop", map[string]string{"type": "message_stop"})
	}))
	defer gateway.Close()
	t.Setenv("SDK_FIXTURE_KEY", "sk-ant-fixture-no-upstream-credentials")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"reviewer": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", InstructionText: "ENGINE_ONLY_FIXTURE", Tools: []string{}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_FIXTURE_KEY"}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "reviewer", Prompt: "Reply SDK_OK. Remember FIRST_TURN_PUBLIC_NONCE_d41d"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != workflows.RunStatusSucceeded || result.Outputs["text"] != "SDK_OK" || result.SessionID == "" || messages.Load() != 1 {
		t.Fatalf("bridge contract failed: %+v requests=%d", result, messages.Load())
	}
	resumed, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "reviewer", Prompt: "Reply SDK_OK again"}, SessionID: result.SessionID}, nil)
	if err != nil || resumed.SessionID != result.SessionID || resumed.Outputs["text"] != "SDK_OK" || messages.Load() != 2 {
		t.Fatalf("SDK resume failed: %+v %v requests=%d", resumed, err, messages.Load())
	}
	before := messages.Load()
	_, _ = runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "reviewer", Prompt: "Return an object", Output: &workflows.AgentOutputContract{Format: "json", Schema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}}}, nil)
	if count := messages.Load() - before; count != 1 {
		t.Fatalf("native schema mismatch repeated a task %d times", count)
	}
	schema := &workflows.AgentOutputContract{Format: "json", Schema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}
	valid, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "reviewer", Prompt: "VALID_STRUCTURED_FIXTURE", Output: schema}}, nil)
	if err != nil || valid.Outputs["structured"] == nil {
		t.Fatalf("native structured decoding failed: %+v %v", valid, err)
	}
	if _, err = os.Stat(filepath.Join(root, "NATIVE_HOOK_RAN")); !os.IsNotExist(err) {
		t.Fatal("native hook executed")
	}
	if err = os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "workflows", "usage.yml"), []byte("jobs: {x: {steps: [{uses: agent/reviewer, with: {prompt: SDK_OK}}]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor := &workflows.Executor{WorkspaceDir: root, Agents: runner, Store: store}
	run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/usage.yml"})
	if err != nil {
		t.Fatal("engine SDK accounting execution failed", err)
	}
	usage, err := store.Usage(ctx, run.ID)
	if err != nil || usage.Summary.InputTokens == nil || *usage.Summary.InputTokens != 16 || usage.Summary.OutputTokens == nil || !usage.Summary.Complete || usage.Summary.CostUSD == nil {
		t.Fatalf("pinned SDK observations did not reach durable accounting: %+v %v", usage, err)
	}
}
