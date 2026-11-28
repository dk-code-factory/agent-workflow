package clirunner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Run the maintained bridge with a deterministic SDK stream fixture. This
// exercises its terminal-loss aggregation, without a CLI or provider process.
func TestSkillChildBridgePartialUsageKeepsParentNamespaceSeparate(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	for _, callID := range []string{"parent", "parent:message", "ordinary-child"} {
		t.Run(callID, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"bridge.mjs", "isolation.mjs", "broker-stream.mjs"} {
				b, err := os.ReadFile(filepath.Join("../../bridge/claude", name))
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(root, name), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			sdk := `export const tool=()=>{}; export const createSdkMcpServer=()=>{};
export function query({options}) {return {
 initializationResult:async()=>({claude_code_version:'2.1.322',plugins_applied:true,hooks_applied:true,hook_on_failure_supported:true,commands:[{name:'aw-probe:probe'}]}),
 interrupt:async()=>{}, close(){},
 async *[Symbol.asyncIterator]() {
  const session_id='original-session', id=process.env.AW_FIXTURE_CALL_ID;
  const post=tool_use_id=>options.hooks.PostToolUse[0].hooks[0]({tool_use_id});
  const invoke=input=>options.hooks.PreToolUse[0].hooks[0]({...input,session_id,effort:{level:'high'}});
  yield {type:'system',subtype:'init',session_id,plugins:[{name:'aw-probe',path:options.plugins[0].path}],skills:['aw-probe:probe'],tools:['Skill','Read','Task']};
  await invoke({tool_name:'Skill',tool_use_id:'skill',tool_input:{skill:'aw-probe:probe',args:'exact'}});
  yield {type:'user',session_id,message:{content:[{type:'tool_result',tool_use_id:'skill'}]},tool_use_result:{success:true,commandName:'aw-probe:probe'}};
  await post('skill');
  yield {type:'stream_event',session_id,event:{type:'message_start',message:{id:'same-message',usage:{input_tokens:48,output_tokens:0}}}};
  yield {type:'stream_event',session_id,event:{type:'message_delta',usage:{output_tokens:9}}};
  const args={subagent_type:'aw-child-review',description:'Admitted review',prompt:'Review.',run_in_background:false};
  yield {type:'assistant',session_id,message:{id:'same-message',usage:{input_tokens:48,output_tokens:9},content:[{type:'tool_use',id,name:'Agent',input:args}]}};
  await invoke({tool_name:'Agent',tool_use_id:id,tool_input:args});
  const message={type:'assistant',session_id,parent_tool_use_id:id,message:{id:'same-message',usage:{input_tokens:32,output_tokens:6},content:[{type:'text',text:'ORIGINAL_CHILD_OUTPUT'}]}};
  yield message; yield message;
  yield {type:'user',session_id,message:{content:[{type:'tool_result',tool_use_id:id}]},tool_use_result:{usage:{input_tokens:16,output_tokens:3}}};
  await post(id);
  throw Error('synthetic original terminal missing');
 }
};}`
			for name, text := range map[string]string{
				"node_modules/@anthropic-ai/claude-agent-sdk/package.json": `{"type":"module","exports":"./sdk.mjs","version":"0.3.322"}`,
				"node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs":      sdk,
				"node_modules/zod/package.json":                            `{"type":"module","exports":"./index.mjs"}`,
				"node_modules/zod/index.mjs":                               `export const z={};`,
			} {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "node", filepath.Join(root, "bridge.mjs"))
			cmd.Dir = root
			cmd.Env = append(providerEnvironment(""), "HOME="+root, "AW_FIXTURE_CALL_ID="+callID)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var raw, diagnostic bytes.Buffer
			cmd.Stderr = &diagnostic
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			skill := map[string]any{"profile": claudeSkillChildProfile, "name": "probe", "qualified": "aw-probe:probe", "root": root, "arguments": "exact", "child": map[string]any{"name": "review", "type": "aw-child-review", "model": "claude-sonnet-4-6", "effort": "high", "max_turns": 2, "max_calls": 1, "background": false, "prompt": "Review.", "description": "Admitted review", "resources": []string{filepath.Join(root, "reference.txt")}}}
			var permits int
			result, protocolErr := bridgeTurn(ctx, io.TeeReader(stdout, &raw), &bridgeWriter{writer: stdin}, map[string]any{"workspace": root, "model": "claude-sonnet-4-6", "tools": []string{"Read", "Skill", "Agent"}, "skill": skill}, nil, nil, func() { _ = cmd.Process.Kill() }, func(context.Context, workflows.SkillChildObservation) error { permits++; return nil })
			_ = stdin.Close()
			exitErr := cmd.Wait()
			usage, _ := result.Outputs["usage"].(map[string]any)
			if ctx.Err() != nil || protocolErr == nil || exitErr == nil || permits != 1 || result.Outputs["usage_incomplete"] != true || fmt.Sprint(usage["input_tokens"]) != "80" || fmt.Sprint(usage["output_tokens"]) != "15" {
				t.Fatal("parent/child identity double counted original partials", callID, result, protocolErr, exitErr, permits, diagnostic.String(), raw.String())
			}
		})
	}
}
