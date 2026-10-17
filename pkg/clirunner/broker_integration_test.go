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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type sdkBrokerFixture struct {
	pool       *workflows.Capacity
	calls      atomic.Int32
	definition workflows.ToolDefinition
	t          *testing.T
}

func (f *sdkBrokerFixture) Catalog() []workflows.ToolDefinition {
	return []workflows.ToolDefinition{f.definition}
}
func (f *sdkBrokerFixture) Call(ctx context.Context, req workflows.ToolRequest) (json.RawMessage, error) {
	f.calls.Add(1)
	if (req.CallID != "upstream-stable-call" && req.CallID != "upstream-second-call") || req.Server != "fixture" || req.Tool != "lookup" {
		f.t.Errorf("unstable callback identity: %+v", req)
	}
	child, err := f.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer child.Release()
	if f.pool.Suspended() != 1 {
		f.t.Error("provider did not attest quiescence before child capacity acquisition")
	}
	return json.RawMessage(`{"text":"BROKER_COMPLETED"}`), nil
}

func TestSDKBrokerAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; local fake API only")
	}
	definition := workflows.ToolDefinition{Server: "fixture", Name: "lookup", Description: "Fixture lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}
	qualified := "mcp__broker__" + workflows.ToolWireName(definition)
	var requests atomic.Int32
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
		model, _ := payload["model"].(string)
		if requests.Add(1) == 1 {
			tools, _ := json.Marshal(payload["tools"])
			if !strings.Contains(string(tools), qualified) {
				t.Errorf("broker tool missing from SDK catalog: %s", tools)
			}
			writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "upstream-stable-call", "name": qualified, "input": map[string]string{"query": "fixture"}}, {"type": "tool_use", "id": "upstream-second-call", "name": qualified, "input": map[string]string{"query": "fixture"}}}, "tool_use")
		} else {
			history, _ := json.Marshal(payload["messages"])
			if !strings.Contains(string(history), "BROKER_COMPLETED") {
				t.Errorf("broker result not returned to SDK: %s", history)
			}
			writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "SDK_BROKER_OK"}}, "end_turn")
		}
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDK_BROKER_KEY", "fake-gateway-only")
	runner := &Runner{Workspace: t.TempDir(), Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-5", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_BROKER_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	binding := &sdkBrokerFixture{pool: pool, definition: definition, t: t}
	var events []workflows.ProviderEvent
	var eventMu sync.Mutex
	result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", Prompt: "Use the fixture lookup tool once.", Tools: &workflows.InvocationTools{Binding: binding, Lease: lease}}}, func(event workflows.ProviderEvent) { eventMu.Lock(); events = append(events, event); eventMu.Unlock() })
	if err != nil || result.Outputs["text"] != "SDK_BROKER_OK" {
		t.Fatalf("SDK broker failed: %+v %v calls=%d suspended=%d events=%+v", result, err, binding.calls.Load(), pool.Suspended(), events)
	}
	if binding.calls.Load() != 2 || requests.Load() != 2 {
		t.Fatalf("unexpected repetitions: callbacks=%d requests=%d", binding.calls.Load(), requests.Load())
	}
	if pool.Suspended() != 0 || len(pool.Slots) != 1 {
		t.Fatal("SDK callback failed to reacquire parent capacity")
	}
}

func writeSDKMessage(w http.ResponseWriter, model string, blocks []map[string]any, stop string) {
	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(kind string, value any) {
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_broker_fixture", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 16, "output_tokens": 0}}})
	for i, block := range blocks {
		if block["type"] == "tool_use" {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}})
			input, _ := json.Marshal(block["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
		} else {
			emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": block["text"]}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 5}})
	emit("message_stop", map[string]string{"type": "message_stop"})
}
