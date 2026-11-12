package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeAgentTaskRejectsSecretsBeforeRetainingDispatch(t *testing.T) {
	const secret = "private-native-input-token"
	e, s, f := agentTaskFixture(t, `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: Review the task.
          input: {credential: '${{ secrets.token }}'}
`, func(SessionRequest) (map[string]any, error) {
		t.Fatal("secret-bearing task reached the provider")
		return nil, nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": secret}})
	if err == nil || !strings.Contains(err.Error(), "credential references") {
		t.Fatalf("expected input rejection, got %v", err)
	}
	attempts, err := s.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(attempts)
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatalf("secret retained in attempt journal: %s (%v)", data, err)
	}
	for _, attempt := range attempts {
		if attempt.AgentTask != nil {
			t.Fatal("rejected input retained a dispatch intent")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatal("rejected input dispatched work")
	}
}

type nativeTaskFixture struct {
	mu       sync.Mutex
	requests []SessionRequest
	respond  func(SessionRequest) (map[string]any, error)
}

func (f *nativeTaskFixture) AdmitNativeTask(d *AgentDefinition) (*AgentDefinition, error) {
	copy := *d
	return &copy, nil
}
func (f *nativeTaskFixture) Capabilities(string) (ProviderCapabilities, error) {
	return ProviderCapabilities{Sessions: true, Resume: true}, nil
}
func (f *nativeTaskFixture) RunAgent(context.Context, AgentRequest) (map[string]any, error) {
	return nil, fmt.Errorf("must use durable session")
}
func (f *nativeTaskFixture) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	session := req.SessionID
	if session == "" {
		session = "session-" + req.ParentWorkerID
	}
	emit(ProviderEvent{Type: "session", SessionID: session})
	out, err := f.respond(req)
	return SessionResult{SessionID: session, Status: RunStatusSucceeded, Outputs: out}, err
}
func agentTaskFixture(t *testing.T, source string, respond func(SessionRequest) (map[string]any, error)) (*Executor, *SQLiteStore, *nativeTaskFixture) {
	t.Helper()
	f := &nativeTaskFixture{respond: respond}
	e, s := durableExecutor(t, source, f)
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", "reviewer.toml"), []byte("name = 'reviewer'\ndescription = 'Review changes'\ndeveloper_instructions = 'Review admitted changes.'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.Slots = make(chan struct{}, 1)
	e.Capacity = &Capacity{Slots: e.Slots}
	m := &WorkerManager{Store: s, Slots: e.Slots, Capacity: e.Capacity, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return f, nil, nil, nil
	}}
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Workers = m
	e.BeforeFinalize = m.StopRun
	serveWorkers(t, m)
	return e, s, f
}
func TestNativeAgentTaskIdentityContinuationAndIndependentAgent(t *testing.T) {
	e, _, f := agentTaskFixture(t, `jobs:
  review:
    steps:
      - id: first
        uses: agent/run
        with: {agent: reviewer.toml, prompt: first, input: {number: 9007199254740993, flag: true}}
      - id: followup
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: next}
      - id: independent
        uses: agent/run
        with: {agent: reviewer.toml, prompt: independent}
`, func(req SessionRequest) (map[string]any, error) {
		return map[string]any{"text": req.Prompt, "session_id": "private-provider-session"}, nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	steps := run.Jobs["review"].Steps
	for _, step := range steps {
		if _, exposed := step.Outputs["session_id"]; exposed {
			t.Fatal("task exposed its provider session identity")
		}
	}
	if steps["first"].Outputs["agent"] != steps["followup"].Outputs["agent"] || steps["first"].Outputs["agent"] == steps["independent"].Outputs["agent"] {
		t.Fatal(steps)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 3 || f.requests[0].SessionID != "" || f.requests[1].SessionID == "" || f.requests[0].ParentWorkerID != f.requests[1].ParentWorkerID || f.requests[2].SessionID != "" {
		t.Fatal(f.requests)
	}
	if f.requests[0].Definition.DeveloperInstructions != "Review admitted changes." {
		t.Fatal(f.requests[0])
	}
}
func TestNativeAgentTaskDefinitionSnapshotAndHooks(t *testing.T) {
	e, s, f := agentTaskFixture(t, `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.toml
          prompt: first
          output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete, count], properties: {complete: {type: boolean}, count: {type: integer}}}}
        onResult:
          - id: verify
            argv: [python3, -c, 'import json,sys; e=json.load(sys.stdin); assert e["type"]=="onResult"; assert e["result"]["count"]==9007199254740993; print(json.dumps({"complete": True, "feedback": {"number": e["result"]["count"]}}))']
        onComplete:
          - {id: accept, argv: [python3, -c, 'import json,sys; e=json.load(sys.stdin); assert e["source"]["trigger"]=="original"; print("accepted")']}
        finally:
          - {id: finalize, argv: [true]}
`, func(req SessionRequest) (map[string]any, error) {
		return map[string]any{"text": `{"complete":true,"count":9007199254740993}`}, nil
	})
	prepared, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml", Event: map[string]any{"trigger": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", "reviewer.toml"), []byte("invalid = true"), 0600); err != nil {
		t.Fatal(err)
	}
	// Run admitted snapshot through the same queued owner entry used by controllers.
	run, err := e.Resume(context.Background(), prepared.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	result := run.Jobs["review"].Steps["review"]
	if result.Outputs["complete"] != true || len(result.Lifecycle.Handlers["onComplete"]) != 1 {
		t.Fatalf("%+v", result)
	}
	verdict := result.Lifecycle.Verification.(map[string]any)
	number := verdict["feedback"].(map[string]any)["number"]
	if fmt.Sprint(number) != "9007199254740993" {
		t.Fatal(number)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 || f.requests[0].Definition.DeveloperInstructions != "Review admitted changes." {
		t.Fatal(f.requests)
	}
	attempts, err := s.Attempts(context.Background(), run.ID)
	if err != nil || len(attempts) != 4 {
		t.Fatal(attempts, err)
	}
}
func TestNativeAgentTaskIncompleteAndBoundedFollowup(t *testing.T) {
	for _, followup := range []bool{false, true} {
		t.Run(fmt.Sprint(followup), func(t *testing.T) {
			extra := "          - {argv: [true]}\n"
			if followup {
				extra = "          - uses: agent/run\n            with: {agent: '${{ event.agent }}', prompt: finish}\n"
			}
			e, _, f := agentTaskFixture(t, `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with: {agent: reviewer.toml, prompt: first, output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}}
        onResult:
          - argv: [python3, -c, 'import json,sys; json.load(sys.stdin); print("{\"complete\":true}")']
        onIncomplete:
`+extra, func(req SessionRequest) (map[string]any, error) {
				return map[string]any{"text": fmt.Sprintf(`{"complete":%t}`, strings.HasPrefix(req.Prompt, "finish"))}, nil
			})
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			result := run.Jobs["review"].Steps["review"]
			if result.Outputs["complete"] != followup {
				t.Fatal(result)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			want := 1
			if followup {
				want = 2
			}
			if len(f.requests) != want {
				t.Fatal(f.requests)
			}
		})
	}
}
func TestNativeAgentDefinitionLoading(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadAgentDefinition(dir); err == nil {
		t.Fatal("empty directory accepted")
	}
	valid := "name = 'review'\ndescription = 'Review'\ndeveloper_instructions = 'Review changes.'\nmodel = 'configured-model'\nmodel_reasoning_effort = 'high'\n"
	path := filepath.Join(dir, "review.toml")
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := LoadAgentDefinition(dir)
	if err != nil || d.Bytes != valid || d.Model != "configured-model" {
		t.Fatal(d, err)
	}
	if err = os.WriteFile(filepath.Join(dir, "second.toml"), []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadAgentDefinition(dir); err == nil {
		t.Fatal("ambiguous directory accepted")
	}
	for _, field := range []string{"sandbox_mode = 'danger-full-access'", "approval_policy = 'never'", "[mcp_servers]", "tools = ['shell']"} {
		if err = os.WriteFile(path, []byte(valid+field+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadAgentDefinition(path); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatal(field, err)
		}
	}
}

func TestNativeAgentTaskRecoveryAndReportingFailures(t *testing.T) {
	for _, scenario := range []string{"invalid", "verifier", "finalizer"} {
		t.Run(scenario, func(t *testing.T) {
			verifier := "          - argv: [python3, -c, 'print(\"{\\\"complete\\\":true}\")']\n"
			finalizer := "          - {argv: [true]}\n"
			if scenario == "verifier" {
				verifier = "          - argv: [python3, -c, 'import json,sys; e=json.load(sys.stdin); print(\"invalid\" if e[\"result\"].get(\"original\") else \"{\\\"complete\\\":true}\")']\n"
			}
			if scenario == "finalizer" {
				finalizer = "          - {argv: [false]}\n"
			}
			source := `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with: {agent: reviewer.toml, prompt: first, output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}}
        onResult:
` + verifier + `        onError:
          - uses: agent/run
            with: {agent: reviewer.toml, prompt: recover, output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}}
        onFailure:
          - {argv: [true]}
        finally:
` + finalizer
			e, _, f := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
				if strings.HasPrefix(req.Prompt, "recover") {
					return map[string]any{"text": `{"complete":true}`}, nil
				}
				if scenario == "invalid" {
					return map[string]any{"text": "not json"}, nil
				}
				return map[string]any{"text": `{"complete":true,"original":true}`}, nil
			})
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			result := run.Jobs["review"].Steps["review"]
			if scenario == "finalizer" {
				if err == nil || result.Status == RunStatusSucceeded {
					t.Fatal("finalizer failure was swallowed", result)
				}
			} else {
				if err != nil || result.Outputs["complete"] != true || result.Lifecycle.OriginalError == "" {
					t.Fatal(result, err)
				}
				if result.Lifecycle.Agent != "" && result.Outputs["agent"] != result.Lifecycle.Agent {
					t.Fatal("fallback replaced the owner's original agent reference", result)
				}
				if len(result.Lifecycle.Handlers["onFailure"]) != 0 || len(result.Lifecycle.Handlers["onError"]) != 1 {
					t.Fatal(result.Lifecycle)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			want := 2
			if scenario == "finalizer" {
				want = 1
			}
			if len(f.requests) != want {
				t.Fatal(f.requests)
			}
		})
	}
}

func TestNativeAgentTaskProviderFailureRecoveryAcknowledgesOriginal(t *testing.T) {
	e, _, _ := agentTaskFixture(t, `jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with: {agent: reviewer.toml, prompt: first}
        onError:
          - uses: agent/run
            with: {agent: reviewer.toml, prompt: recover}
`, func(req SessionRequest) (map[string]any, error) {
		if req.Prompt == "first" {
			return map[string]any{"text": "original failure"}, ErrProviderFailure
		}
		return map[string]any{"text": "recovered"}, nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	result := run.Jobs["review"].Steps["review"]
	if result.Outputs["text"] != "recovered" || result.Lifecycle.OriginalClass != "provider_failure" {
		t.Fatal(result)
	}
	workers, err := e.Workers.List(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := false
	for _, worker := range workers {
		messages, err := e.Workers.Messages(context.Background(), run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			if message.Status == RunStatusFailed {
				acknowledged = message.FailureAcknowledgement != nil
			}
		}
	}
	if !acknowledged {
		t.Fatal("original native failure was not dispositioned")
	}
}

func TestNativeAgentStaleProducerCannotCommitCompletion(t *testing.T) {
	ctx := context.Background()
	manager, runID := workerFixture(t, &fakeSession{})
	entered, release := make(chan struct{}), make(chan struct{})
	runner := &nativeTaskFixture{respond: func(SessionRequest) (map[string]any, error) {
		close(entered)
		<-release
		return map[string]any{"text": "stale result"}, nil
	}}
	manager.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return runner, nil, nil, nil
	}
	definition := &AgentDefinition{Name: "fixture", Description: "Fixture", DeveloperInstructions: "Run fixture."}
	worker, err := manager.Spawn(ctx, WorkerSpec{RunID: runID, Role: "codex", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.sendTask(ctx, runID, worker.ID, "stale-producer", AgentRequest{Prompt: "test", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	stop := serveWorkers(t, manager)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not start")
	}
	if _, err = manager.Store.db.Exec("UPDATE runs SET owner='successor',generation=generation+1 WHERE id=?", runID); err != nil {
		t.Fatal(err)
	}
	close(release)
	// Stop joins the provider; the old owner must leave its unresolved evidence.
	stop()
	manager.wg.Wait()
	retained, err := manager.taskMessage(ctx, worker.ID, message.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Status != RunStatusRunning || retained.Result.Outputs != nil || !retained.FinishedAt.IsZero() {
		t.Fatalf("stale completion crossed owner fence: %+v", retained)
	}
}

func TestNativeTaskLostCompletionReusesAuthoritativeTurn(t *testing.T) {
	e, store, fixture := agentTaskFixture(t, `jobs:
  task:
    steps:
      - id: task
        uses: agent/run
        with: {agent: reviewer.toml, prompt: work}
`, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "original"}, nil })
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "task"}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil {
		t.Fatal("completion fault not exercised", run, err)
	}
	e.Store = store
	resumed, err := e.Resume(context.Background(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded {
		t.Fatal(resumed, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 1 || resumed.Jobs["task"].Steps["task"].Outputs["text"] != "original" {
		t.Fatal("durable receipt duplicated provider work", fixture.requests, resumed)
	}
}

func TestNativeAgentAIVerificationRejectsDeclaredCompletion(t *testing.T) {
	e, _, fixture := agentTaskFixture(t, `jobs:
  task:
    steps:
      - id: task
        uses: agent/run
        with: {agent: reviewer.toml, prompt: work, output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}}
        onResult:
          - id: verifier
            uses: agent/run
            with: {agent: reviewer.toml, prompt: verify}
        onComplete:
          - argv: [false]
        onIncomplete:
          - argv: [python3, -c, 'import json,sys; e=json.load(sys.stdin); assert e["result"]["complete"] is True; assert e["verification"]["complete"] is False; assert e["source"]["flag"] is False']
`, func(req SessionRequest) (map[string]any, error) {
		if strings.HasPrefix(req.Prompt, "verify") {
			event, ok := req.Input.(map[string]any)
			if !ok || event["type"] != "onResult" {
				return nil, fmt.Errorf("AI verifier lost typed event: %v", req.Input)
			}
			return map[string]any{"text": `{"complete":false,"feedback":{"number":9007199254740993}}`}, nil
		}
		return map[string]any{"text": `{"complete":true}`}, nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Event: map[string]any{"flag": false}})
	if err != nil {
		t.Fatal(err)
	}
	result := run.Jobs["task"].Steps["task"]
	if result.Outputs["complete"] != false || len(result.Lifecycle.Handlers["onComplete"]) != 0 || len(result.Lifecycle.Verifications) != 1 {
		t.Fatal(result)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 2 || fixture.requests[0].ParentWorkerID == fixture.requests[1].ParentWorkerID {
		t.Fatal("verifier did not own an independent agent", fixture.requests)
	}
}
