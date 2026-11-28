package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Exercise the public Runner and maintained bridge with an in-memory SDK stream,
// without a native CLI or provider. A read-only invocation removes the broader
// role's broker and unsafe native tools without mutating configured authority.
// An empty grant retains its acknowledgement and ordinary usage observation.
func TestNativeTaskPolicyAndManagedAccountingBoundaries(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	for _, nativeTools := range [][]string{{}, {"Read"}} {
		name := "back-to-back-empty-grant"
		if len(nativeTools) != 0 {
			name = "preterminal-barrier-and-native-ack"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"bridge.mjs", "isolation.mjs", "broker-stream.mjs"} {
				contents, err := os.ReadFile(filepath.Join("../../bridge/claude", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name), contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			const sdk = `export const tool=()=>{}; export const createSdkMcpServer=()=>{};
export function query({prompt,options}) {
 let acknowledged=false,earlyPrompt=false;
 const first=prompt[Symbol.asyncIterator]().next().then(row=>{earlyPrompt=!acknowledged;return row;});
 return {
  initializationResult:async()=>{await new Promise(resolve=>setTimeout(resolve,25));acknowledged=true;return {claude_code_version:'2.1.322',hooks_applied:true,hook_on_failure_supported:true};},
  interrupt:async()=>{},close(){},
  async *[Symbol.asyncIterator]() {
   const row=await first;
   if(earlyPrompt||row.done||row.value.message.content!=='EMPTY_POLICY_TASK')throw Error('task prompt escaped native policy acknowledgement');
   if(options.allowedTools.length!==options.tools.length||options.tools.some(name=>name!=='Read')||options.spawnClaudeCodeProcess||options.hooks.PreToolUse[0].onFailure!=='block')throw Error('empty native policy crossed into broker authority');
   const session_id='empty-native-policy-session';
   yield {type:'system',subtype:'init',session_id,tools:options.tools,plugins:[],skills:[]};
   yield {type:'assistant',session_id,message:{id:'native-policy-usage',content:[{type:'text',text:'EMPTY_POLICY_OK'}],usage:{input_tokens:2,output_tokens:1}}};
   if(options.tools.length){
    await options.hooks.PreToolUse[0].hooks[0]({tool_name:'Read',tool_use_id:'original-native-call',tool_input:{file_path:'owned-fixture-only'},session_id});
    await options.hooks.PostToolUse[0].hooks[0]({tool_use_id:'original-native-call'});
    await new Promise(resolve=>setImmediate(resolve));
   }
   yield {type:'result',session_id,subtype:'success',result:'EMPTY_POLICY_OK',usage:{input_tokens:2,output_tokens:1},total_cost_usd:0.01};
  }
 };
}`
			for name, contents := range map[string]string{
				"node_modules/@anthropic-ai/claude-agent-sdk/package.json": `{"type":"module","exports":"./sdk.mjs","version":"0.3.322"}`,
				"node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs":      sdk,
				"node_modules/zod/package.json":                            `{"type":"module","exports":"./index.mjs"}`,
				"node_modules/zod/index.mjs":                               `export const z={};`,
			} {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("EMPTY_NATIVE_POLICY_KEY", "fake-only")
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {
				Provider: "claude-sdk", Bridge: filepath.Join(root, "bridge.mjs"), Model: "claude-sonnet-4-6", Tools: append(append([]string{}, nativeTools...), "Write", "WebFetch"),
				MCPGrants:      []workflows.ToolGrant{{Server: "engine", Tool: "worker_status"}},
				Authentication: Authentication{Mode: "gateway", Endpoint: "http://127.0.0.1:1", CredentialEnv: "EMPTY_NATIVE_POLICY_KEY"},
			}}}}
			originalRole, err := json.Marshal(runner.Config.Agents["native"])
			if err != nil {
				t.Fatal(err)
			}
			definition, err := runner.AdmitNativeTask(&workflows.AgentDefinition{Name: "empty-validator", ToolPolicy: "read-only"})
			if err != nil || definition.NativePolicy == nil || !reflect.DeepEqual(definition.NativePolicy.NativeTools, nativeTools) {
				t.Fatal("empty native policy admission failed", definition, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			observations, barriers := 0, 0
			probe := &nativePolicyBrokerProbe{}
			result, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{
				AgentID: "native", ParentWorkerID: "original-worker", InvocationID: "original-message", Definition: definition, Prompt: "EMPTY_POLICY_TASK",
				Tools: &workflows.InvocationTools{Binding: probe},
				ObserveUsage: func(update workflows.UsageUpdate) error {
					observations++
					if update.Key != "native-policy-usage" || fmt.Sprint(update.Usage["input_tokens"]) != "2" || fmt.Sprint(update.Usage["output_tokens"]) != "1" {
						return fmt.Errorf("native usage observation changed: %+v", update)
					}
					return nil
				},
			}}, func(event workflows.ProviderEvent) {
				if event.Type == "progress" && event.Data["type"] == "broker_wait_state" {
					barriers++
				}
			})
			if err != nil || result.Outputs["text"] != "EMPTY_POLICY_OK" || observations != 1 || result.TaskPolicy == nil || result.SessionID != "empty-native-policy-session" {
				t.Fatal("native policy crossed managed accounting admission", result, err, observations)
			}
			if len(nativeTools) != 0 && barriers == 0 {
				t.Fatal("preterminal barrier was suppressed after native permit acknowledgement")
			}
			if !reflect.DeepEqual(result.TaskPolicy.Grant, *definition.NativePolicy) || result.TaskPolicy.InvocationID != "original-message" || result.TaskPolicy.Agent != "original-worker" || result.TaskPolicy.FormattingTool {
				t.Fatal("original empty native grant receipt changed", result.TaskPolicy)
			}
			currentRole, err := json.Marshal(runner.Config.Agents["native"])
			if err != nil || string(currentRole) != string(originalRole) || probe.calls.Load() != 0 {
				t.Fatal("read-only invocation retained broker effects or changed its configured role", err, probe.calls.Load())
			}
		})
	}
}

func TestSDKMixedNativeBrokerAuthorityRejected(t *testing.T) {
	probe := &nativePolicyBrokerProbe{}
	runner := &Runner{Workspace: t.TempDir(), Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Tools: []string{"Read"}}}}}
	_, err := runner.RunTurn(t.Context(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", Tools: &workflows.InvocationTools{Binding: probe}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "direct native tools with managed broker") || probe.calls.Load() != 0 {
		t.Fatal("genuine mixed native/broker authority was admitted", err, probe.calls.Load())
	}
}
