package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Both providers consume exactly this YAML, including the same portable definition.
const nativeContinuationWorkflow = `jobs:
  review:
    steps:
      - id: first
        uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: First task
          input: {exact: 9007199254740993}
          turns: 2
          output: {format: json, repair-attempts: 0, schema: {type: object, required: [approved], properties: {approved: {type: boolean}}}}
      - id: gate
        uses: input/wait
        with: {prompt: Continue, evidence-id: native-evidence}
      - id: follow
        uses: agent/run
        with:
          agent: ${{ steps.first.outputs.agent }}
          prompt: Continue task
          output: {format: json, repair-attempts: 0, schema: {type: object, required: [approved], properties: {approved: {type: boolean}}}}
`

func TestNativeRuntimeContinuationUsesAdmittedSettingsAfterRestart(t *testing.T) {
	for _, provider := range []string{"codex-app-server", "claude-sdk"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, source := range map[string]string{"main.yml": nativeContinuationWorkflow, "reviewer.yaml": "name: portable-reviewer\ndescription: Portable review\ninstructions: DEFINITION_INSTRUCTIONS\n"} {
				if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, log := nativeRuntimeFixture(t, root, provider)
			// No Codex executable exists in PATH. A Claude-only run must not discover or
			// probe Codex, even though its production adapter normally defaults to it.
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			if _, err := exec.LookPath("codex"); err == nil {
				t.Fatal("fixture unexpectedly exposes Codex")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			store, err := workflows.OpenSQLite(state)
			if err != nil {
				t.Fatal(err)
			}
			initialStore := store
			t.Cleanup(func() { _ = initialStore.Close() })
			executor, workers := Executor(cfg, store, root, 1)
			stop := serveNativeWorkers(t, ctx, workers)
			queued, err := executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			waiting, err := executor.Resume(ctx, queued.ID, nil, false)
			if err != nil || waiting.Status != workflows.RunStatusWaiting {
				t.Fatalf("first task: %+v %v", waiting, err)
			}
			_, saved, err := store.Load(ctx, queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			first, err := workers.List(ctx, queued.ID)
			if err != nil || len(first) != 1 {
				t.Fatalf("workers: %+v %v", first, err)
			}
			if first[0].Role != "native" || first[0].Remaining != 1 || first[0].TaskTurns != 2 || first[0].SessionID != "retained-session" {
				t.Fatalf("initial binding: %+v", first[0])
			}
			initialUsage, err := store.Usage(ctx, queued.ID)
			if err != nil || initialUsage.Summary.Invocations != 1 {
				t.Fatalf("initial usage: %+v %v", initialUsage, err)
			}
			stop()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(root, "workflows")); err != nil {
				t.Fatal(err)
			}
			store, err = workflows.OpenSQLite(state)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			// Current settings intentionally select another backend, model and policy.
			// Recovery must use only the immutable original role snapshot.
			changed := clirunner.Config{AgentRuntime: "replacement", Agents: map[string]clirunner.Agent{"replacement": {Provider: "claude-sdk", Command: "must-not-execute", Model: "changed-model", Limits: clirunner.Limits{MaxTurns: 99}}}}
			executor, workers = Executor(changed, store, root, 1)
			stop = serveNativeWorkers(t, ctx, workers)
			gates, err := store.Inputs(ctx, queued.ID)
			if err != nil || len(gates) != 1 {
				t.Fatalf("gate: %+v %v", gates, err)
			}
			if _, err := store.AcceptInput(ctx, queued.ID, gates[0].ID, workflows.ExternalInputAnswer{AnswerID: "answer-native", EvidenceID: "native-evidence", Value: json.RawMessage(`true`)}); err != nil {
				t.Fatal(err)
			}
			if err := store.QueueInputContinuations(ctx, 128); err != nil {
				t.Fatal(err)
			}
			done, err := executor.Resume(ctx, queued.ID, nil, false)
			if err != nil || done.Status != workflows.RunStatusSucceeded {
				t.Fatalf("continued task: %+v %v", done, err)
			}
			final, err := workers.List(ctx, queued.ID)
			if err != nil || len(final) != 1 {
				t.Fatal(final, err)
			}
			if final[0].ID != first[0].ID || final[0].SessionID != first[0].SessionID || final[0].SessionOwnerID != first[0].SessionOwnerID || final[0].Remaining != 0 || final[0].TaskTurns != 2 || final[0].TurnDuration != first[0].TurnDuration || !reflect.DeepEqual(final[0].Access, first[0].Access) {
				t.Fatalf("continued bindings/allowance changed: before=%+v after=%+v", first[0], final[0])
			}
			messages, err := workers.Messages(ctx, queued.ID, first[0].ID)
			if err != nil || len(messages) != 2 || messages[0].Sequence != 1 || messages[1].Sequence != 2 {
				t.Fatal(messages, err)
			}
			usage, err := store.Usage(ctx, queued.ID)
			if err != nil || usage.Summary.Invocations != 2 || usage.Summary.InputTokens == nil || *usage.Summary.InputTokens != 14 || usage.Summary.OutputTokens == nil || *usage.Summary.OutputTokens != 6 {
				t.Fatalf("usage lost/replenished: %+v %v", usage, err)
			}
			_, retained, err := store.Load(ctx, queued.ID)
			if err != nil || string(saved.Roles) != string(retained.Roles) || !reflect.DeepEqual(saved.Hashes, retained.Hashes) {
				t.Fatal("retained admission changed", err)
			}
			records, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(records)), "\n")
			if len(lines) != 2 {
				t.Fatalf("unexpected provider turn count: %s", records)
			}
			for i, line := range lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["model"] != "retained-model" || !strings.Contains(record["instructions"].(string), "DEFINITION_INSTRUCTIONS") || !strings.Contains(record["instructions"].(string), "WORKSPACE_INSTRUCTIONS") {
					t.Fatalf("settings changed: %v", record)
				}
				if i == 0 && !strings.Contains(record["prompt"].(string), "9007199254740993") {
					t.Fatalf("typed input lost: %v", record)
				}
				if i == 1 && record["session"] != "retained-session" {
					t.Fatalf("new session on continuation: %v", record)
				}
			}
		})
	}
}

func serveNativeWorkers(t *testing.T, ctx context.Context, workers *workflows.WorkerManager) func() {
	t.Helper()
	if err := workers.Init(ctx); err != nil {
		t.Fatal(err)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); workers.Serve(serviceCtx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Error("worker service did not stop within 5 seconds")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func nativeRuntimeFixture(t *testing.T, root, provider string) (clirunner.Config, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("local Python fixture required", err)
	}
	log := filepath.Join(root, "native-turns.jsonl")
	command := filepath.Join(root, "provider")
	fixture := `import sys,json,os
provider=` + strconv.Quote(provider) + `
log=` + strconv.Quote(log) + `
session=''
settings={}
def out(v): print(json.dumps(v),flush=True)
def record(prompt):
 with open(log,'a') as f: f.write(json.dumps(dict(model=settings.get('model'),instructions=settings.get('instructions'),prompt=prompt,session=session))+'\n')
if provider=='claude-sdk':
 assert os.environ.get('CLAUDE_CODE_OAUTH_TOKEN')=='local-native-token'
 out({'version':2,'type':'hello','provider_version':'claude-agent-sdk@` + clirunner.ClaudeSDKVersion + `'})
 f=json.loads(sys.stdin.readline()); settings=f;session=f.get('session_id') or '';record(f['prompt'])
 out({'version':2,'type':'broker_model_identity','data':{'requested':f['model'],'resolved':f['model']}})
 assert json.loads(sys.stdin.readline())['type']=='model_bound'
 out({'version':2,'type':'session','session_id':'retained-session'})
 response='continued' if session else 'first'
 usage={'input_tokens':7,'output_tokens':3,'cache_read_input_tokens':0,'cache_creation_input_tokens':0}
 out({'version':2,'type':'broker_model_start','session_id':'retained-session','data':{'response_id':response,'model':f['model']}})
 out({'version':2,'type':'broker_batch','session_id':'retained-session','data':{'response_id':response,'model':f['model'],'stop_reason':'end_turn','calls':[],'usage':usage}})
 out({'version':2,'type':'result','session_id':'retained-session','status':'succeeded','provider_status':'success','outputs':{'text':'{"approved":true}','usage':usage,'cost_usd':0}})
else:
 for line in sys.stdin:
  f=json.loads(line);m=f.get('method');i=f.get('id');p=f.get('params') or {}
  if m=='initialize': out({'id':i,'result':{'userAgent':'codex_cli_rs/` + clirunner.CodexProtocolVersion + ` (fixture)'}})
  elif m=='account/login/start': out({'id':i,'result':{}})
  elif m=='skills/list': out({'id':i,'result':{'data':[{'cwd':p['cwds'][0],'skills':[],'errors':[]}]}})
  elif m in ('thread/start','thread/resume'):
   settings=dict(model=p.get('model'),instructions=p.get('baseInstructions'));session=p.get('threadId') or '';out({'id':i,'result':{'thread':{'id':'retained-session'}}})
  elif m=='turn/start':
   record(json.dumps(p.get('input')))
   out({'id':i,'result':{'turn':{'id':'retained-turn'}}})
   out({'method':'thread/tokenUsage/updated','params':{'threadId':'retained-session','tokenUsage':{'total':{'inputTokens':14 if session else 7,'outputTokens':6 if session else 3}}}})
   out({'method':'item/completed','params':{'threadId':'retained-session','turnId':'retained-turn','item':{'type':'agentMessage','text':'{"approved":true}'}}})
   out({'method':'turn/completed','params':{'threadId':'retained-session','turn':{'id':'retained-turn','status':'completed'}}})
  elif i is not None: out({'id':i,'error':{'code':-32601,'message':'unsupported fixture method'}})
`
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli " + clirunner.CodexProtocolVersion + "'; exit 0; fi\nexec '" + python + "' -c '" + strings.ReplaceAll(fixture, "'", "'\\''") + "'\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "bridge.mjs")
	if err := os.WriteFile(bridge, []byte("// local protocol fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "package.json"), []byte(`{"version":"`+clirunner.ClaudeSDKVersion+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	auth := clirunner.Authentication{Mode: "api-key", CredentialEnv: "NATIVE_RUNTIME_TEST_TOKEN"}
	tools := []string(nil)
	if provider == "claude-sdk" {
		auth.Mode = "native"
		tools = []string{}
	}
	t.Setenv("NATIVE_RUNTIME_TEST_TOKEN", "local-native-token")
	cfg := clirunner.Config{AgentRuntime: "native", Agents: map[string]clirunner.Agent{"native": {Provider: provider, Command: command, Bridge: bridge, Model: "retained-model", InstructionText: "WORKSPACE_INSTRUCTIONS", Tools: tools, Authentication: auth, Limits: clirunner.Limits{MaxTurns: 4, TurnTimeout: "15s"}}}}
	return cfg, log
}
