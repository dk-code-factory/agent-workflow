package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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

	"github.com/dk-code-factory/agent-workflow/pkg/process"
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
    if 'reviewThreads' in request['query']:
        result={'data':{'repository':{'pullRequest':{'reviewThreads':{'totalCount':0,'nodes':[], 'pageInfo':{'hasNextPage':False,'endCursor':None}}}}}}
    else:
        result={'data':{'repository':{'pullRequest':{'headRefOid':metadata['head']['sha'],'isDraft':False,'mergeable':'MERGEABLE','mergeStateStatus':'CLEAN','reviewDecision':'APPROVED'}}}}
elif method=='GET' and route.endswith('/check-runs'):
    state=metadata['fixture_ci_status']
    result={'total_count':1,'check_runs':[{'id':11,'head_sha':metadata['head']['sha'],'name':'Required','status':state,'conclusion':metadata.get('fixture_ci_conclusion','success') if state=='completed' else None,'html_url':'https://github.com/fixture/repository/checks/11'}]}
elif method=='PUT' and route=='repos/fixture/repository/pulls/1/merge':
    payload=json.load(sys.stdin)
    assert payload['sha']==metadata['head']['sha'], payload
    metadata.update(merged=True,state='closed',merge_commit_sha='c'*40,merged_by={'id':1})
    pathlib.Path(os.environ['AW_PORTABLE_METADATA']).write_text(json.dumps(metadata))
    result={'merged':True,'sha':'c'*40}
elif method=='GET' and route=='repos/fixture/repository/git/commits/'+'c'*40:
    result={'sha':'c'*40,'parents':[{'sha':metadata['base']['sha']},{'sha':metadata['head']['sha']}]}
elif method=='POST' and route=='repos/fixture/repository/issues/1/comments':
    payload=json.load(sys.stdin)
    result={'id':123,'body':payload['body'],'user':{'id':1,'login':'fixture'}}
    metadata['fixture_comment']=result
    pathlib.Path(os.environ['AW_PORTABLE_METADATA']).write_text(json.dumps(metadata))
elif method=='GET' and route.endswith('/issues/1/comments'):
    result=[metadata['fixture_comment']] if 'fixture_comment' in metadata else []
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
	Workflow string `json:"workflow"`
	Config   string `json:"config"`
	Inputs   map[string]any
}

func portableFixturePrepare(t *testing.T, root, instructions string, settings ...map[string]any) portablePrepared {
	t.Helper()
	request := map[string]any{
		"pr": "https://github.com/fixture/repository/pull/1", "instructions": instructions,
		"image": "fixture/python-tools@sha256:" + strings.Repeat("a", 64),
		"roles": map[string]string{"leaf": "reviewer.toml", "reviewer": "reviewer.toml", "verifier": "reviewer.toml", "fixer": "reviewer.toml", "audience": "reviewer.toml"},
		"options": map[string]any{"tasks": []any{map[string]any{"skill": "local:review"}, "dk-workflows:dk-pr-publish"}, "finalizers": []any{}, "reviewers": []any{},
			"permissions": map[string]any{"reply_to_feedback": true}},
		"authorizations": map[string]any{"iterate": map[string]any{"request_id": "fixture", "instruction": "Review and reply to this fixture.", "source": "user:fixture"}},
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
	// The stock engine executes the checked-in entry point, including preparation.
	// The request is private Git metadata and never adds an untracked source file.
	original := map[string]any{"pr": request["pr"], "instructions": instructions, "authorizations": request["authorizations"]}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(".git", "request-"+actionHash(input)+".json")
	if err := os.WriteFile(filepath.Join(root, requestPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	controls := request["options"].(map[string]any)
	inputs := map[string]any{"request": filepath.ToSlash(requestPath), "adapters": request["adapters"]}
	for key, value := range controls {
		inputs[key] = value
	}
	if len(settings) != 0 {
		inputs["settings"] = settings[0]
	}
	return portablePrepared{Workflow: "examples/pr-fix/workflows/main.yml", Config: "examples/pr-fix/agents.yml", Inputs: inputs}
}

func portablePreparedIdentity(t *testing.T, root string, run *Run) (string, string, map[string][2]int) {
	t.Helper()
	reference, _ := run.Jobs["prepare"].Outputs["plan"].(string)
	if reference == "" {
		t.Fatal("preparation returned no plan", run.Jobs["prepare"])
	}
	data, err := os.ReadFile(filepath.Join(root, reference))
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		LogicalRun string `json:"logical_run"`
	}
	if err := json.Unmarshal(data, &plan); err != nil || plan.LogicalRun == "" {
		t.Fatal("invalid prepared identity", string(data), err)
	}
	ledger, err := sql.Open("sqlite", "file:"+filepath.Join(root, ".pr-fix-state", plan.LogicalRun+".sqlite")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	rows, err := ledger.Query("SELECT name, operation, amount, limit_value FROM reservations")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	reservations := map[string][2]int{}
	for rows.Next() {
		var name, operation string
		var amount, limit int
		if err := rows.Scan(&name, &operation, &amount, &limit); err != nil {
			t.Fatal(err)
		}
		reservations[name+":"+operation] = [2]int{amount, limit}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(reservations) == 0 {
		t.Fatal("completed preparation retained no budget reservations")
	}
	return plan.LogicalRun, reference, reservations
}

func portableAssertRetainedReservations(t *testing.T, before, after map[string][2]int) {
	t.Helper()
	for operation, value := range before {
		if got, ok := after[operation]; !ok || got != value {
			t.Fatalf("replacement changed retained allowance %s: %v -> %v (present=%v)", operation, value, got, ok)
		}
	}
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
	if err := os.CopyFS(filepath.Join(root, "examples", "pr-fix"), os.DirFS(filepath.Join("..", "..", "examples", "pr-fix"))); err != nil {
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
	beforeDefinitions := portableFixtureGit(t, root, "status", "--porcelain", "--", ".", ":(exclude).pr-fix-state")
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
			t.Fatalf("authored workflow %s failed stock validation: %v", entry.Name(), parseErr)
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
	e.CommandExecution.Image = "fixture/python-tools@sha256:" + strings.Repeat("a", 64)

	var mu sync.Mutex
	assignments := map[string]map[string]any{}
	turns := map[string]int{}
	var reviewed, verificationAgents []string
	failedAgent := ""
	audienceAgent := ""
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
		if kind == "publication_audience" && audienceAgent == "" {
			audienceAgent = request.ParentWorkerID
			return map[string]any{"text": "retained audience analysis"}, ErrUsageExhausted
		}
		incomplete := kind == "review" && (turns[request.ParentWorkerID] == 1 || request.ParentWorkerID == failedAgent && turns[request.ParentWorkerID] == 2) || kind == "publication_audience" && turns[request.ParentWorkerID] == 2
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
			report["operations"] = []any{map[string]any{"kind": "reply", "head": head, "target": base,
				"payload": map[string]any{"body": "Deterministic source-backed fixture reply."}}}
		}
		if kind == "publication_audience" {
			if request.ParentWorkerID != audienceAgent {
				return nil, fmt.Errorf("audience retry or checklist changed the original session")
			}
			report["audience"] = map[string]any{"fingerprint": assignment["payload"].(map[string]any)["fingerprint"], "suitable": true, "evidence": []string{"source"}}
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
	// This scenario includes recovery and multiple replacement runs at capacity one.
	// Keep the shorter command and cancellation guards while allowing slower CI.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	secrets := map[string]string{"github_token": "deterministic-fixture-token"}
	request := RunRequest{Ref: prepared.Workflow, Inputs: prepared.Inputs, Secrets: secrets}
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
	docker.helper = func(ctx context.Context, argv []string, input []byte, environment []string) ([]byte, []byte, int, bool) {
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
		commandContext, cancelCommand := context.WithTimeout(ctx, 20*time.Second)
		defer cancelCommand()
		command := exec.CommandContext(commandContext, "python3", "-B", filepath.Join(closure, "helpers", "action.py"), "turn_result")
		command.Dir, command.Env, command.Stdin = root, append(os.Environ(), environment...), bytes.NewReader(input)
		process.Configure(command)
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
	if audienceAgent == "" || turns[audienceAgent] != 3 {
		t.Fatalf("authored audience flow lost retained retry/checklist: agent=%s turns=%v", audienceAgent, turns)
	}
	originalLogical, originalPlan, originalReservations := portablePreparedIdentity(t, root, run)
	changed := portableFixturePrepare(t, root, "Review the changed governing instructions.")
	changedRun, err := e.Run(ctx, RunRequest{Ref: changed.Workflow, Inputs: changed.Inputs, Secrets: secrets})
	if err != nil {
		t.Fatal("changed instructions replacement:", err)
	}
	changedLogical, changedPlan, changedReservations := portablePreparedIdentity(t, root, changedRun)
	if changedLogical != originalLogical || changedPlan == originalPlan {
		t.Fatal("changed instructions did not preserve logical budgets and create a new plan", originalLogical, changedLogical, originalPlan, changedPlan)
	}
	portableAssertRetainedReservations(t, originalReservations, changedReservations)
	if len(fake.requests) <= originalCalls {
		t.Fatal("changed instructions reused stale review coverage")
	}
	previousCalls := len(fake.requests)
	configured := portableFixturePrepare(t, root, "Review the changed governing instructions.", map[string]any{"scope": "changed configuration"})
	configuredRun, err := e.Run(ctx, RunRequest{Ref: configured.Workflow, Inputs: configured.Inputs, Secrets: secrets})
	if err != nil || configuredRun == nil || configuredRun.Status != RunStatusSucceeded {
		t.Fatal("changed configuration replacement:", configuredRun, err)
	}
	configuredLogical, configuredPlan, configuredReservations := portablePreparedIdentity(t, root, configuredRun)
	if configuredLogical != changedLogical || configuredPlan == changedPlan || configuredPlan == originalPlan {
		t.Fatal("changed settings did not preserve logical budgets and create a new plan", changedLogical, configuredLogical, changedPlan, configuredPlan)
	}
	portableAssertRetainedReservations(t, changedReservations, configuredReservations)

	if len(fake.requests) <= previousCalls {
		t.Fatal("changed YAML settings reused stale native review coverage")
	}
	if after := portableFixtureGit(t, root, "status", "--porcelain", "--", ".", ":(exclude).pr-fix-state"); after != beforeDefinitions {
		t.Fatalf("execution wrote source definitions/configuration: %s", after)
	}
	if _, err := os.Stat(filepath.Join(root, "workflows", ".pr-fix-portable")); !os.IsNotExist(err) {
		t.Fatal("execution generated workflow files", err)
	}
}

func TestPortablePRFixAuthoredSelectionsAndPublicationGates(t *testing.T) {
	for _, scenario := range []string{"empty", "defaults", "ordered-parallel-finalizers", "ci-repair", "ci-polling-chunk", "merge-without-authorization", "merge-authorized"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_CONFIG_PARAMETERS", "")
			t.Setenv("GIT_TEMPLATE_DIR", t.TempDir())
			e, store, docker := containerExecutor(t)
			docker.commandTimeout = 15 * time.Second
			root := e.WorkspaceDir
			if err := os.CopyFS(filepath.Join(root, "examples", "pr-fix"), os.DirFS(filepath.Join("..", "..", "examples", "pr-fix"))); err != nil {
				t.Fatal(err)
			}
			entry := "examples/pr-fix/workflows/main.yml"
			if scenario == "ci-repair" {
				// Exercise the documented wrapper through the deepest CI audience.
				entry = "examples/pr-fix/workflows/fixture-wrapper.yml"
				wrapper := `on:
  workflow_call:
    inputs:
      request: {type: string, required: true}
      tasks: {type: array, required: true}
      finalizers: {type: array, required: true}
      permissions: {type: object, required: true}
    secrets: {github_token: {required: true}}
    outputs:
      complete: {value: '${{ jobs.maintain.outputs.complete }}'}
jobs:
  maintain:
    uses: examples/pr-fix/workflows/main.yml
    with:
      request: ${{ inputs.request }}
      tasks: ${{ inputs.tasks }}
      finalizers: ${{ inputs.finalizers }}
      permissions: ${{ inputs.permissions }}
    secrets: {github_token: '${{ secrets.github_token }}'}
`
				if err := os.WriteFile(filepath.Join(root, entry), []byte(wrapper), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "ci-polling-chunk" {
				// Scale only the chunk boundary to exercise rollover quickly. The
				// authored condition and outer polling controller stay intact.
				path := filepath.Join(root, "examples/pr-fix/workflows/ci-poll.yml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.ReplaceAll(data, []byte("100"), []byte("2"))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			portableFixtureGit(t, root, "init", "-b", "main")
			portableFixtureGit(t, root, "config", "user.name", "Fixture")
			portableFixtureGit(t, root, "config", "user.email", "fixture@example.invalid")
			portableFixtureGit(t, root, "add", ".")
			portableFixtureGit(t, root, "commit", "-m", "Fixture base")
			base := portableFixtureGit(t, root, "rev-parse", "HEAD")
			portableFixtureGit(t, root, "switch", "-c", "fix")
			if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			portableFixtureGit(t, root, "commit", "-am", "Fixture change")
			head := portableFixtureGit(t, root, "rev-parse", "HEAD")
			portableFixtureGit(t, root, "remote", "add", "origin", "https://github.com/fixture/repository.git")
			portableFixtureGitHub(t, root, head, base)
			updateMetadata := func(changes map[string]any) {
				b, err := os.ReadFile(os.Getenv("AW_PORTABLE_METADATA"))
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err = json.Unmarshal(b, &value); err != nil {
					t.Fatal(err)
				}
				for k, v := range changes {
					value[k] = v
				}
				b, _ = json.Marshal(value)
				if err = os.WriteFile(os.Getenv("AW_PORTABLE_METADATA"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			updateMetadata(map[string]any{"fixture_ci_status": "completed"})
			if scenario == "ci-repair" {
				updateMetadata(map[string]any{"fixture_ci_conclusion": "failure"})
			}
			var pollDelays atomic.Int32
			if scenario == "ci-polling-chunk" {
				updateMetadata(map[string]any{"fixture_ci_status": "in_progress"})
				docker.helper = func(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, int, bool) {
					if len(argv) == 2 && argv[0] == "sleep" && argv[1] == "10" {
						if pollDelays.Add(1) == 2 {
							updateMetadata(map[string]any{"fixture_ci_status": "completed"})
						}
						return nil, nil, 0, true
					}
					return nil, nil, 0, false
				}
			}
			original := map[string]any{"pr": "https://github.com/fixture/repository/pull/1", "instructions": "Maintain the exact fixture."}
			if scenario == "ci-repair" {
				original["authorizations"] = map[string]any{"reply": map[string]any{"request_id": "fixture-wrapper-reply", "instruction": "Publish the exact deterministic fixture reply.", "source": "user:fixture"}}
			}
			if scenario == "merge-authorized" {
				original["authorizations"] = map[string]any{"merge": map[string]any{"request_id": "fixture-merge", "instruction": "Merge this exact fixture PR.", "source": "user:fixture"}}
			}
			b, _ := json.Marshal(original)
			if err := os.WriteFile(filepath.Join(root, ".git", "request.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			inputs := map[string]any{"request": ".git/request.json"}
			switch scenario {
			case "empty":
				inputs["settings"] = map[string]any{"scope": "fixture-settings"}
				inputs["tasks"] = []any{}
				inputs["finalizers"] = []any{}
			case "ordered-parallel-finalizers":
				// Other scenarios keep capacity one. This group must admit both
				// reviewers before either returns, proving authored concurrency.
				e.MaxParallel = 2
				e.Slots = make(chan struct{}, 2)
				inputs["tasks"] = []any{[]any{"local:review", "local:review"}, "dk-workflows:dk-pr-description", "dk-workflows:dk-pr-description"}
				inputs["finalizers"] = []any{"dk-workflows:dk-pr-feedback-read", "dk-workflows:dk-ci"}
			case "ci-repair":
				inputs["tasks"] = []any{"dk-workflows:dk-pr-description", "dk-workflows:dk-ci"}
				inputs["finalizers"] = []any{}
				inputs["permissions"] = map[string]any{"reply_to_feedback": true}
			case "ci-polling-chunk":
				inputs["tasks"] = []any{"dk-workflows:dk-ci"}
				inputs["finalizers"] = []any{}
				inputs["limits"] = map[string]any{"review_cycles": 5, "retries_per_task": 1, "ci_repairs_per_cycle": 0, "ci_reruns_per_head": 3}
			case "merge-without-authorization", "merge-authorized":
				inputs["tasks"] = []any{"dk-workflows:dk-pr-reviewers-run"}
				inputs["finalizers"] = []any{"dk-workflows:dk-ci"}
			}
			e.CommandExecution = runtimeexec.Policy{Mode: "container", Image: "fixture/python-tools@sha256:" + strings.Repeat("a", 64), Runtime: "docker", NetworkMode: "bridge", Writable: true}
			var mu sync.Mutex
			var observed []string
			var reviewArrivals atomic.Int32
			reviewOverlap := make(chan struct{})
			reviewIDs := map[string]bool{}
			repairs, audiences := 0, 0
			sourceHash := sha256.Sum256([]byte("changed\n"))
			fake := &nativeTaskFixture{respond: func(request SessionRequest) (map[string]any, error) {
				mu.Lock()
				defer mu.Unlock()
				assignment := portableFixtureAssignment(request.Input)
				if assignment == nil {
					return nil, fmt.Errorf("lost assignment")
				}
				task := assignment["task"].(map[string]any)
				kind := task["kind"].(string)
				skill, _ := task["skill"].(string)
				observed = append(observed, skill)
				if scenario == "ordered-parallel-finalizers" && skill == "local:review" {
					reviewIDs[task["id"].(string)] = true
					mu.Unlock()
					if reviewArrivals.Add(1) == 2 {
						close(reviewOverlap)
					}
					timer := time.NewTimer(30 * time.Second)
					var overlapErr error
					select {
					case <-reviewOverlap:
					case <-timer.C:
						overlapErr = fmt.Errorf("authored parallel reviewers did not overlap")
					case <-t.Context().Done():
						overlapErr = t.Context().Err()
					}
					timer.Stop()
					mu.Lock()
					if overlapErr != nil {
						return nil, overlapErr
					}
				}
				checks := []any{}
				for _, raw := range task["checklist"].([]any) {
					checks = append(checks, map[string]any{"id": raw.(map[string]any)["id"], "status": "completed", "evidence": []string{"source"}})
				}
				report := map[string]any{"complete": true, "checklist": checks, "findings": []any{}, "blockers": []any{}, "evidence": []any{map[string]any{"id": "source", "path": "file.txt", "sha256": hex.EncodeToString(sourceHash[:])}}}
				if task["id"] == "ci_repair" {
					repairs++
					updateMetadata(map[string]any{"fixture_ci_conclusion": "success"})
				}
				if scenario == "ci-repair" && skill == "dk-workflows:dk-pr-description" {
					report["operations"] = []any{map[string]any{"kind": "reply", "head": head, "target": base, "payload": map[string]any{"body": "Deterministic wrapper CI publication."}}}
				}
				if kind == "publication_audience" {
					audiences++
					report["audience"] = map[string]any{"fingerprint": assignment["payload"].(map[string]any)["fingerprint"], "suitable": true, "evidence": []string{"source"}}
				}
				data, err := json.Marshal(report)
				return map[string]any{"text": string(data)}, err
			}}
			e.Agents = fake
			e.Capacity = &Capacity{Slots: e.Slots}
			manager := &WorkerManager{Store: store, Slots: e.Slots, Capacity: e.Capacity, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return fake, nil, nil, nil
			}}
			if err := manager.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.Workers = manager
			e.BeforeFinalize = manager.StopRun
			serveWorkers(t, manager)
			budget := 20 * time.Minute
			if scenario == "defaults" {
				// The complete default plan needs more time than the custom selections.
				budget = 40 * time.Minute
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			run, err := e.Run(ctx, RunRequest{Ref: entry, Inputs: inputs, Secrets: map[string]string{"github_token": "fixture-token"}})
			if err != nil || run == nil || run.Status != RunStatusSucceeded || run.Outputs["complete"] != true {
				t.Fatalf("%s failed: run=%+v err=%v", scenario, run, err)
			}
			if scenario == "ci-repair" {
				main, _, err := store.Load(ctx, run.Jobs["maintain"].ChildRunID)
				if err != nil || main.Status != RunStatusSucceeded {
					t.Fatal("wrapper lost main result", main, err)
				}
				run = main
			}
			if scenario == "empty" {
				planRef, _ := run.Jobs["prepare"].Outputs["plan"].(string)
				data, _ := os.ReadFile(filepath.Join(root, planRef))
				var plan map[string]any
				_ = json.Unmarshal(data, &plan)
				if plan["settings"].(map[string]any)["scope"] != "fixture-settings" {
					t.Fatalf("settings lost: inputs=%v plan=%s", inputs, data)
				}
				inputs["settings"] = map[string]any{"scope": "changed-settings"}
				second, err := e.Run(ctx, RunRequest{Ref: "examples/pr-fix/workflows/main.yml", Inputs: inputs, Secrets: map[string]string{"github_token": "fixture-token"}})
				if err != nil {
					t.Fatal(err)
				}
				planRef, _ = second.Jobs["prepare"].Outputs["plan"].(string)
				data, _ = os.ReadFile(filepath.Join(root, planRef))
				_ = json.Unmarshal(data, &plan)
				if plan["settings"].(map[string]any)["scope"] != "changed-settings" {
					t.Fatalf("changed settings lost: inputs=%v plan=%s", inputs, data)
				}
			}
			if scenario == "empty" && len(observed) != 0 {
				t.Fatal("empty selections dispatched", observed)
			}
			if scenario == "defaults" {
				// The first feedback fix predates independent verification. Its
				// changed governing evidence requires one refresh in the next cycle.
				expected := []string{"dk-workflows:dk-pr-feedback-read", "dk-workflows:dk-pr-decisions", "dk-workflows:dk-pr-feedback-address", "dk-workflows:dk-pr-split", "dk-workflows:dk-pr-description", "local:review", "finding_verification", "finding_fix", "dk-workflows:dk-commit-split", "dk-workflows:dk-preferences", "dk-workflows:dk-pr-feedback-address"}
				if strings.Join(observed, ",") != strings.Join(expected, ",") {
					t.Fatal("default task/finalizer order changed", observed)
				}
			}

			if scenario == "ordered-parallel-finalizers" {
				if reviewArrivals.Load() != 2 || len(reviewIDs) != 2 {
					t.Fatal("parallel group lost distinct reviewer occurrences", reviewArrivals.Load(), reviewIDs)
				}
				expected := []string{"local:review", "local:review", "finding_verification", "finding_fix", "dk-workflows:dk-pr-description", "dk-workflows:dk-pr-description", "dk-workflows:dk-pr-feedback-read"}
				if strings.Join(observed, ",") != strings.Join(expected, ",") {
					t.Fatal("task occurrence order changed", observed)
				}
			}
			if scenario == "ci-repair" {
				if repairs != 1 || audiences != 1 {
					t.Fatal("CI repair/audience counts", repairs, audiences)
				}
				calls, err := os.ReadFile(os.Getenv("AW_PORTABLE_CALLS"))
				if err != nil {
					t.Fatal(err)
				}
				writes := 0
				for _, line := range bytes.Split(bytes.TrimSpace(calls), []byte("\n")) {
					var call []string
					if err := json.Unmarshal(line, &call); err != nil {
						t.Fatal(err)
					}
					if len(call) == 2 && call[0] == "POST" && call[1] == "repos/fixture/repository/issues/1/comments" {
						writes++
					}
				}
				logical, _, _ := portablePreparedIdentity(t, root, run)
				ledger, err := sql.Open("sqlite", "file:"+filepath.Join(root, ".pr-fix-state", logical+".sqlite")+"?mode=ro")
				if err != nil {
					t.Fatal(err)
				}
				defer ledger.Close()
				var receipts int
				if err := ledger.QueryRow("SELECT count(*) FROM publication_intents WHERE state='completed' AND receipt IS NOT NULL AND json_extract(CAST(value AS TEXT),'$.kind')='reply'").Scan(&receipts); err != nil {
					t.Fatal(err)
				}
				if writes != 1 || receipts != 1 {
					t.Fatal("wrapper publication lost exact effect/receipt", writes, receipts)
				}
			}
			if scenario == "ci-polling-chunk" && pollDelays.Load() != 2 {
				t.Fatal("CI chunk did not roll over", pollDelays.Load())
			}
			if scenario == "merge-authorized" {
				if run.Jobs["merge"].Status != RunStatusSucceeded {
					t.Fatal("authorized merge not executed", run.Jobs["merge"])
				}
			} else if run.Jobs["merge"].Status != RunStatusSkipped {
				t.Fatal("unauthorized merge branch executed", run.Jobs["merge"])
			}
			if changes := portableFixtureGit(t, root, "status", "--porcelain", "--", ".", ":(exclude).pr-fix-state"); changes != "" {
				t.Fatal("execution changed definitions/configuration", changes)
			}
		})
	}
}

func TestPortablePRFixRetainedReportSchemasStayCompatible(t *testing.T) {
	var original map[string]any
	for _, name := range []string{"native.yml", "retry.yml", "continue.yml"} {
		source, err := os.ReadFile(filepath.Join("..", "..", "examples", "pr-fix", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		workflow, err := Parse(source)
		if err != nil {
			t.Fatal(name, err)
		}
		schema := workflow.Schemas["task-report"]
		if len(schema) == 0 {
			t.Fatal(name, "has no retained task-report schema")
		}
		if original == nil {
			original = schema
		}
		if actionHash(schema) != actionHash(original) {
			t.Fatal(name, "task-report differs from native.yml; retained reports must survive retry and checklist continuation")
		}
	}
	report := map[string]any{"complete": true, "checklist": []any{}, "findings": []any{}, "evidence": []any{}, "blockers": []any{}, "deliverables": map[string]any{}}
	if err := validateJSONSchemaValue(report, original, "$"); err != nil {
		t.Fatal("original report contract rejected its allowed field", err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var narrowed map[string]any
	if err := json.Unmarshal(data, &narrowed); err != nil {
		t.Fatal(err)
	}
	delete(narrowed["properties"].(map[string]any), "deliverables")
	if err := validateJSONSchemaValue(report, narrowed, "$"); err == nil {
		t.Fatal("one-phase allowed-field drift did not reject the retained report")
	}
}

func TestPortablePRFixRetainedPublicationHasOneReceiptOwner(t *testing.T) {
	for _, scenario := range []string{"completed", "failed-prepare", "unresolved", "lost-response"} {
		t.Run(scenario, func(t *testing.T) {
			e, store, docker := containerExecutor(t)
			if err := os.CopyFS(filepath.Join(e.WorkspaceDir, "examples", "pr-fix"), os.DirFS(filepath.Join("..", "..", "examples", "pr-fix"))); err != nil {
				t.Fatal(err)
			}
			e.CommandExecution = runtimeexec.Policy{Mode: "container", Image: "fixture/python-tools@sha256:" + strings.Repeat("a", 64), Runtime: "docker", NetworkMode: "bridge", Writable: true}
			fake := &nativeTaskFixture{respond: func(SessionRequest) (map[string]any, error) {
				return nil, fmt.Errorf("retained publication must not dispatch an audience")
			}}
			e.Agents = fake
			e.Capacity = &Capacity{Slots: e.Slots}
			manager := &WorkerManager{Store: store, Slots: e.Slots, Capacity: e.Capacity, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return fake, nil, nil, nil
			}}
			if err := manager.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			e.Workers, e.BeforeFinalize = manager, manager.StopRun
			serveWorkers(t, manager)
			var mu sync.Mutex
			calls := map[string]int{}
			completed := map[string]map[string]any{}
			docker.helper = func(_ context.Context, argv []string, input []byte, _ []string) ([]byte, []byte, int, bool) {
				if len(argv) != 4 || argv[2] != "/actions/helpers/action.py" {
					return nil, nil, 0, false
				}
				mu.Lock()
				defer mu.Unlock()
				operation := argv[3]
				var request map[string]any
				if err := json.Unmarshal(input, &request); err != nil {
					return nil, []byte(err.Error()), 1, true
				}
				phase := request["phase"].(string)
				calls[operation+"/"+phase]++
				if scenario == "failed-prepare" && operation == "publication_prepare" && phase == "bindings" {
					return nil, []byte("fixture preparation rejected"), 1, true
				}
				if operation != "publication_prepare" && operation != "publication_recover" {
					return nil, []byte("unexpected audience or publication dispatch"), 1, true
				}
				key := request["context"].(map[string]any)["operation_key"].(string)
				output := map[string]any{"id": "retained", "invoke": false, "intent": map[string]any{}, "fingerprint": "fixture"}
				if operation == "publication_recover" {
					output = map[string]any{"receipt": map[string]any{"id": "original-receipt"}}
				}
				result := map[string]any{"output": output, "evidence": map[string]any{"original": "retained"}}
				response := map[string]any{"protocol": "action-v1", "phase": phase}
				switch phase {
				case "bindings":
					response["bindings"] = map[string]any{"fixture": "retained-publication"}
					response["capabilities"] = []string{"bindings", "execute", "validate_checkpoint", "reconcile"}
					response["read_only_phases"] = []string{"bindings", "validate_checkpoint", "reconcile"}
				case "execute":
					if operation == "publication_recover" && scenario == "unresolved" {
						return nil, []byte("fixture original publication unresolved"), 1, true
					}
					completed[key] = result
					if operation == "publication_recover" && scenario == "lost-response" {
						return nil, []byte("fixture response lost after recording"), 1, true
					}
					response["result"] = result
				case "validate_checkpoint":
					response["valid"] = true
				case "reconcile":
					response["outcome"] = "unknown"
					if original, ok := completed[key]; ok {
						response["outcome"], response["result"] = "completed", original
						response["proof"] = map[string]any{"operation_key": key}
					}
				}
				data, err := json.Marshal(response)
				if err != nil {
					return nil, []byte(err.Error()), 1, true
				}
				return data, nil, 0, true
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			run, err := e.Run(ctx, RunRequest{Ref: "examples/pr-fix/workflows/publication.yml", Inputs: map[string]any{"plan": "fixture-plan", "proposal": map[string]any{}}, Secrets: map[string]string{"github_token": "fixture-token"}})
			if scenario == "lost-response" {
				if run == nil || err == nil {
					t.Fatalf("lost recovery response was not retained: %+v %v", run, err)
				}
				run, err = e.Resume(ctx, run.ID, map[string]string{"github_token": "fixture-token"}, false)
			}
			if run == nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if scenario == "failed-prepare" {
				if err == nil || run.Jobs["receipt"].Status != RunStatusSkipped || calls["publication_recover/bindings"] != 0 {
					t.Fatalf("failed prepare admitted recovery: %+v %v calls=%v", run, err, calls)
				}
				return
			}
			if scenario == "unresolved" {
				if err == nil || run.Jobs["receipt"].Status != RunStatusNeedsAttention {
					t.Fatalf("unresolved original accepted: %+v %v", run, err)
				}
			} else if err != nil || run.Status != RunStatusSucceeded || run.Outputs["receipt"].(map[string]any)["id"] != "original-receipt" {
				t.Fatalf("retained receipt lost: %+v %v", run, err)
			}
			if calls["publication_recover/execute"] != 1 || run.Jobs["audience"].Status != RunStatusSkipped || run.Jobs["publish"].Status != RunStatusSkipped {
				t.Fatalf("retained publication repeated recovery or dispatch: calls=%v jobs=%+v", calls, run.Jobs)
			}
		})
	}
}

func TestPortablePRFixWrappersRetainEngineDepthFence(t *testing.T) {
	e, _, fake := agentTaskFixture(t, `jobs: {leaf: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work}}]}}`, nil)
	all := map[string]*Workflow{}
	for level := 0; level <= 9; level++ {
		name := fmt.Sprintf("depth-%d.yml", level)
		source := `on: {workflow_call: {}}
jobs: {leaf: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work}}]}}
`
		if level < 9 {
			source = fmt.Sprintf("on: {workflow_call: {}}\njobs: {child: {uses: workflows/depth-%d.yml}}", level+1)
		}
		writeWorkflow(t, e.WorkspaceDir, name, source)
		definition, err := Parse([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		all["workflows/"+name] = definition
	}
	if _, err := e.Load("workflows/depth-0.yml"); err == nil || !strings.Contains(err.Error(), "depth exceeds 8") {
		t.Fatal("closure depth fence removed", err)
	}
	// Already available definitions must still meet the execution-time fence.
	if _, err := e.execute(t.Context(), "workflows/depth-9.yml", RunRequest{}, all, e.Slots, 9, nil); err == nil || !strings.Contains(err.Error(), "depth exceeds 8") {
		t.Fatal("execution depth fence removed", err)
	}
	if len(fake.requests) != 0 {
		t.Fatal("above-limit wrapper dispatched", fake.requests)
	}
}
