package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestLegacyNativeCompletedWorkSurvivesEngineRestart(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed-run", true: "lost-step-ack"}[lostAck], func(t *testing.T) { testLegacyNativeRestart(t, lostAck) })
	}
}

func testLegacyNativeRestart(t *testing.T, lostAck bool) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("local Python fixture unavailable")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	calls := filepath.Join(root, "turns")
	command := filepath.Join(root, "fake-codex")
	fixture := `import sys,json
for line in sys.stdin:
 f=json.loads(line); m=f.get('method'); i=f.get('id')
 def out(v): print(json.dumps(v),flush=True)
 if m=='initialize': out({'id':i,'result':{'userAgent':'codex_cli_rs/` + clirunner.CodexProtocolVersion + ` (fixture)'}})
 elif m=='account/login/start': out({'id':i,'result':{}})
 elif m in ('thread/start','thread/resume'): out({'id':i,'result':{'thread':{'id':'legacy-thread'}}})
 elif m=='turn/start':
  with open(` + strconv.Quote(calls) + `,'a') as count: count.write('turn\n')
  out({'id':i,'result':{'turn':{'id':'legacy-turn'}}})
  out({'method':'item/completed','params':{'threadId':'legacy-thread','turnId':'legacy-turn','item':{'type':'agentMessage','text':'{"approved":true}'}}})
  out({'method':'turn/completed','params':{'threadId':'legacy-thread','turn':{'id':'legacy-turn','status':'completed'}}})
`
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli " + clirunner.CodexProtocolVersion + "'; exit 0; fi\nexec '" + python + "' -c '" + strings.ReplaceAll(fixture, "'", "'\\''") + "'\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGACY_NATIVE_TEST_KEY", "local-fixture-key")
	cfg := clirunner.Config{Agents: map[string]clirunner.Agent{"codex": {Provider: "codex-app-server", Command: command, Limits: clirunner.Limits{MaxTurns: 3}, Authentication: clirunner.Authentication{Mode: "api-key", CredentialEnv: "LEGACY_NATIVE_TEST_KEY"}}}}
	roles, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	source := `jobs: {review: {steps: [{id: work, uses: agent/run, with: {agent: removed.toml, prompt: Review, turns: 1, output: {format: json, repair-attempts: 1}}}]}}`
	w, err := workflows.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	raw := `name = 'reviewer'
developer_instructions = 'Original instructions.'
`
	definition := &workflows.AgentDefinition{Name: "reviewer", Description: "Legacy fixture", DeveloperInstructions: "Original instructions.", Source: "/removed/reviewer.toml", Bytes: raw}
	job := w.Jobs["review"]
	job.Steps[0].Definition = definition
	w.Jobs["review"] = job
	encoded, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(encoded)
	snapshot := workflows.Snapshot{Version: 1, Definitions: map[string]*workflows.Workflow{"workflows/main.yml": w}, Hashes: map[string]string{"workflows/main.yml": hex.EncodeToString(hash[:])}, Roles: roles, Parallel: 1}
	run := &workflows.Run{ID: "legacy-native-runtime", Version: 1, WorkflowRef: "workflows/main.yml", Status: workflows.RunStatusQueued, StartedAt: time.Now().UTC(), Jobs: map[string]workflows.JobExecution{}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := workflows.OpenSQLite(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Admit(ctx, run, snapshot); err != nil {
		store.Close()
		t.Fatal(err)
	}
	executor, workers := Executor(clirunner.Config{AgentRuntime: "changed"}, store, root, 1)
	workers.Limits = workflows.WorkerLimits{MaxWorkers: 4, MaxDepth: 2, MaxQueued: 4, MaxTurns: 3, TurnDuration: time.Second * 5, MaxOutput: 65536}
	if err := workers.Init(ctx); err != nil {
		t.Fatal(err)
	}
	serviceCtx, stop := context.WithCancel(ctx)
	serviceDone := make(chan struct{})
	go func() { workers.Serve(serviceCtx); close(serviceDone) }()
	defer stop()
	fault := &nativeStepAckFault{SQLiteStore: store}
	if lostAck {
		executor.Store = fault
	}
	result, err := executor.Resume(ctx, run.ID, nil, false)
	if !lostAck && (err != nil || result.Status != workflows.RunStatusSucceeded) {
		store.Close()
		t.Fatalf("legacy resume: %+v %v", result, err)
	}
	if lostAck {
		if err == nil || !fault.failed {
			t.Fatal("step acknowledgement fault not exercised")
		}
		attempts, loadErr := store.Attempts(ctx, run.ID)
		if loadErr != nil || len(attempts) != 1 || attempts[0].AgentTask == nil || attempts[0].Phase != "" || attempts[0].Result.Status != "" {
			t.Fatalf("lost acknowledgement retained step result instead of only dispatch: %+v %v", attempts, loadErr)
		}
	}
	before, err := workers.List(ctx, run.ID)
	if err != nil || len(before) != 1 {
		store.Close()
		t.Fatalf("legacy workers: %+v %v", before, err)
	}
	if before[0].Role != "codex" || before[0].SessionID != "legacy-thread" || before[0].Remaining != 0 || before[0].Definition.Bytes != raw {
		store.Close()
		t.Fatalf("legacy bindings: %+v", before[0])
	}
	originalMessages, err := workers.Messages(ctx, run.ID, before[0].ID)
	if err != nil || len(originalMessages) != 1 || originalMessages[0].Status != workflows.RunStatusSucceeded {
		t.Fatal(originalMessages, err)
	}
	stop()
	<-serviceDone
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = workflows.OpenSQLite(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, workers = Executor(clirunner.Config{AgentRuntime: "changed-again"}, store, root, 1)
	var outputChecks atomic.Int32
	if lostAck {
		executor.RoleRunner = func(raw json.RawMessage) (workflows.AgentRunner, error) {
			var saved clirunner.Config
			if err := json.Unmarshal(raw, &saved); err != nil {
				return nil, err
			}
			return &receiptOutputObserver{Runner: &clirunner.Runner{Config: saved, Workspace: root}, checks: &outputChecks}, nil
		}
	}
	if err := workers.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if resumed, err := executor.Resume(ctx, run.ID, nil, lostAck); err != nil || resumed.Status != workflows.RunStatusSucceeded {
		t.Fatal(err)
	}
	after, err := workers.List(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("completed worker budget/bindings changed: %+v %v", after, err)
	}
	if outputChecks.Load() != 0 {
		t.Fatal("completed dispatch was revalidated after restart")
	}
	retainedMessages, err := workers.Messages(ctx, run.ID, before[0].ID)
	if err != nil || !reflect.DeepEqual(originalMessages, retainedMessages) {
		t.Fatalf("authoritative message changed: %+v %v", retainedMessages, err)
	}
	turns, err := os.ReadFile(calls)
	if err != nil || string(turns) != "turn\n" {
		t.Fatalf("completed work replayed: %q %v", turns, err)
	}
	_, retained, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(retained.Roles) != string(roles) {
		t.Fatal("legacy Roles rewritten")
	}
	if retained.Definitions["workflows/main.yml"].Jobs["review"].Steps[0].Definition.Bytes != raw || retained.Hashes["workflows/main.yml"] != snapshot.Hashes["workflows/main.yml"] {
		t.Fatal("legacy definition identity changed")
	}
}

// Lose only the outer step acknowledgement, after the production worker has
// committed its authoritative message. The embedded store retains all journal
// extensions and production recovery/fencing behavior.
type nativeStepAckFault struct {
	*workflows.SQLiteStore
	failed bool
}

func (s *nativeStepAckFault) Complete(ctx context.Context, attempt workflows.Attempt, result workflows.StepExecution) error {
	if attempt.StepID == "work" && !s.failed {
		s.failed = true
		return errors.New("lost native step acknowledgement")
	}
	return s.SQLiteStore.Complete(ctx, attempt, result)
}

func (s *nativeStepAckFault) RecordAttempt(ctx context.Context, attempt workflows.Attempt) error {
	if attempt.StepID == "work" && attempt.Phase == "task_completed" {
		return errors.New("lost native task result acknowledgement")
	}
	return s.SQLiteStore.RecordAttempt(ctx, attempt)
}

// Observe the production validation callback without replacing its policy or
// provider. An existing authoritative dispatch must not invoke it again.
type receiptOutputObserver struct {
	*clirunner.Runner
	checks *atomic.Int32
}

func (r *receiptOutputObserver) ValidateNativeOutput(contract *workflows.AgentOutputContract) error {
	r.checks.Add(1)
	return r.Runner.ValidateNativeOutput(contract)
}
