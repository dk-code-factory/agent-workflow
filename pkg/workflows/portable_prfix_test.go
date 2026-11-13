package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"gopkg.in/yaml.v3"
)

// portableFixtureCommand is bounded independently of the engine deadline. A
// broken helper must not leave the fake Docker server waiting for a subprocess
// after the workflow's cancellation/cleanup path has started.
func portableFixtureCommand(t *testing.T, root string, argv []string, input []byte, env []string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = root
	command.Env = append(os.Environ(), env...)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		t.Fatalf("fixture command %q: %v\n%s", argv, err, stderr.String())
	}
	return out
}

func portableFixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	argv := append([]string{"git", "-c", "commit.gpgsign=false"}, args...)
	return strings.TrimSpace(string(portableFixtureCommand(t, root, argv, nil, nil)))
}

func portableFixtureGitHub(t *testing.T, root, head, base string) {
	t.Helper()
	directory := t.TempDir()
	metadata := map[string]any{
		"html_url": "https://github.com/fixture/repository/pull/1", "number": 1,
		"state": "open", "merged": false, "draft": false,
		"updated_at": "2026-01-01T00:00:00Z", "mergeable": true, "mergeable_state": "clean",
		"fixture_ci_status": "in_progress",
		"user":              map[string]any{"id": 1, "login": "fixture"},
		"head":              map[string]any{"sha": head, "ref": "fix", "repo": map[string]any{"full_name": "fixture/repository"}},
		"base":              map[string]any{"sha": base, "ref": "main", "repo": map[string]any{"full_name": "fixture/repository"}},
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(directory, "metadata.json")
	if err = os.WriteFile(metadataPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("portable deterministic fixture requires Python 3:", err)
	}
	script := fmt.Sprintf(`#!%s
import json, os, pathlib, sys
args=sys.argv[1:]
assert args[0]=='api', args
method=args[args.index('--method')+1]
route=args[args.index('--method')+2].split('?')[0]
metadata=json.loads(pathlib.Path(os.environ['AW_PORTABLE_METADATA']).read_text())
with open(os.environ['AW_PORTABLE_CALLS'],'a') as stream:
    stream.write(json.dumps([method,route])+'\n')
if method=='GET' and route=='user':
    result={'id':1,'login':'fixture'}
elif method=='GET' and route=='repos/fixture/repository/pulls/1':
    result=metadata
elif method=='POST' and route=='graphql':
    request=json.load(sys.stdin)
    assert request['query'].startswith('query('), request
    result={'data':{'repository':{'pullRequest':{'reviewThreads':{'totalCount':0,'nodes':[], 'pageInfo':{'hasNextPage':False,'endCursor':None}}}}}}
elif method=='GET' and route.endswith('/check-runs'):
    state=metadata['fixture_ci_status']
    result={'total_count':1,'check_runs':[{'id':11,'head_sha':metadata['head']['sha'],'name':'Required','status':state,'conclusion':'success' if state=='completed' else None,'html_url':'https://github.com/fixture/repository/checks/11'}]}
elif method=='GET' and route.endswith(('/comments','/reviews','/statuses')):
    result=[]
else:
    raise SystemExit('unexpected fake GitHub request: '+repr(args))
print(json.dumps(result))
`, python)
	if err = os.WriteFile(filepath.Join(directory, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AW_PORTABLE_METADATA", metadataPath)
	t.Setenv("AW_PORTABLE_CALLS", filepath.Join(directory, "calls.jsonl"))
}

type portablePrepared struct {
	LogicalRun string `json:"logical_run"`
	Plan       string `json:"plan"`
	Workflow   string `json:"workflow"`
	Config     string `json:"config"`
}

func portableFixturePrepare(t *testing.T, root, instructions string, settings ...map[string]any) portablePrepared {
	t.Helper()
	request := map[string]any{
		"pr": "https://github.com/fixture/repository/pull/1", "instructions": instructions,
		"image":   "fixture/python-tools@sha256:" + strings.Repeat("a", 64),
		"roles":   map[string]string{"leaf": "reviewer.toml", "reviewer": "reviewer.toml", "verifier": "reviewer.toml", "fixer": "reviewer.toml", "audience": "reviewer.toml"},
		"options": map[string]any{"tasks": []any{map[string]any{"skill": "local:review"}}, "finalizers": []any{}, "reviewers": []any{}},
		"adapters": map[string]any{"local:review": map[string]any{
			"kind": "review", "definition": "reviewer", "assignment": "Inspect file.txt and report sourced findings.",
			"checklist": []any{map[string]any{"id": "inspect", "text": "Inspect the changed source."}},
		}},
	}
	if len(settings) != 0 {
		request["settings"] = settings[0]
	}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	script := "import json,pathlib,sys; from prfix.prepare import prepare; print(json.dumps(prepare(pathlib.Path.cwd(),json.load(sys.stdin))))"
	helperPath := filepath.Join(root, "example", "workflows", "helpers")
	out := portableFixtureCommand(t, root, []string{"python3", "-B", "-c", script}, input, []string{"PYTHONPATH=" + helperPath})
	var prepared portablePrepared
	if err = json.Unmarshal(out, &prepared); err != nil || prepared.Plan == "" || prepared.Workflow == "" {
		t.Fatalf("prepare did not return an admitted closure: %s (%v)", out, err)
	}
	return prepared
}

type portableInterruptedSession struct {
	*nativeTaskFixture
	first   atomic.Bool
	entered chan SessionRequest
}

func (f *portableInterruptedSession) RunTurn(ctx context.Context, request SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	if f.first.CompareAndSwap(false, true) {
		session := "session-" + request.ParentWorkerID
		emit(ProviderEvent{Type: "session", SessionID: session})
		f.entered <- request
		<-ctx.Done()
		return SessionResult{SessionID: session, Status: RunStatusInterrupted}, ctx.Err()
	}
	return f.nativeTaskFixture.RunTurn(ctx, request, emit)
}

func portableFixtureAssignment(input any) map[string]any {
	switch value := input.(type) {
	case map[string]any:
		if _, ok := value["task"].(map[string]any); ok {
			return value
		}
		for _, child := range value {
			if found := portableFixtureAssignment(child); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := portableFixtureAssignment(child); found != nil {
				return found
			}
		}
	}
	return nil
}

func TestPortablePRFixStockEngineRetainsSessionsAndInputSensitiveCoverage(t *testing.T) {
	// Fake container guardians are subprocesses of this instrumented binary.
	// Keep race detection enabled without paying its exit sleep for every hook.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_TEMPLATE_DIR", t.TempDir())
	e, store, docker := containerExecutor(t)
	docker.commandTimeout = 15 * time.Second
	root := e.WorkspaceDir
	if err := os.CopyFS(filepath.Join(root, "example"), os.DirFS(filepath.Join("..", "..", "examples", "pr-fix"))); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"file.txt":      "base\n",
		"reviewer.toml": "name='fixture'\ndescription='Deterministic report fixture'\ndeveloper_instructions='Inspect admitted inputs.'\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	portableFixtureGit(t, root, "init", "-b", "main")
	portableFixtureGit(t, root, "config", "user.name", "Fixture")
	portableFixtureGit(t, root, "config", "user.email", "fixture@example.invalid")
	portableFixtureGit(t, root, "add", ".")
	portableFixtureGit(t, root, "commit", "-m", "Initial fixture")
	base := portableFixtureGit(t, root, "rev-parse", "HEAD")
	portableFixtureGit(t, root, "switch", "-c", "fix")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	portableFixtureGit(t, root, "commit", "-am", "Change source")
	head := portableFixtureGit(t, root, "rev-parse", "HEAD")
	portableFixtureGit(t, root, "remote", "add", "origin", "https://github.com/fixture/repository.git")
	portableFixtureGitHub(t, root, head, base)
	prepared := portableFixturePrepare(t, root, "Review the original inputs.")
	closure := filepath.Join(root, filepath.Dir(prepared.Workflow))
	entries, err := os.ReadDir(closure)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yml" || entry.Name() == "agents.yml" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(closure, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		workflow, parseErr := Parse(data)
		if parseErr == nil {
			parseErr = Validate(workflow)
		}
		if parseErr != nil {
			t.Fatalf("generated closure %s failed stock validation: %v", entry.Name(), parseErr)
		}
	}
	configuration, err := os.ReadFile(filepath.Join(root, prepared.Config))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Execution runtimeexec.Policy `yaml:"execution"`
	}
	if err = yaml.Unmarshal(configuration, &config); err != nil {
		t.Fatal(err)
	}
	e.CommandExecution = config.Execution

	var mu sync.Mutex
	assignments := map[string]map[string]any{}
	turns := map[string]int{}
	var reviewed, verificationAgents []string
	failedAgent := ""
	sourceHash := sha256.Sum256([]byte("changed\n"))
	fake := &nativeTaskFixture{respond: func(request SessionRequest) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		assignment := portableFixtureAssignment(request.Input)
		if assignment != nil {
			assignments[request.ParentWorkerID] = assignment
		} else {
			assignment = assignments[request.ParentWorkerID]
		}
		if assignment == nil {
			return nil, fmt.Errorf("native task lost its typed application assignment: %#v", request.Input)
		}
		task := assignment["task"].(map[string]any)
		kind, _ := task["kind"].(string)
		turns[request.ParentWorkerID]++
		if kind == "review" && failedAgent == "" {
			failedAgent = request.ParentWorkerID
			return map[string]any{"text": "retained partial analysis"}, ErrUsageExhausted
		}
		incomplete := kind == "review" && (turns[request.ParentWorkerID] == 1 || request.ParentWorkerID == failedAgent && turns[request.ParentWorkerID] == 2)
		checks := []any{}
		for _, raw := range task["checklist"].([]any) {
			check := raw.(map[string]any)
			status := "completed"
			if incomplete {
				status = "incomplete"
			}
			checks = append(checks, map[string]any{"id": check["id"], "status": status, "evidence": []string{"source"}})
		}
		report := map[string]any{
			"complete": !incomplete, "checklist": checks, "blockers": []any{},
			"evidence": []any{map[string]any{"id": "source", "path": "file.txt", "sha256": hex.EncodeToString(sourceHash[:])}},
			"findings": []any{}, "dispositions": []any{},
		}
		if kind == "review" {
			reviewed = append(reviewed, request.ParentWorkerID)
			report["findings"] = []any{map[string]any{"id": "candidate", "text": "Check the changed source against the fixture specification.", "sources": []string{"source"}}}
		}
		if kind == "verification" {
			verificationAgents = append(verificationAgents, request.ParentWorkerID)
			for _, raw := range assignment["findings"].([]any) {
				finding := raw.(map[string]any)
				report["dispositions"] = append(report["dispositions"].([]any), map[string]any{"finding_id": finding["id"], "status": "rejected", "reason": "The inspected source matches this deterministic fixture's specification.", "sources": []string{"source"}})
			}
		}
		body, err := json.Marshal(report)
		return map[string]any{"text": string(body)}, err
	}}
	runner := &portableInterruptedSession{nativeTaskFixture: fake, entered: make(chan SessionRequest, 1)}
	e.Agents = runner
	e.Capacity = &Capacity{Slots: e.Slots}
	manager := &WorkerManager{Store: store, Slots: e.Slots, Capacity: e.Capacity, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return runner, nil, nil, nil
	}}
	if err := manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.Workers = manager
	e.BeforeFinalize = manager.StopRun
	serveWorkers(t, manager)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	secrets := map[string]string{"github_token": "deterministic-fixture-token"}
	request := RunRequest{Ref: prepared.Workflow, Inputs: map[string]any{"plan": prepared.Plan}, Secrets: secrets}
	firstContext, interrupt := context.WithCancel(ctx)
	defer interrupt()
	type outcome struct {
		run *Run
		err error
	}
	firstResult := make(chan outcome, 1)
	firstFinished := make(chan struct{})
	defer func() {
		interrupt()
		select {
		case <-firstFinished:
		case <-time.After(35 * time.Second):
			t.Error("interrupted portable executor did not stop before fixture teardown")
		}
	}()
	go func() {
		defer close(firstFinished)
		run, err := e.Run(firstContext, request)
		firstResult <- outcome{run, err}
	}()
	var interrupted SessionRequest
	select {
	case interrupted = <-runner.entered:
		interrupt()
	case <-time.After(30 * time.Second):
		t.Fatal("portable native session did not start")
	}
	var stopped outcome
	select {
	case stopped = <-firstResult:
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted portable run did not join")
	}
	if stopped.run == nil || stopped.err == nil {
		t.Fatalf("session interruption was not exercised: %+v", stopped)
	}
	messages, loadErr := manager.Messages(ctx, stopped.run.ID, interrupted.ParentWorkerID)
	if loadErr != nil || len(messages) != 1 || messages[0].Status != RunStatusInterrupted || messages[0].Result.SessionID == "" {
		t.Fatalf("original session did not retain an interrupted message: %+v %v", messages, loadErr)
	}
	var lostResponse, reconciled atomic.Bool
	var lostKey string
	var lossMu sync.Mutex
	docker.helper = func(argv []string, input []byte, environment []string) ([]byte, []byte, int, bool) {
		if len(argv) == 0 || argv[len(argv)-1] != "turn_result" {
			return nil, nil, 0, false
		}
		var hook struct {
			Phase   string `json:"phase"`
			Context struct {
				Operation string `json:"operation_key"`
			} `json:"context"`
		}
		if json.Unmarshal(input, &hook) != nil {
			return nil, []byte("invalid portable hook input"), 1, true
		}
		if hook.Phase == "reconcile" {
			lossMu.Lock()
			reconciled.Store(hook.Context.Operation == lostKey)
			lossMu.Unlock()
		}
		if hook.Phase != "execute" || !lostResponse.CompareAndSwap(false, true) {
			return nil, nil, 0, false
		}
		commandContext, cancelCommand := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancelCommand()
		command := exec.CommandContext(commandContext, "python3", "-B", filepath.Join(closure, "helpers", "action.py"), "turn_result")
		command.Dir, command.Env, command.Stdin = root, append(os.Environ(), environment...), bytes.NewReader(input)
		command.WaitDelay = time.Second
		output, executeErr := command.CombinedOutput()
		if executeErr != nil {
			return nil, output, 1, true
		}
		lossMu.Lock()
		lostKey = hook.Context.Operation
		lossMu.Unlock()
		return nil, []byte("injected portable helper response loss after retained application result"), 1, true
	}
	run, err := e.Resume(ctx, stopped.run.ID, secrets, true)
	if err == nil || !lostResponse.Load() {
		t.Fatalf("portable helper response loss was not exercised: run=%+v error=%v", run, err)
	}
	run, err = e.Resume(ctx, stopped.run.ID, secrets, true)
	if err != nil || run.Status != RunStatusSucceeded {
		status := "not admitted"
		if run != nil {
			status = run.Status
		}
		t.Fatalf("portable closure failed at capacity one: status=%s error=%v", status, err)
	}
	if run.Outputs["complete"] != true || run.Outputs["ready"] != false {
		t.Fatalf("selected completion did not remain separate from pending live CI: %#v", run.Outputs)
	}
	if len(reviewed) != 2 || reviewed[0] != reviewed[1] || len(verificationAgents) != 1 || verificationAgents[0] == reviewed[0] {
		t.Fatalf("lost original-session checklist or independent verification: reviews=%v verifiers=%v", reviewed, verificationAgents)
	}
	if reviewed[0] != failedAgent || turns[failedAgent] != 3 {
		t.Fatalf("eligible failure did not retain its original agent through retry and checklist: failed=%s reviewed=%v turns=%v", failedAgent, reviewed, turns)
	}
	if !reconciled.Load() || len(fake.requests) == 0 || fake.requests[0].ParentWorkerID != interrupted.ParentWorkerID || fake.requests[0].SessionID != messages[0].Result.SessionID {
		t.Fatalf("original interruption or lost effect was not reconciled: interrupted=%+v calls=%+v reconciled=%v", interrupted, fake.requests, reconciled.Load())
	}
	mu.Lock()
	originalCalls := len(fake.requests)
	mu.Unlock()
	if _, err = e.Resume(ctx, run.ID, secrets, false); err != nil {
		t.Fatal("completed portable checkpoint validation:", err)
	}
	if _, err = e.Run(ctx, request); err != nil {
		t.Fatal("unchanged input-sensitive report reuse:", err)
	}
	if len(fake.requests) != originalCalls {
		t.Fatalf("unchanged recovery or replacement duplicated provider work: %d -> %d", originalCalls, len(fake.requests))
	}
	changed := portableFixturePrepare(t, root, "Review the changed governing instructions.")
	if changed.LogicalRun != prepared.LogicalRun || changed.Plan == prepared.Plan {
		t.Fatal("changed instructions did not preserve logical budgets and create a new plan", prepared, changed)
	}
	if _, err = e.Run(ctx, RunRequest{Ref: changed.Workflow, Inputs: map[string]any{"plan": changed.Plan}, Secrets: secrets}); err != nil {
		t.Fatal("changed instructions replacement:", err)
	}
	if len(fake.requests) <= originalCalls {
		t.Fatal("changed instructions reused stale review coverage")
	}
	previousCalls := len(fake.requests)
	configured := portableFixturePrepare(t, root, "Review the changed governing instructions.", map[string]any{"scope": "changed configuration"})
	if configured.LogicalRun != changed.LogicalRun || configured.Plan == changed.Plan {
		t.Fatal("changed settings did not preserve logical budgets and create a new plan", changed, configured)
	}
	if _, err = e.Run(ctx, RunRequest{Ref: configured.Workflow, Inputs: map[string]any{"plan": configured.Plan}, Secrets: secrets}); err != nil {
		t.Fatal("changed configuration replacement:", err)
	}
	if len(fake.requests) <= previousCalls {
		t.Fatal("changed configuration reused stale review coverage")
	}
}
