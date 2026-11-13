package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"gopkg.in/yaml.v3"
)

func releaseEnvironment(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{"CI": "true", "GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "dk-code-factory/agent-workflow", "GITHUB_EVENT_NAME": "push", "GITHUB_REF": "refs/tags/v0.2.0", "GITHUB_WORKFLOW_REF": "dk-code-factory/agent-workflow/.github/workflows/release.yml@refs/tags/v0.2.0", "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_SHA": strings.Repeat("a", 40)} {
		t.Setenv(k, v)
	}
}
func TestReleaseAuthorizationRejectsOtherCIAndPublication(t *testing.T) {
	releaseEnvironment(t)
	if err := authorizeCI(true, "native-codex-smoke", false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ k, v string }{{"GITHUB_REPOSITORY", "fork/repo"}, {"GITHUB_REF", "refs/heads/main"}, {"GITHUB_EVENT_NAME", "workflow_dispatch"}, {"GITHUB_WORKFLOW_REF", "dk-code-factory/agent-workflow/.github/workflows/test.yml@refs/tags/v0.2.0"}, {"GITHUB_RUN_ID", ""}, {"GITHUB_RUN_ATTEMPT", "0"}, {"GITHUB_SHA", "development"}} {
		t.Run(change.k, func(t *testing.T) {
			t.Setenv(change.k, change.v)
			if authorizeCI(true, "smoke", false) == nil {
				t.Fatal("foreign execution authorized")
			}
		})
	}
	if authorizeCI(false, "smoke", false) == nil || authorizeCI(true, "", false) == nil || authorizeCI(true, "publication", true) == nil {
		t.Fatal("implicit or publication authorization")
	}
}

func TestDirectSmokeRoutesCLIProvidersWithoutNativeTasks(t *testing.T) {
	for _, provider := range []string{"claude-cli", "codex-cli"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			command := filepath.Join(root, "fake-provider")
			payload := `{"type":"result","subtype":"success","result":"CONFORMANCE_OK","usage":{"input_tokens":3,"output_tokens":1}}`
			version := "2.1.323"
			if provider == "codex-cli" {
				payload = "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"CONFORMANCE_OK\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}"
				version = "codex-cli 0.167.0"
			}
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' '" + version + "';exit;fi\ncat >/dev/null\nprintf '%s\\n' '" + payload + "'\n"
			if err := os.WriteFile(command, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			runner := &clirunner.Runner{Workspace: root, Config: clirunner.Config{Agents: map[string]clirunner.Agent{"arbitrary-role": {Provider: provider, Command: command}}}}
			report := evidence{ProviderVersions: map[string]string{}}
			err := directScenario(context.Background(), runner, "arbitrary-role", manifest{Driver: "agent", Prompt: "public marker"}, root, &report)
			if err != nil {
				t.Fatal(err)
			}
			if report.Usage == nil || report.Usage.Invocations != 1 || report.ProviderVersions[provider] != version || len(report.Invocations) != 1 {
				t.Fatalf("missing physical provider evidence: %+v", report)
			}
		})
	}
}

func TestManagedAndNativeWorkflowRouting(t *testing.T) {
	root := t.TempDir()
	ref, err := writeManagedWorkflow(root, "claude", "bounded", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ref))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "action/conformance-turn") || strings.Contains(string(raw), "agent/run") {
		t.Fatal(string(raw))
	}
	if _, err = writeWorkflow(root, "claude", "bounded", false); err == nil {
		t.Fatal("native agent/run accepted Claude")
	}
	if _, err = writeWorkflow(root, "codex", "bounded", false); err != nil {
		t.Fatal(err)
	}
	if observedBothProviders([]observedInvocation{{"one", "claude-sdk", "v"}}) {
		t.Fatal("single provider satisfied mixed assertion")
	}
}

func TestReleaseWorkflowGatesAndSequentialPhases(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs       any    `yaml:"needs"`
			Environment string `yaml:"environment"`
			If          string `yaml:"if"`
			Timeout     int    `yaml:"timeout-minutes"`
			Steps       []struct {
				Run  string         `yaml:"run"`
				With map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	jobs := workflow.Jobs
	needs := func(name string, want ...string) {
		t.Helper()
		raw, _ := json.Marshal(jobs[name].Needs)
		var actual []string
		if err := json.Unmarshal(raw, &actual); err != nil {
			t.Fatal(err)
		}
		if strings.Join(actual, ",") != strings.Join(want, ",") {
			t.Fatalf("%s dependencies %s", name, raw)
		}
	}
	needs("provider-e2e", "archives", "images")
	needs("certify", "archives", "images", "provider-e2e")
	needs("publish", "archives", "images", "provider-e2e", "certify")
	if jobs["provider-e2e"].Environment != "pre-release" || jobs["provider-e2e"].Timeout != 120 || jobs["certify"].Environment != "release-certification" {
		t.Fatal("missing provider/human protection")
	}
	condition := jobs["certify"].If
	for _, name := range []string{"archives", "images", "provider-e2e"} {
		if !strings.Contains(condition, "needs."+name+".result == 'success'") {
			t.Fatal("failed/skipped coverage can reach environment", condition)
		}
	}
	var runs string
	for _, step := range jobs["provider-e2e"].Steps {
		runs += step.Run + "\n"
		if _, ok := step.With["overwrite"]; ok {
			t.Fatal("E2E artifact can be replaced")
		}
	}
	positions := []int{strings.Index(runs, "release_e2e.py setup"), strings.Index(runs, "release_e2e.py smoke"), strings.Index(runs, "release_e2e.py matrix"), strings.Index(runs, "release_e2e.py aggregate")}
	for i, p := range positions {
		if p < 0 || (i > 0 && p <= positions[i-1]) {
			t.Fatal("E2E phases not sequential", positions)
		}
	}
	if strings.Contains(runs, "build-worker-image") || strings.Contains(runs, "go build") {
		t.Fatal("certification rebuilds release artifacts")
	}
}

func TestReleaseConfigurationsAreAdmissibleForEveryScenario(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("python3", "-c", `import sys,json;from pathlib import Path;sys.path.insert(0,'../../scripts');from release_e2e import configuration;print(json.dumps([configuration(Path('/private'),Path('/package'),r,'sha256:'+'1'*64) for r in [None,'docker','podman']]))`)
	data, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var configs []json.RawMessage
	if err = json.Unmarshal(data, &configs); err != nil {
		t.Fatal(err)
	}
	for i, raw := range configs {
		path := filepath.Join(root, fmt.Sprint(i)+".json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = clirunner.LoadConfig(path); err != nil {
			t.Fatal("release scenario configuration rejected", i, err)
		}
	}
}

func TestFixtureControllerNestedWorkersAtCapacityOne(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the fake bridge")
	}
	root := t.TempDir()
	bridge := filepath.Join(root, "bridge.mjs")
	metadata := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "package.json"), []byte(`{"version":"0.3.319"}`), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
import readline from 'node:readline';
const emit=x=>process.stdout.write(JSON.stringify({version:2,...x})+'\n');
let prompt='';
const finish=()=>{emit({type:'result',session_id:'fixture',status:'succeeded',provider_version:'claude-agent-sdk@0.3.319',outputs:{text:'CONFORMANCE_OK',usage:{input_tokens:2,output_tokens:1}}});process.exit(0)};
const call=(id,tool,args)=>{emit({type:'tool_request',call_id:id,server:'engine',tool,arguments:args});emit({type:'blocked',call_ids:[id]})};
emit({type:'hello',provider_version:'claude-agent-sdk@0.3.319'});
for await(const line of readline.createInterface({input:process.stdin})){
 const f=JSON.parse(line);
 if(f.type==='turn_start'){
  prompt=f.prompt;emit({type:'session',session_id:'fixture'});emit({type:'usage',session_id:'fixture',data:{usage:{input_tokens:2,output_tokens:1},observation_key:'first'}});
  if(prompt==='LEAF'){finish()}else{call('spawn','worker_spawn',{role:'claude',turns:prompt==='ROOT'?4:1,prompt:prompt==='ROOT'?'CHILD':'LEAF'})}
 }else if(f.type==='tool_result'){
  if(f.error){emit({type:'result',status:'failed',error:f.error});process.exit(1)}
  if(f.call_id==='spawn'){call('wait','worker_wait',{worker_id:f.result.worker.id,message_id:f.result.message.id})}
  else{if(f.result.status!=='succeeded'){emit({type:'result',status:'failed',error:JSON.stringify(f.result)});process.exit(1)}finish()}
 }
}
`
	if err := os.WriteFile(bridge, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := clirunner.Config{Agents: map[string]clirunner.Agent{"claude": {Provider: "claude-sdk", Bridge: bridge, Tools: []string{}, MCPGrants: []workflows.ToolGrant{{Server: "engine", Tool: "worker_spawn"}, {Server: "engine", Tool: "worker_wait"}}, Authentication: clirunner.Authentication{Mode: "native"}, Limits: clirunner.Limits{MaxWorkers: 4, MaxDepth: 3, MaxTurns: 4}}}}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "agents.yml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := writeManagedWorkflow(root, "claude", "ROOT", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fake-no-network-token")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveFixture(ctx, root, path) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture controller did not stop")
		}
	}()
	client := controller.NewClient(filepath.Join(root, ".agent-workflow"))
	for client.Call(ctx, "GET", "/health", nil, nil) != nil {
		select {
		case err := <-done:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	var run workflows.Run
	if err = client.Call(ctx, "POST", "/runs", controller.StartRequest{Ref: ref}, &run); err != nil {
		t.Fatal(err)
	}
	for {
		if err = client.Call(ctx, "GET", "/runs/"+run.ID, nil, &run); err != nil {
			t.Fatal(err)
		}
		if run.Status != workflows.RunStatusQueued && run.Status != workflows.RunStatusRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("nested fixture deadlocked", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if run.Status != workflows.RunStatusSucceeded {
		t.Fatalf("managed fixture did not succeed: %+v", run)
	}
	store, err := workflows.OpenSQLiteInspection(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workers, err := (&workflows.WorkerManager{Store: store}).List(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 2 {
		t.Fatalf("expected child and grandchild, got %+v", workers)
	}
	usage, err := store.Usage(ctx, run.ID)
	if err != nil || usage.Summary.Invocations != 3 {
		t.Fatal("missing physical provider usage", usage, err)
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStaleOwnershipAfterRecoveryUsesCurrentPositiveControl(t *testing.T) {
	ctx := context.Background()
	store, err := workflows.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: "recovered-run", Owner: "old-owner", Generation: 1, Status: workflows.RunStatusNeedsAttention, FailureClass: "owner_loss"}
	if err = store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	original := runtimeexec.Record{RunID: run.ID, Generation: 1}
	if err = verifyStaleOwnership(ctx, store, original); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Generation != 2 || current.Status != workflows.RunStatusNeedsAttention {
		t.Fatal("stale owner check dispatched a new turn", current)
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
}
