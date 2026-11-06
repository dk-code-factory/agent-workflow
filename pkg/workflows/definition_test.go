package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func jsonExecutor(t *testing.T, source string, runner AgentRunner) (*Executor, *SQLiteStore) {
	t.Helper()
	e, s := durableExecutor(t, source, runner)
	writeWorkflow(t, e.WorkspaceDir, "simple.json", source)
	return e, s
}

func TestJSONWorkflowExecutionAndPinnedResume(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	e, store := jsonExecutor(t, `{"version":"0.2","sources":{"config":{"file":"config.json"}},"steps":[{"id":"one","agent":"writer","prompt":"{{ $.config.message }}"},{"agent":"writer","prompt":"second"}]}`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		n := calls.Add(1)
		if n == 2 {
			return nil, errors.New("definitive task failure")
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	config := filepath.Join(e.WorkspaceDir, "workflows", "config.json")
	if err := os.WriteFile(config, []byte(`{"message":"first"}`), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/simple.json"})
	if err == nil || run.Status != RunStatusFailed || calls.Load() != 2 {
		t.Fatalf("run=%+v err=%v calls=%d", run, err, calls.Load())
	}
	retained, snapshot, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.DefinitionState == nil || snapshot.Definitions[run.WorkflowRef].Definition == nil {
		t.Fatal("definition state not persisted")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	writeWorkflow(t, e.WorkspaceDir, "simple.json", `{"version":"0.2","steps":[{"fail":"changed"}]}`)
	if _, err = e.Resume(ctx, run.ID, nil, false); err == nil || calls.Load() != 2 {
		t.Fatalf("unguarded resume: %v calls=%d", err, calls.Load())
	}
	run, err = e.Resume(ctx, run.ID, nil, true)
	if err != nil || run.Status != RunStatusSucceeded || calls.Load() != 3 || run.Outputs["text"] != "second" {
		t.Fatalf("run=%+v error=%v calls=%d", run, err, calls.Load())
	}
	attempts, _ := store.Attempts(ctx, run.ID)
	if len(attempts) != 3 {
		t.Fatalf("attempts=%+v", attempts)
	}
}

func TestJSONWorkflowPreparePinsSourceBeforeAcknowledgement(t *testing.T) {
	ctx := context.Background()
	e, _ := jsonExecutor(t, `{"version":"0.2","sources":{"config":{"file":"config.json"}},"steps":[{"value":"$.config"}]}`, nil)
	config := filepath.Join(e.WorkspaceDir, "workflows", "config.json")
	_ = os.WriteFile(config, []byte(`{"pinned":true}`), 0600)
	run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/simple.json"})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(config, []byte(`{"pinned":false}`), 0600)
	run, err = e.Resume(ctx, run.ID, nil, false)
	if err != nil || run.Outputs["pinned"] != true {
		t.Fatalf("%+v %v", run, err)
	}
}

func TestJSONWorkflowScriptBreakerAndRecovery(t *testing.T) {
	var calls atomic.Int32
	e, _ := jsonExecutor(t, `{"version":"0.2","steps":[{"agent":"writer","prompt":"task","breaker":{"run":["sh","-c","cat >/dev/null; printf '{\"allow\":false,\"reason\":\"hold\"}'"]},"onError":[{"run":["sh","-c","cat"],"with":{"handled":"$.error.code"},"output":{"type":"object","required":["handled"]}}]}]}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) { calls.Add(1); return nil, nil }))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json"})
	if err != nil || run.Status != RunStatusSucceeded || run.Outputs["handled"] != "circuit.denied" || calls.Load() != 0 {
		t.Fatalf("%+v %v calls=%d", run, err, calls.Load())
	}
}

func TestJSONWorkflowNeverReevaluatesInsertedData(t *testing.T) {
	e, _ := jsonExecutor(t, `{"version":"0.2","steps":[{"agent":"a","prompt":"Task {{ $.input.text }}"}]}`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		if req.Prompt != "Task ${{ secrets.token }}" {
			t.Fatalf("prompt changed: %q", req.Prompt)
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json", Inputs: map[string]any{"text": "${{ secrets.token }}"}, Secrets: map[string]string{"token": "hidden-secret-value"}})
	if err != nil || run.Status != RunStatusSucceeded {
		t.Fatalf("%+v %v", run, err)
	}
}

func TestJSONWorkflowFullOutputContract(t *testing.T) {
	e, _ := jsonExecutor(t, `{"version":"0.2","schemas":{"Result":{"type":"object","$defs":{"Count":{"type":"integer","minimum":1}},"properties":{"count":{"$ref":"#/$defs/Count"}},"required":["count"]}},"steps":[{"agent":"a","prompt":"count","output":"Result"}]}`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		if req.Output == nil || req.Output.Schema["$defs"] == nil || req.Output.RepairAttempts != 0 {
			t.Fatal("provider output request lost its full schema")
		}
		return map[string]any{"text": `{"count":2}`}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json"})
	if err != nil || run.Outputs["count"] != float64(2) {
		t.Fatalf("%+v %v", run, err)
	}
}

type definitionSession struct {
	t            *testing.T
	turns        int
	failOriginal bool
}

func (s *definitionSession) Capabilities(string) (ProviderCapabilities, error) {
	return ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (s *definitionSession) RunAgent(context.Context, AgentRequest) (map[string]any, error) {
	s.t.Fatal("conversation used stateless adapter")
	return nil, nil
}
func (s *definitionSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	s.turns++
	session := req.SessionID
	if session == "" {
		session = "session-" + req.AgentID
	}
	emit(ProviderEvent{SessionID: session})
	if s.failOriginal && s.turns == 1 {
		return SessionResult{SessionID: session, Status: RunStatusFailed}, errors.New("task failed")
	}
	var tool string
	var args json.RawMessage
	if req.AgentID == "helper" || req.AgentID == "writer" && !(s.failOriginal && s.turns > 1) {
		tool = "send"
		to := "reviewer"
		if s.failOriginal {
			to = "writer"
		}
		args, _ = json.Marshal(map[string]any{"to": to, "message": "please verify"})
	} else {
		tool = "finish"
		args = json.RawMessage(`{"text":"approved"}`)
		if s.failOriginal && req.SessionID != "session-writer" {
			s.t.Fatal("recovery replaced original session")
		}
	}
	found := false
	for _, entry := range req.Tools.Binding.Catalog() {
		if entry.Server == definitionToolServer && entry.Name == tool {
			found = true
			if _, err := req.Tools.Binding.Call(ctx, ToolRequest{CallID: "tool-1", Server: entry.Server, Tool: entry.Name, Arguments: args}); err != nil {
				return SessionResult{SessionID: session, Status: RunStatusFailed}, err
			}
		}
	}
	if !found {
		s.t.Fatalf("missing %s tool", tool)
	}
	return SessionResult{SessionID: session, Status: RunStatusSucceeded, Outputs: map[string]any{"text": "turn ended"}}, nil
}

func TestJSONWorkflowConversationAndOriginalAgentRecovery(t *testing.T) {
	for _, recoverOriginal := range []bool{false, true} {
		t.Run(map[bool]string{false: "conversation", true: "recovery"}[recoverOriginal], func(t *testing.T) {
			source := `{"version":"0.2","steps":[{"conversation":["writer","reviewer"],"finishBy":"reviewer","prompt":"task"}]}`
			if recoverOriginal {
				source = `{"version":"0.2","steps":[{"agent":"writer","prompt":"task","onError":[{"recover":"helper"}]}]}`
			}
			runner := &definitionSession{t: t, failOriginal: recoverOriginal}
			e, s := jsonExecutor(t, source, runner)
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json"})
			if err != nil || run.Status != RunStatusSucceeded || run.Outputs["text"] != "approved" {
				t.Fatalf("%+v %v", run, err)
			}
			attempts, _ := s.Attempts(context.Background(), run.ID)
			want := 2
			if recoverOriginal {
				want = 3
			}
			if len(attempts) != want {
				t.Fatalf("attempts=%d", len(attempts))
			}
		})
	}
}

func TestJSONWorkflowAdmissionFailuresStartNoWork(t *testing.T) {
	for _, source := range []string{
		`{"version":"0.2","sources":{"config":{"file":"missing.json"}},"steps":[{"agent":"a","prompt":"task"}]}`,
		`{"version":"0.2","input":{"type":"object","required":["task"]},"steps":[{"agent":"a","prompt":"task"}]}`,
		`{"version":"0.2","steps":[{"conversation":["a","b"],"finishBy":"b","prompt":"task"}]}`,
	} {
		e, _ := jsonExecutor(t, source, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
			t.Fatal("effect started before admission")
			return nil, nil
		}))
		if _, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/simple.json"}); err == nil {
			t.Fatalf("admitted %s", source)
		}
	}
}

func TestJSONWorkflowCheckpointRedaction(t *testing.T) {
	const secret = "test-secret-value"
	e, s := jsonExecutor(t, `{"version":"0.2","steps":[{"agent":"a","prompt":"task"}]}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": secret}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json", Secrets: map[string]string{"token": secret}, SecretRefs: map[string]string{"token": "TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	retained, _, err := s.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(retained)
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "REDACTED") {
		t.Fatalf("unsafe checkpoint: %s", data)
	}
}

func TestJSONWorkflowRegisteredActionCheckpoints(t *testing.T) {
	var calls atomic.Int32
	e, s := jsonExecutor(t, `{"version":"0.2","steps":[{"id":"first","action":"capture","with":{"value":{"literal":"${{ secrets.token }}"}}},{"action":"capture","with":{"value":"$.steps.first.value"}}]}`, nil)
	registerTestAction(t, e, readSpec(&calls))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/simple.json"})
	if err != nil || calls.Load() != 2 || run.Outputs["value"] != "${{ secrets.token }}" {
		t.Fatalf("%+v %v calls=%d", run, err, calls.Load())
	}
	records, _ := s.ActionAttempts(context.Background(), run.ID)
	if len(records) != 2 || records[0].State != "completed" {
		t.Fatalf("missing action provenance: %+v", records)
	}
	if _, err = e.Resume(context.Background(), run.ID, nil, false); err != nil || calls.Load() != 2 {
		t.Fatalf("completed action repeated: %v", err)
	}
}

type definitionFaultStore struct {
	*SQLiteStore
	loseCheckpoint bool
	failCompletion bool
	broken         bool
}

func (s *definitionFaultStore) Save(ctx context.Context, run *Run) error {
	if s.loseCheckpoint && run.DefinitionState != nil {
		for _, call := range run.DefinitionState.Calls {
			if !call.Pending {
				s.broken = true
			}
		}
	}
	if s.broken {
		return errors.New("fixture storage failure after invocation receipt")
	}
	return s.SQLiteStore.Save(ctx, run)
}
func (s *definitionFaultStore) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	if s.failCompletion {
		return errors.New("fixture storage failure before invocation receipt")
	}
	return s.SQLiteStore.Complete(ctx, a, result)
}

func TestJSONWorkflowCrashBoundaries(t *testing.T) {
	for _, lostReceipt := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed_receipt", true: "uncertain_effect"}[lostReceipt], func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, s := jsonExecutor(t, `{"version":"0.2","steps":[{"agent":"a","prompt":"first"},{"agent":"a","prompt":"second"}]}`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{"text": "done"}, nil
			}))
			faulty := &definitionFaultStore{SQLiteStore: s, loseCheckpoint: !lostReceipt, failCompletion: lostReceipt}
			e.Store = faulty
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/simple.json"})
			if err == nil || calls.Load() != 1 {
				t.Fatalf("failure did not stop dependents: %v calls=%d", err, calls.Load())
			}
			faulty.loseCheckpoint = false
			faulty.failCompletion = false
			faulty.broken = false
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			resumed, err := e.Resume(ctx, run.ID, nil, false)
			if lostReceipt {
				if err == nil || calls.Load() != 1 {
					t.Fatalf("uncertain effect replayed: %v calls=%d", err, calls.Load())
				}
				resumed, err = e.Resume(ctx, run.ID, nil, true)
			}
			want := int32(2)
			if lostReceipt {
				want = 3
			}
			if err != nil || resumed.Status != RunStatusSucceeded || calls.Load() != want {
				t.Fatalf("resume=%+v err=%v calls=%d", resumed, err, calls.Load())
			}
		})
	}
}
