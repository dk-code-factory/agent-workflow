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

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type sdkBoundaryWriter struct {
	http.ResponseWriter
	before func(string) bool
	after  func(string)
}

func (w sdkBoundaryWriter) Write(p []byte) (int, error) {
	s := string(p)
	if w.before != nil && !w.before(s) {
		return len(p), nil
	}
	_, err := w.ResponseWriter.Write(p)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	if w.after != nil {
		w.after(s)
	}
	return len(p), err
}

type sdkBoundaryBinding struct {
	definition     workflows.ToolDefinition
	pool           *workflows.Capacity
	calls          atomic.Int32
	release        <-chan struct{}
	started        chan struct{}
	once           sync.Once
	blockReacquire chan *workflows.CapacityLease
}

func (b *sdkBoundaryBinding) Catalog() []workflows.ToolDefinition {
	return []workflows.ToolDefinition{b.definition}
}
func (b *sdkBoundaryBinding) Call(ctx context.Context, _ workflows.ToolRequest) (json.RawMessage, error) {
	b.calls.Add(1)
	child, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer child.Release()
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if b.blockReacquire != nil {
		child.Release()
		blocker, err := b.pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		b.blockReacquire <- blocker
	}
	return json.RawMessage(`{"text":"ORIGINAL_BOUNDARY_RESULT"}`), nil
}
func TestSDKBrokerNativeStreamBoundaries(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("locked SDK/local fake gateway only")
	}
	for _, mode := range []string{"delayed-final-and-eof", "synthetic-stop", "late-unplanned-tool", "error-before-stop", "cancel-reacquire"} {
		t.Run(mode, func(t *testing.T) {
			definition := workflows.ToolDefinition{Server: "fixture", Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`)}
			name := "mcp__broker__" + workflows.ToolWireName(definition)
			pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			lease, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			finalReached, releaseFinal, eofReached, releaseEOF, releaseTool := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var finalOnce, eofOnce, toolOnce sync.Once
			defer finalOnce.Do(func() { close(releaseFinal) })
			defer eofOnce.Do(func() { close(releaseEOF) })
			defer toolOnce.Do(func() { close(releaseTool) })
			binding := &sdkBoundaryBinding{definition: definition, pool: pool, release: releaseTool, started: make(chan struct{})}
			if mode == "cancel-reacquire" {
				binding.blockReacquire = make(chan *workflows.CapacityLease, 1)
			}
			var requests atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					fmt.Fprint(w, `{"input_tokens":16,"data":[],"has_more":false}`)
					return
				}
				var body map[string]any
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
					return
				}
				model, _ := body["model"].(string)
				stage := requests.Add(1)
				w = skillChildSDKWriter{ResponseWriter: w, id: fmt.Sprint("boundary-", stage)}
				if stage > 1 {
					capture, _ := json.Marshal(body)
					t.Logf("additional original SDK native API stage=%d bytes=%d semantic_results=%d suspended=%d", stage, len(capture), strings.Count(string(capture), "ORIGINAL_BOUNDARY_RESULT"), pool.Suspended())
					if mode == "delayed-final-and-eof" || mode == "error-before-stop" {
						if strings.Count(string(capture), "ORIGINAL_BOUNDARY_RESULT") != 2 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
							t.Error("normal next SDK model did not follow both leased original results")
						}
					}
					writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "BOUNDARY_OK"}}, "end_turn")
					return
				}
				wrapped := sdkBoundaryWriter{ResponseWriter: w, before: func(s string) bool {
					if strings.HasPrefix(s, "event: message_delta\n") {
						if mode == "delayed-final-and-eof" {
							close(finalReached)
							select {
							case <-releaseFinal:
							case <-ctx.Done():
								return false
							}
						}
						if mode == "synthetic-stop" {
							return false
						}
						if mode == "late-unplanned-tool" {
							fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"unplanned-native\",\"name\":\"Bash\",\"input\":{}}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n")
						}
					}
					if strings.HasPrefix(s, "event: message_stop\n") && mode == "error-before-stop" {
						fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"original stream cut\"}}\n\n")
						return false
					}
					return true
				}, after: func(s string) {
					if strings.HasPrefix(s, "event: message_stop\n") && mode == "delayed-final-and-eof" {
						close(eofReached)
						select {
						case <-releaseEOF:
						case <-ctx.Done():
						}
					}
				}}
				writeSDKMessage(wrapped, model, []map[string]any{{"type": "tool_use", "id": "original-first", "name": name, "input": map[string]string{"query": "first"}}, {"type": "tool_use", "id": "original-second", "name": name, "input": map[string]string{"query": "second"}}}, "tool_use")
			}))
			defer gateway.Close()
			bridge, e := filepath.Abs("../../bridge/claude/bridge.mjs")
			if e != nil {
				t.Fatal(e)
			}
			t.Setenv("SDK_BOUNDARY_KEY", "fake-only")
			runner := &Runner{Workspace: t.TempDir(), Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_BOUNDARY_KEY"}}}}}
			var events []workflows.ProviderEvent
			var eventMu sync.Mutex
			var result workflows.SessionResult
			var runErr error
			done := make(chan struct{})
			go func() {
				result, runErr = runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", Prompt: "Use exactly the original admitted lookup calls", Tools: &workflows.InvocationTools{Binding: binding, Lease: lease}}}, func(event workflows.ProviderEvent) { eventMu.Lock(); events = append(events, event); eventMu.Unlock() })
				close(done)
			}()
			if mode == "delayed-final-and-eof" {
				select {
				case <-finalReached:
				case <-done:
					t.Fatal("provider ended before delayed final", runErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				time.Sleep(100 * time.Millisecond)
				if binding.calls.Load() != 0 || pool.Suspended() != 0 || requests.Load() != 1 {
					t.Fatalf("false early barrier: calls%d suspended%d APIs%d", binding.calls.Load(), pool.Suspended(), requests.Load())
				}
				finalOnce.Do(func() { close(releaseFinal) })
				select {
				case <-eofReached:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case <-binding.started:
				case <-ctx.Done():
					t.Fatal("valid stop did not permit isolated callback", ctx.Err())
				}
				time.Sleep(100 * time.Millisecond)
				if binding.calls.Load() != 1 || pool.Suspended() != 1 || requests.Load() != 1 {
					t.Fatalf("held original callback allowed model/native overlap: calls%d suspended%d APIs%d", binding.calls.Load(), pool.Suspended(), requests.Load())
				}
				toolOnce.Do(func() { close(releaseTool) })
				eofOnce.Do(func() { close(releaseEOF) })
			} else {
				toolOnce.Do(func() { close(releaseTool) })
			}
			if mode == "cancel-reacquire" {
				var blocker *workflows.CapacityLease
				select {
				case blocker = <-binding.blockReacquire:
				case <-done:
					t.Fatal("SDK original callback ended before blocked reacquire", runErr)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				defer blocker.Release()
				cancel()
				select {
				case <-done:
				case <-time.After(6 * time.Second):
					t.Fatal("cancelled SDK original did not join")
				}
			} else {
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("owned SDK boundary did not settle", ctx.Err())
				}
			}
			eventMu.Lock()
			t.Logf("original SDK native events: %+v", events)
			eventMu.Unlock()
			if mode == "delayed-final-and-eof" || mode == "error-before-stop" {
				if runErr != nil || result.Status != workflows.RunStatusSucceeded || binding.calls.Load() != 2 || requests.Load() != 2 {
					t.Fatalf("original serial calls failed: %+v %v calls%d APIs%d", result, runErr, binding.calls.Load(), requests.Load())
				}
			} else if mode == "cancel-reacquire" {
				if runErr == nil || binding.calls.Load() != 1 || requests.Load() != 1 {
					t.Fatalf("cancelled SDK wait resumed model: %+v %v calls%d APIs%d", result, runErr, binding.calls.Load(), requests.Load())
				}
			} else if runErr == nil || binding.calls.Load() != 0 || requests.Load() != 1 {
				t.Fatalf("false boundary permitted effects/retry: %+v %v calls%d APIs%d", result, runErr, binding.calls.Load(), requests.Load())
			}
		})
	}
}
