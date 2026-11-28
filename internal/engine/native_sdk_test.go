package engine

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

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Runs the locked upstream SDK, with a local fake API and no real credentials.
func TestSDKNativeAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("enable after npm ci --prefix bridge/claude")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node required", err)
	}
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(nativeContinuationWorkflow, "      - id: gate\n        uses: input/wait\n        with: {prompt: Continue, evidence-id: native-evidence}\n", "", 1)
	for name, text := range map[string]string{"workflows/main.yml": source, "workflows/reviewer.yaml": "name: portable-reviewer\ndescription: Portable review\ninstructions: DEFINITION_INSTRUCTIONS\n", "CLAUDE.md": "HOST_MEMORY_MUST_NOT_LOAD"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch HOST_HOOK_RAN"}]}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"input_tokens":7}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[],"has_more":false}`)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		n := calls.Add(1)
		// This managed role admits no native formatter. JSON remains subject to
		// the unchanged engine output schema on both original turns.
		if tools, ok := payload["tools"].([]any); !ok || len(tools) != 0 {
			t.Errorf("unexpected SDK tool grant: %v", payload["tools"])
		}
		outputConfig, _ := payload["output_config"].(map[string]any)
		if payload["output_format"] != nil || outputConfig["format"] != nil {
			t.Errorf("unexpected SDK output formatter: %v %v", payload["output_format"], outputConfig)
		}
		history, _ := json.Marshal(payload["messages"])
		system, _ := json.Marshal(payload["system"])
		if !strings.Contains(string(system), "DEFINITION_INSTRUCTIONS") || !strings.Contains(string(system), "WORKSPACE_INSTRUCTIONS") || strings.Contains(string(system), "HOST_MEMORY_MUST_NOT_LOAD") {
			t.Errorf("native definition/workspace isolation lost: %s", system)
		}
		if !strings.Contains(string(history), "9007199254740993") {
			t.Errorf("typed native input/history lost on turn%d: %s", n, history)
		}
		if r.Header.Get("X-Api-Key") != "local-sdk-native-key" {
			t.Error("explicit gateway credential missing")
		}
		message := map[string]any{"id": fmt.Sprintf("native_%d", n), "type": "message", "role": "assistant", "model": payload["model"], "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 7, "output_tokens": 0}}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(kind string, data any) {
			encoded, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded)
		}
		emit("message_start", map[string]any{"type": "message_start", "message": message})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": `{"approved":true}`}})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]string{"stop_reason": "end_turn"}, "usage": map[string]int{"output_tokens": 3}})
		emit("message_stop", map[string]string{"type": "message_stop"})
	}))
	defer gateway.Close()
	t.Setenv("SDK_NATIVE_FIXTURE_KEY", "local-sdk-native-key")
	// Only Node is discoverable: Codex and provider login executables are absent.
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(node, filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := exec.LookPath("codex"); err == nil {
		t.Fatal("Codex unexpectedly available")
	}
	cfg := clirunner.Config{AgentRuntime: "native", Agents: map[string]clirunner.Agent{"native": {Provider: "claude-sdk", Command: node, Bridge: bridge, Model: "claude-sonnet-4-6", InstructionText: "WORKSPACE_INSTRUCTIONS", Tools: []string{}, Authentication: clirunner.Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_NATIVE_FIXTURE_KEY"}}}}
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, workers := Executor(cfg, store, root, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stop := serveNativeWorkers(t, ctx, workers)
	defer stop()
	run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != workflows.RunStatusSucceeded {
		t.Fatalf("SDK native workflow: %+v %v", run, err)
	}
	all, err := workers.List(ctx, run.ID)
	if err != nil || len(all) != 1 || all[0].Role != "native" || all[0].SessionID == "" || all[0].Remaining != 0 || calls.Load() != 2 {
		t.Fatalf("SDK native continuation: %+v calls=%d err=%v", all, calls.Load(), err)
	}
	usage, err := store.Usage(ctx, run.ID)
	if err != nil || usage.Summary.Invocations != 2 || !usage.Summary.Complete || usage.Summary.InputTokens == nil || *usage.Summary.InputTokens != 14 || usage.Summary.OutputTokens == nil || *usage.Summary.OutputTokens != 6 {
		t.Fatalf("SDK native original accounting: %+v %v", usage, err)
	}
	for _, id := range []string{"first", "follow"} {
		if structured, ok := run.Jobs["review"].Steps[id].Outputs["structured"].(map[string]any); !ok || structured["approved"] != true {
			t.Fatalf("structured native output missing: %+v", run.Jobs["review"].Steps[id])
		}
	}
	if _, err := os.Stat(filepath.Join(root, "HOST_HOOK_RAN")); !os.IsNotExist(err) {
		t.Fatal("host hook ran", err)
	}
}
