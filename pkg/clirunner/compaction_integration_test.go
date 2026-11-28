package clirunner

import (
	"bytes"
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

// Force the actual pinned native runtime across its compaction threshold. The
// contrary settings exercise the environment ceiling, while the unbounded
// control must visibly attempt extra model work against the same fake API.
func testSDKCompactionCeilingAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; local fake API only")
	}
	for _, ceiling := range []bool{true, false} {
		name := "enforced"
		if !ceiling {
			name = "negative-control"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			bridgeRoot := filepath.Join(root, "bridge")
			if err := os.Mkdir(bridgeRoot, 0700); err != nil {
				t.Fatal(err)
			}
			original, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"bridge.mjs", "isolation.mjs", "broker-stream.mjs"} {
				data, err := os.ReadFile(filepath.Join(original, file))
				if err != nil {
					t.Fatal(err)
				}
				if file == "isolation.mjs" {
					data = append(data, []byte("\nisolatedSettings.autoCompactEnabled = true; isolatedSettings.precomputeCompactionEnabled = true; isolatedSettings.idleCompaction = true; isolatedEnvironment.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE = '1'; isolatedEnvironment.CLAUDE_CODE_AUTO_COMPACT_WINDOW = '50000';\n")...)
					if !ceiling {
						data = append(data, []byte("isolatedEnvironment.DISABLE_COMPACT = '0'; isolatedEnvironment.DISABLE_AUTO_COMPACT = '0';\n")...)
					}
				}
				if err := os.WriteFile(filepath.Join(bridgeRoot, file), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(original, "node_modules"), filepath.Join(bridgeRoot, "node_modules")); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			var summaries atomic.Int32
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
				wire, _ := json.Marshal(payload)
				if bytes.Contains(wire, []byte("Your task is to create a detailed summary")) || bytes.Contains(wire, []byte("summarizing conversations")) {
					summaries.Add(1)
				}
				stage := calls.Add(1)
				if stage > 6 {
					w.WriteHeader(400)
					return
				}
				model, _ := payload["model"].(string)
				input := 16
				if stage == 1 {
					input = 9900000
				}
				if stream, _ := payload["stream"].(bool); !stream {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_compaction_fixture", "type": "message", "role": "assistant", "model": model, "content": []any{map[string]string{"type": "text", "text": "COMPACTION_GATE_OK"}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]int{"input_tokens": input, "output_tokens": 5}})
					return
				}
				recorded := httptest.NewRecorder()
				writeSDKMessage(recorded, model, []map[string]any{{"type": "text", "text": "COMPACTION_GATE_OK"}}, "end_turn")
				body := bytes.ReplaceAll(recorded.Body.Bytes(), []byte(`"input_tokens":16`), []byte(`"input_tokens":`+jsonNumber(input)))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(body)
			}))
			defer gateway.Close()
			t.Setenv("SDK_COMPACTION_KEY", "fake-gateway-only")
			runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Bridge: filepath.Join(bridgeRoot, "bridge.mjs"), Model: "claude-sonnet-4-6", Tools: []string{}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_COMPACTION_KEY"}}}}}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", RunID: "compaction-fixture", InvocationID: "first", ParentWorkerID: "worker", Prompt: "Reply COMPACTION_GATE_OK. FIRST_TURN_COMPACTION_NONCE"}}
			first, firstErr := runner.RunTurn(ctx, req, nil)
			if firstErr != nil || first.SessionID == "" {
				t.Fatalf("initial fake turn: %v %+v calls=%d", firstErr, first, calls.Load())
			}
			req.SessionID, req.InvocationID, req.Prompt = first.SessionID, "second", "Reply COMPACTION_GATE_OK. SECOND_TURN_COMPACTION_NONCE"
			second, secondErr := runner.RunTurn(ctx, req, nil)
			if ceiling {
				// With automatic compaction disabled an oversized continuation must
				// stop locally instead of silently buying another model turn.
				if secondErr == nil || second.SessionID != first.SessionID || second.Outputs["text"] != "Prompt is too long" || calls.Load() != 1 || summaries.Load() != 0 {
					t.Fatalf("compaction ceiling: %v %+v calls=%d summaries=%d", secondErr, second, calls.Load(), summaries.Load())
				}
			} else if calls.Load() <= 2 || summaries.Load() == 0 {
				t.Fatalf("negative control did not trigger native summarization: calls=%d summaries=%d err=%v", calls.Load(), summaries.Load(), secondErr)
			}
			t.Logf("ceiling=%t model_requests=%d summary_requests=%d", ceiling, calls.Load(), summaries.Load())
		})
	}
}

func jsonNumber(value int) string {
	data, _ := json.Marshal(value)
	return string(data)
}
