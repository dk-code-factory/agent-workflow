package clirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestSDKManagedInitializationFailureDiagnostic(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	root := t.TempDir()
	for _, name := range []string{"bridge.mjs", "isolation.mjs", "broker-stream.mjs"} {
		data, err := os.ReadFile(filepath.Join("../../bridge/claude", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Only the SDK stream is a fixture: the public Runner and maintained bridge
	// perform their real model binding, initialization check and failed result.
	const sdk = `export const tool=()=>{}; export const createSdkMcpServer=()=>{};
export function query({prompt,options}) {
 return {
  initializationResult:async()=>({claude_code_version:'2.1.322',models:[{value:'claude-sonnet-4-6',resolvedModel:'claude-sonnet-4-6'}]}),
  interrupt:async()=>{},close(){},
  async *[Symbol.asyncIterator]() {
   const row=await prompt[Symbol.asyncIterator]().next();
   if(row.done||row.value.message.content!=='OWNED_INIT_DIAGNOSTIC'||options.tools.length)throw Error('fixture prompt/authority mismatch');
   yield {type:'system',subtype:'init',session_id:'rejected-init',tools:[],plugins:[{name:'unadmitted-plugin'}],skills:[]};
   throw Error('initialization rejection did not stop the original');
  }
 };
}`
	for name, data := range map[string]string{
		"node_modules/@anthropic-ai/claude-agent-sdk/package.json": `{"type":"module","exports":"./sdk.mjs","version":"0.3.322"}`,
		"node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs":      sdk,
		"node_modules/zod/package.json":                            `{"type":"module","exports":"./index.mjs"}`,
		"node_modules/zod/index.mjs":                               `export const z={};`,
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	binding := &managedBatchBinding{}
	t.Setenv("INIT_DIAGNOSTIC_KEY", "fake-only")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Bridge: filepath.Join(root, "bridge.mjs"), Model: "claude-sonnet-4-6", Tools: []string{}, Authentication: Authentication{Mode: "gateway", Endpoint: "http://127.0.0.1:1", CredentialEnv: "INIT_DIAGNOSTIC_KEY"}}}}}
	result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", Prompt: "OWNED_INIT_DIAGNOSTIC", Tools: &workflows.InvocationTools{Binding: binding, Lease: lease}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "Provider initialization violates engine skill/plugin isolation") || !strings.Contains(err.Error(), "SDK original response/callback did not complete") || result.Status != workflows.RunStatusFailed || result.Outputs["usage_incomplete"] != true {
		t.Fatal("original initialization cause or failed integrity was lost", result, err)
	}
	if binding.calls.Load() != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
		t.Fatal("failed initialization granted effects or released capacity")
	}
}

func TestSDKManagedFailedFrameDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name                                                                string
		proof, partial, pending, badUsage, badIdentity, success, emptyError bool
		integrity                                                           string
	}{
		{name: "empty-failed-frame", emptyError: true, integrity: "SDK original response/callback did not complete"},
		{name: "partial-known-usage", partial: true, integrity: "SDK original response/callback did not complete"},
		{name: "complete-proof-failed", proof: true},
		{name: "invalid-terminal-accounting", proof: true, partial: true, badUsage: true, integrity: "SDK final input_tokens differs"},
		{name: "unresolved-original-callback", proof: true, pending: true, integrity: "SDK original response/callback did not complete"},
		{name: "failed-provider-identity-mutation", proof: true, partial: true, badIdentity: true, integrity: "bridge provider identity changed"},
		{name: "successful-original-control", proof: true, success: true},
		{name: "successful-accounting-rejection", proof: true, badUsage: true, success: true, integrity: "SDK final input_tokens differs"},
		{name: "successful-identity-rejection", proof: true, badIdentity: true, success: true, integrity: "bridge provider identity changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
			lease, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			definition := workflows.ToolDefinition{Server: "engine", Name: "worker_status"}
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}}
			usage := map[string]any{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 0}
			frames := []bridgeFrame{{Version: 2, Type: "hello", ProviderVersion: "claude-agent-sdk@" + ClaudeSDKVersion}}
			if tc.partial {
				frames = append(frames, bridgeFrame{Version: 2, Type: "usage", SessionID: "original", Data: map[string]any{"usage": usage}})
			}
			if tc.proof {
				calls := []map[string]any{}
				reason := "end_turn"
				if tc.pending {
					calls = append(calls, map[string]any{"id": "original-call", "name": "mcp__broker__" + workflows.ToolWireName(definition), "arguments": json.RawMessage(`{}`)})
					reason = "tool_use"
				}
				proof := map[string]any{"response_id": "original-response", "model": "fixture", "stop_reason": reason, "usage": usage, "calls": calls}
				frames = append(frames, bridgeFrame{Version: 2, Type: "broker_model_start", SessionID: "original", Data: proof}, bridgeFrame{Version: 2, Type: "broker_batch", SessionID: "original", Data: proof})
				if tc.pending {
					frames = append(frames, bridgeFrame{Version: 2, Type: "tool_request", SessionID: "original", CallID: "original-call", Server: definition.Server, Tool: definition.Name, Arguments: json.RawMessage(`{}`)})
				}
			}
			status, cause := workflows.RunStatusFailed, "original provider failure"
			if tc.success {
				status, cause = workflows.RunStatusSucceeded, ""
			}
			if tc.emptyError {
				cause = ""
			}
			outputs := map[string]any{}
			if tc.proof {
				terminalUsage := map[string]any{}
				for key, value := range usage {
					terminalUsage[key] = value
				}
				if tc.badUsage {
					terminalUsage["input_tokens"] = 12
				}
				outputs["usage"] = terminalUsage
			}
			version := "claude-agent-sdk@" + ClaudeSDKVersion
			if tc.badIdentity {
				version = "claude-agent-sdk@unadmitted"
			}
			frames = append(frames, bridgeFrame{Version: 2, Type: "result", Status: status, ProviderStatus: "original-status", ProviderVersion: version, Outputs: outputs, Error: cause})
			var input, output bytes.Buffer
			encoder := json.NewEncoder(&input)
			for _, frame := range frames {
				if err := encoder.Encode(frame); err != nil {
					t.Fatal(err)
				}
			}
			observed, known, stopped, authorizations, usageEvents := 0, 0, 0, 0, 0
			result, err := bridgeTurnControlled(ctx, &input, &bridgeWriter{writer: &output}, map[string]any{"managed_broker": true, "model": "fixture"}, &workflows.InvocationTools{Binding: binding, Lease: lease}, func(event workflows.ProviderEvent) {
				if event.Type == "usage" {
					usageEvents++
				}
			}, func() { stopped++ }, nil, func(p sdkBrokerProof) error {
				observed++
				if p.ResponseID != "original-response" || p.SessionID != "original" {
					return fmt.Errorf("wrong original observation")
				}
				return nil
			}, func() error { authorizations++; return nil }, func(workflows.SessionResult) error { known++; return nil })
			validSuccess := tc.success && tc.integrity == ""
			if validSuccess {
				if err != nil || result.Status != workflows.RunStatusSucceeded || known != 1 {
					t.Fatal("strict original success changed", result, err, known)
				}
			} else {
				if err == nil || tc.integrity != "" && !strings.Contains(err.Error(), tc.integrity) {
					t.Fatal("integrity diagnostic lost", result, err)
				}
				if !tc.success && (!strings.Contains(err.Error(), "provider failed: "+cause) || result.Status != workflows.RunStatusFailed || result.ProviderStatus != "original-status" || result.Outputs["usage_incomplete"] != true) {
					t.Fatal("original failed result/cause was lost", result, err)
				}
				if tc.success && result.Status == workflows.RunStatusSucceeded || known != 0 {
					t.Fatal("invalid or failed frame granted a successful known terminal", result, known)
				}
			}
			if tc.partial && !tc.success {
				retained, _ := result.Outputs["usage"].(map[string]any)
				if fmt.Sprint(retained["input_tokens"]) != "11" || fmt.Sprint(retained["output_tokens"]) != "3" || usageEvents != 1 {
					t.Fatal("known partial usage was lost/replaced", result, usageEvents)
				}
			}
			wantObserved := 0
			if tc.proof {
				wantObserved = 1
			}
			if observed != wantObserved || stopped != 1 || binding.calls.Load() != 0 || authorizations != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 || strings.Contains(output.String(), "tool_result") || result.ProviderVersion != "claude-agent-sdk@"+ClaudeSDKVersion {
				t.Fatal("failed/untrusted original gained authority or changed provider identity", result, observed, known, stopped, output.String())
			}
		})
	}
}
