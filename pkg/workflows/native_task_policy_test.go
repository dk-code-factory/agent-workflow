package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativePolicyFixture struct {
	*nativeTaskFixture
	reject      bool
	alterResult func(*SessionResult)
}

func (f *nativePolicyFixture) AdmitNativeTask(d *AgentDefinition) (*AgentDefinition, error) {
	copy := *d
	if d.ToolPolicy == "" {
		return &copy, nil
	}
	if f.reject || d.ToolPolicy != "read-only" {
		return nil, fmt.Errorf("fixture runtime cannot enforce requested policy")
	}
	policy := &NativeTaskPolicy{Mode: "read-only", Provider: "fixture", Profile: "native-read-only-fixture-v1", NativeTools: []string{"Read"}}
	if d.NativePolicy != nil && !reflect.DeepEqual(d.NativePolicy, policy) {
		return nil, fmt.Errorf("fixture retained policy changed")
	}
	copy.NativePolicy = policy
	return &copy, nil
}

func (f *nativePolicyFixture) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	result, err := f.nativeTaskFixture.RunTurn(ctx, req, emit)
	if req.Definition != nil && req.Definition.NativePolicy != nil {
		result.TaskPolicy = &NativeTaskPolicyEvidence{Grant: *req.Definition.NativePolicy, Agent: req.ParentWorkerID, InvocationID: req.InvocationID, SessionFingerprint: NativeTaskSessionFingerprint(result.SessionID), FormattingTool: req.Output != nil && req.Output.Schema != nil}
	}
	if f.alterResult != nil {
		f.alterResult(&result)
	}
	return result, err
}

func nativePolicyExecutor(t *testing.T, root string, store *SQLiteStore, f *nativePolicyFixture) (*Executor, func()) {
	t.Helper()
	slots := make(chan struct{}, 1)
	m := &WorkerManager{Store: store, Slots: slots, Factory: func(_ context.Context, _, _ string, requested []string) (SessionRunner, []string, map[string]string, error) {
		tools := []string{"Read", "Bash"}
		if requested != nil {
			tools = append([]string{}, requested...)
		}
		return f, tools, nil, nil
	}}
	if err := m.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("policy fixture workers did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return &Executor{WorkspaceDir: root, Agents: f, Store: store, Workers: m, Slots: slots, BeforeFinalize: m.StopRun}, stop
}

func newNativePolicyFixture(t *testing.T, source string, respond func(SessionRequest) (map[string]any, error)) (*Executor, *SQLiteStore, *nativePolicyFixture, func()) {
	t.Helper()
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", source)
	if err := os.WriteFile(filepath.Join(root, "workflows", "reviewer.yaml"), []byte("name: reviewer\ndescription: Validate proposal.\ninstructions: Read the proposal and report a verdict.\ntool-policy: read-only\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLite(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &nativePolicyFixture{nativeTaskFixture: &nativeTaskFixture{respond: respond}}
	e, stop := nativePolicyExecutor(t, root, store, f)
	return e, store, f, stop
}

func TestNativeTaskReadOnlyDurableEvidenceAndContinuation(t *testing.T) {
	e, store, f, stop := newNativePolicyFixture(t, `jobs:
  validate:
    steps:
      - id: first
        uses: agent/run
        with: {agent: reviewer.yaml, prompt: Validate, turns: 4}
      - id: followup
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: Explain the original verdict}
      - id: verify
        argv: [python3, -c, "import sys; assert sys.argv[1]=='read-only'; assert len(sys.argv[2])==64; assert sys.argv[2]==sys.argv[3]; assert sys.argv[4]==sys.argv[5]; print('GRANT_VERIFIED')", '${{ steps.first.outputs.tool_policy.grant.mode }}', '${{ steps.first.outputs.tool_policy.session_fingerprint }}', '${{ steps.followup.outputs.tool_policy.session_fingerprint }}', '${{ steps.first.outputs.tool_policy.agent }}', '${{ steps.followup.outputs.agent }}']
`, func(SessionRequest) (map[string]any, error) {
		return map[string]any{"text": "verdict", "tool_policy": map[string]any{"grant": map[string]any{"mode": "MODEL_FORGED"}}}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "first"}
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil || !strings.Contains(err.Error(), "lost checkpoint write") {
		t.Fatal("completion fault not exercised", run, err)
	}
	workers, err := e.Workers.List(ctx, run.ID)
	if err != nil || len(workers) != 1 || !reflect.DeepEqual(workers[0].Tools, []string{"Read"}) || len(workers[0].ToolGrants) != 0 {
		t.Fatal("worker retained broader authority", workers, err)
	}
	messages, err := e.Workers.Messages(ctx, run.ID, workers[0].ID)
	if err != nil || len(messages) != 1 || messages[0].Result.TaskPolicy == nil {
		t.Fatal("original grant was not durable", messages, err)
	}
	original := messages[0]
	proof := original.Result.TaskPolicy
	if proof.TaskAttemptID != original.TaskAttemptID || proof.InvocationID != original.ID || proof.Agent != workers[0].ID || proof.SessionFingerprint == "" {
		t.Fatal("grant lacks original controller identity", proof)
	}
	stop()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Neither source bytes nor the current caller can widen an admitted session.
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", "reviewer.yaml"), []byte("name: changed\ntool-policy: unrestricted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.WorkspaceDir, "workflows", "main.yml")); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenSQLite(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	replacement, _ := nativePolicyExecutor(t, e.WorkspaceDir, fresh, f)
	resumed, err := replacement.Resume(ctx, run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded {
		t.Fatal("reopened recovery failed", resumed, err)
	}
	first := resumed.Jobs["validate"].Steps["first"]
	if first.AttemptID == original.TaskAttemptID || !reflect.DeepEqual(first.Outputs["tool_policy"], original.Result.Outputs["tool_policy"]) {
		t.Fatal("recovery substituted a new grant", first, original)
	}
	retained, err := replacement.Workers.Messages(ctx, run.ID, workers[0].ID)
	if err != nil || len(retained) != 2 || !reflect.DeepEqual(retained[0], original) || retained[1].Result.TaskPolicy.SessionFingerprint != proof.SessionFingerprint || retained[1].Result.TaskPolicy.InvocationID == proof.InvocationID {
		t.Fatal("original receipt/session changed", retained, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[1].SessionID != original.Result.SessionID || f.requests[1].Definition.ToolPolicy != "read-only" {
		t.Fatal("lost completion replayed or widened provider work", f.requests)
	}
}

func TestNativeTaskReadOnlyExplicitRetryRetainsGrant(t *testing.T) {
	calls := 0
	e, store, _, _ := newNativePolicyFixture(t, `jobs: {validate: {steps: [{id: task, uses: agent/run, with: {agent: reviewer.yaml, prompt: Validate, turns: 4}}]}}`, func(SessionRequest) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"text": "original partial"}, ErrProviderFailure
		}
		return map[string]any{"text": "retried verdict"}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil {
		t.Fatal("expected original native failure", run, err)
	}
	workers, _ := e.Workers.List(ctx, run.ID)
	if len(workers) != 1 {
		t.Fatal(workers)
	}
	original, _ := e.Workers.Messages(ctx, run.ID, workers[0].ID)
	resumed, err := e.Resume(ctx, run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded || calls != 2 {
		t.Fatal("explicit original-session retry failed", resumed, err, calls)
	}
	messages, err := e.Workers.Messages(ctx, run.ID, workers[0].ID)
	if err != nil || len(messages) != 2 || !reflect.DeepEqual(messages[0].Result.TaskPolicy, original[0].Result.TaskPolicy) || messages[1].Result.TaskPolicy.SessionFingerprint != original[0].Result.TaskPolicy.SessionFingerprint || actionHash(messages[1].Result.TaskPolicy.Grant) != actionHash(original[0].Result.TaskPolicy.Grant) {
		t.Fatal("retry replaced the original evidence or authority", messages, err)
	}
	attempts, _ := store.Attempts(ctx, run.ID)
	if len(attempts) != 2 || messages[0].TaskAttemptID == messages[1].TaskAttemptID {
		t.Fatal("retry lost attempt identity", attempts, messages)
	}
}

func TestNativeTaskReadOnlyRejectsTamperedRecoveryReceipt(t *testing.T) {
	for _, variant := range []string{"public-grant", "missing-public-grant", "original-attempt", "session", "output-limit-public-grant", "output-limit-missing-public-grant"} {
		t.Run(variant, func(t *testing.T) {
			e, store, f, stop := newNativePolicyFixture(t, `jobs: {validate: {steps: [{id: task, uses: agent/run, with: {agent: reviewer.yaml, prompt: Validate}}]}}`, func(SessionRequest) (map[string]any, error) {
				if strings.HasPrefix(variant, "output-limit-") {
					return map[string]any{"text": strings.Repeat("x", 16<<10)}, nil
				}
				return map[string]any{"text": "verdict"}, nil
			})
			e.Workers.Limits.MaxOutput = 16 << 10
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			e.Store = &blockCompletionFault{SQLiteStore: store, failID: "task"}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || !strings.Contains(err.Error(), "lost checkpoint write") {
				t.Fatal("completion fault not exercised", run, err)
			}
			stop()
			var raw []byte
			if err := store.db.QueryRowContext(ctx, "SELECT record FROM worker_waits WHERE run_id=?", run.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var receipt workerWait
			if err := decodeJSONWithNumbers(raw, &receipt); err != nil || receipt.Result == nil || receipt.Result.Result.TaskPolicy == nil {
				t.Fatal("missing frozen completion", receipt, err)
			}
			switch variant {
			case "public-grant", "output-limit-public-grant":
				receipt.Result.Result.Outputs["tool_policy"].(map[string]any)["grant"].(map[string]any)["mode"] = "forged"
			case "missing-public-grant", "output-limit-missing-public-grant":
				delete(receipt.Result.Result.Outputs, "tool_policy")
			case "original-attempt":
				receipt.Result.Result.TaskPolicy.TaskAttemptID = "foreign-attempt"
			case "session":
				receipt.Result.Result.TaskPolicy.SessionFingerprint = NativeTaskSessionFingerprint("foreign-session")
			}
			raw, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, "UPDATE worker_waits SET record=? WHERE run_id=?", raw, run.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			fresh, err := OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fresh.Close() })
			replacement, _ := nativePolicyExecutor(t, e.WorkspaceDir, fresh, f)
			resumed, err := replacement.Resume(ctx, run.ID, nil, true)
			if err == nil || resumed == nil || resumed.Status == RunStatusSucceeded || !strings.Contains(err.Error(), ErrActionIntegrity.Error()) {
				t.Fatal("tampered completion became workflow evidence", resumed, err)
			}
			if resumed.Jobs["validate"].Steps["task"].Outputs["tool_policy"] != nil {
				t.Fatal("rejected grant leaked into workflow outputs", resumed)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 1 {
				t.Fatal("tampered receipt replayed provider work", f.requests)
			}
		})
	}
}

func TestNativeTaskReadOnlyContinuationCannotWidenOriginalWorker(t *testing.T) {
	e, _, f, _ := newNativePolicyFixture(t, `jobs: {validate: {steps: [{id: task, uses: agent/run, with: {agent: reviewer.yaml, prompt: Validate, turns: 4}}]}}`, func(SessionRequest) (map[string]any, error) {
		return map[string]any{"text": "verdict"}, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	workers, err := e.Workers.List(ctx, run.ID)
	if err != nil || len(workers) != 1 {
		t.Fatal(workers, err)
	}
	original := workers[0]
	messages, err := e.Workers.Messages(ctx, run.ID, original.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	// A caller cannot strip the declaration, attach a different profile, enable
	// repair, or add supplied code to the original session's next mailbox turn.
	for _, variant := range []string{"declaration", "profile", "repair-only", "repair-output", "skill"} {
		t.Run(variant, func(t *testing.T) {
			task := AgentRequest{Prompt: "Continue with broader authority"}
			definition := *original.Definition
			policy := *definition.NativePolicy
			definition.NativePolicy = &policy
			switch variant {
			case "declaration":
				definition.ToolPolicy, definition.NativePolicy = "", nil
				task.Definition = &definition
			case "profile":
				policy.Profile = "unrestricted-profile"
				task.Definition = &definition
			case "repair-only":
				task.RepairOnly = true
			case "repair-output":
				task.Output = &AgentOutputContract{RepairAttempts: 1}
			case "skill":
				task.Skill = &SkillInvocation{}
			}
			if _, err := e.Workers.sendTask(ctx, run.ID, original.ID, "widen-"+variant, task); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("continuation changed the original binding", err)
			}
		})
	}
	after, err := e.Workers.Get(ctx, run.ID, original.ID)
	if err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("rejected continuation changed worker authority or allowance", after, original, err)
	}
	retained, err := e.Workers.Messages(ctx, run.ID, original.ID)
	if err != nil || !reflect.DeepEqual(retained, messages) {
		t.Fatal("rejected continuation created or changed a mailbox receipt", retained, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal("rejected continuation reached provider", f.requests)
	}
}

func TestNativeTaskReadOnlyPreclaimAndRetainedPolicyRejection(t *testing.T) {
	for _, variant := range []string{"unsupported", "repair", "profile", "ceiling", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			output := ""
			if variant == "repair" {
				output = ", output: {format: json, repair-attempts: 1}"
			}
			e, store, f, _ := newNativePolicyFixture(t, "jobs: {validate: {steps: [{id: task, uses: agent/run, with: {agent: reviewer.yaml, prompt: Validate"+output+"}}]}}", func(SessionRequest) (map[string]any, error) {
				return nil, errors.New("rejected policy dispatched provider work")
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			f.reject = variant == "unsupported"
			prepared, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
			if variant == "unsupported" || variant == "repair" {
				if err == nil {
					t.Fatal("unsupported fresh policy admitted", prepared)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				before, snapshot, err := store.Load(ctx, prepared.ID)
				if err != nil {
					t.Fatal(err)
				}
				definition := snapshot.Definitions["workflows/main.yml"].Jobs["validate"].Steps[0].Definition
				switch variant {
				case "profile":
					definition.NativePolicy.Profile = "unverified-profile"
				case "ceiling":
					definition.NativePolicy.NativeTools = []string{"Read", "Bash"}
				case "runtime":
					f.reject = true
				}
				encoded, _ := json.Marshal(snapshot)
				if _, err := store.db.ExecContext(ctx, "UPDATE runs SET snapshot=? WHERE id=?", encoded, prepared.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := e.Resume(ctx, prepared.ID, nil, true); err == nil {
					t.Fatal("changed queued policy resumed")
				}
				after, _, _ := store.Load(ctx, prepared.ID)
				if after.Generation != before.Generation || after.Status != before.Status {
					t.Fatal("rejected policy claimed the queued run", before, after)
				}
			}
			var attempts int
			if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts").Scan(&attempts); err != nil || attempts != 0 {
				t.Fatal("rejected policy claimed a step", attempts, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 0 {
				t.Fatal("rejected policy reached provider", f.requests)
			}
		})
	}
}

func TestNativeTaskReadOnlyEvidenceCannotBeForged(t *testing.T) {
	policy := &NativeTaskPolicy{Mode: "read-only", Provider: "fixture", Profile: "native-read-only-fixture-v1", NativeTools: []string{"Read"}}
	w := Worker{ID: "agent-original", Definition: &AgentDefinition{ToolPolicy: "read-only", NativePolicy: policy}}
	m := WorkerMessage{ID: "original-message", TaskAttemptID: "original-attempt"}
	for _, variant := range []string{"missing", "agent", "invocation", "session", "grant", "formatting", "attempt"} {
		t.Run(variant, func(t *testing.T) {
			e := &NativeTaskPolicyEvidence{Grant: *policy, Agent: w.ID, InvocationID: m.ID, SessionFingerprint: NativeTaskSessionFingerprint("session-original")}
			result := SessionResult{SessionID: "session-original", TaskPolicy: e, Outputs: map[string]any{"tool_policy": map[string]any{"grant": map[string]any{"mode": "read-only"}}}}
			switch variant {
			case "missing":
				result.TaskPolicy = nil
			case "agent":
				e.Agent = "foreign"
			case "invocation":
				e.InvocationID = "foreign"
			case "session":
				e.SessionFingerprint = NativeTaskSessionFingerprint("foreign")
			case "grant":
				e.Grant.NativeTools = []string{"Read", "Bash"}
			case "formatting":
				e.FormattingTool = true
			case "attempt":
				e.TaskAttemptID = "foreign"
			}
			if err := retainNativeTaskPolicy(w, m, &result, true); !errors.Is(err, ErrActionIntegrity) || result.Outputs["tool_policy"] != nil || result.TaskPolicy != nil {
				t.Fatal("forged policy became evidence", variant, result, err)
			}
		})
	}
	result := SessionResult{TaskPolicy: &NativeTaskPolicyEvidence{Grant: *policy}, Outputs: map[string]any{"tool_policy": "model-claim"}}
	if err := retainNativeTaskPolicy(Worker{}, m, &result, true); err != nil || result.TaskPolicy != nil || result.Outputs["tool_policy"] != nil {
		t.Fatal("ordinary historical task acquired read-only evidence", result, err)
	}
}

func TestNativeTaskReadOnlyOutputLimitRecovery(t *testing.T) {
	for _, lostCompletion := range []bool{false, true} {
		t.Run(fmt.Sprintf("lost-completion=%v", lostCompletion), func(t *testing.T) {
			calls := 0
			// Provider outputs fit the cap; adding controller grant metadata crosses it.
			providerOutputs := map[string]any{"text": strings.Repeat("x", (16<<10)-256), "business": "discard me"}
			raw, err := json.Marshal(providerOutputs)
			if err != nil || len(raw) >= 16<<10 {
				t.Fatal("fixture provider outputs must fit the cap", len(raw), err)
			}
			source := fmt.Sprintf(`jobs: {validate: {steps: [{id: task, uses: agent/run, continue-on-error: %v, with: {agent: reviewer.yaml, prompt: Validate, turns: 4}}]}}`, lostCompletion)
			e, store, f, stop := newNativePolicyFixture(t, source, func(SessionRequest) (map[string]any, error) {
				calls++
				if calls == 1 {
					return providerOutputs, nil
				}
				return map[string]any{"text": "retried verdict"}, nil
			})
			e.Workers.Limits.MaxOutput = 16 << 10
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if lostCompletion {
				e.Store = &blockCompletionFault{SQLiteStore: store, failID: "task"}
			}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil {
				t.Fatal("expected output failure", run, err)
			}
			if lostCompletion {
				if !strings.Contains(err.Error(), "lost checkpoint write") {
					t.Fatal("completion fault not exercised", err)
				}
			} else {
				if run.Status != RunStatusFailed {
					t.Fatal("ordinary limit changed run status", run.Status)
				}
				assertNativeOutputLimitFailure(t, run)
			}
			workers, err := e.Workers.List(ctx, run.ID)
			if err != nil || len(workers) != 1 {
				t.Fatal(workers, err)
			}
			messages, err := e.Workers.Messages(ctx, run.ID, workers[0].ID)
			if err != nil || len(messages) != 1 {
				t.Fatal(messages, err)
			}
			original := messages[0]
			if original.Status != RunStatusFailed || original.FailureClass != "limit" || original.UnsafeFailureClass != "" || original.Error != "worker output exceeds limit" || original.Result.TaskPolicy == nil || len(original.Result.Outputs) != 1 || original.Result.Outputs["tool_policy"] == nil {
				t.Fatal("output cap lost diagnostic or retained provider data", original)
			}
			if err := validateRetainedNativeTaskPolicy(workers[0], original); err != nil {
				t.Fatal("controller receipt did not survive output cap", err)
			}
			stop()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(e.WorkspaceDir, "workflows", "main.yml")); err != nil {
				t.Fatal(err)
			}
			fresh, err := OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fresh.Close() })
			replacement, _ := nativePolicyExecutor(t, e.WorkspaceDir, fresh, f)
			if lostCompletion {
				// Continue-on-error recovery exposes the frozen failure without retry.
				recovered, err := replacement.Resume(ctx, run.ID, nil, true)
				if err != nil || recovered.Status != RunStatusSucceeded || calls != 1 {
					t.Fatal("lost completion replayed provider work", recovered, err, calls)
				}
				assertNativeOutputLimitFailure(t, recovered)
				if !reflect.DeepEqual(recovered.Jobs["validate"].Steps["task"].Outputs["tool_policy"], original.Result.Outputs["tool_policy"]) {
					t.Fatal("recovery replaced original public receipt", recovered)
				}
				retained, err := replacement.Workers.Messages(ctx, run.ID, workers[0].ID)
				if err != nil || len(retained) != 1 || !reflect.DeepEqual(retained[0].Result, original.Result) {
					t.Fatal("recovery changed original durable result", retained, err)
				}
				return
			}
			resumed, err := replacement.Resume(ctx, run.ID, nil, true)
			if err != nil || resumed.Status != RunStatusSucceeded || calls != 2 {
				t.Fatal("explicit output-limit retry failed", resumed, err, calls)
			}
			retained, err := replacement.Workers.Messages(ctx, run.ID, workers[0].ID)
			if err != nil || len(retained) != 2 {
				t.Fatal(retained, err)
			}
			// Acknowledgement may be added; the frozen result and grant cannot change.
			if !reflect.DeepEqual(retained[0].Result, original.Result) || retained[1].Result.SessionID != original.Result.SessionID || retained[1].TaskAttemptID == original.TaskAttemptID || retained[1].Result.TaskPolicy.InvocationID == original.ID || actionHash(retained[1].Result.TaskPolicy.Grant) != actionHash(original.Result.TaskPolicy.Grant) {
				t.Fatal("retry replaced original result or widened grant", retained)
			}
		})
	}
}

func assertNativeOutputLimitFailure(t *testing.T, run *Run) {
	t.Helper()
	if run == nil {
		t.Fatal("missing failed run")
	}
	step := run.Jobs["validate"].Steps["task"]
	if step.Status != RunStatusFailed || step.FailureClass != "limit" || step.Error != "worker output exceeds limit" || step.Outputs["text"] != nil || step.Outputs["business"] != nil || step.Outputs["tool_policy"] == nil {
		t.Fatal("normal limit failure was replaced", run.Status, step)
	}
}

func TestNativeTaskReadOnlyOutputLimitHandlers(t *testing.T) {
	e, _, _, _ := newNativePolicyFixture(t, `jobs:
  validate:
    steps:
      - id: task
        uses: agent/run
        with: {agent: reviewer.yaml, prompt: Validate}
        onError: [{id: recover, argv: [echo, recovered]}]
        onFailure: [{id: report, argv: [echo, reported]}]
        finally: [{id: finish, argv: [echo, finalized]}]
`, func(SessionRequest) (map[string]any, error) {
		return map[string]any{"text": strings.Repeat("x", 16<<10)}, nil
	})
	e.Workers.Limits.MaxOutput = 16 << 10
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil || run.Status != RunStatusFailed {
		t.Fatal("limit did not retain ordinary failure status", run, err)
	}
	life := run.Jobs["validate"].Steps["task"].Lifecycle
	if life == nil || life.OriginalError != "worker output exceeds limit" || len(life.Handlers["onError"]) != 1 || life.Handlers["onError"][0].Status != RunStatusSucceeded || len(life.Handlers["onFailure"]) != 1 || life.Handlers["onFailure"][0].Status != RunStatusSucceeded || len(life.Handlers["finally"]) != 1 || life.Handlers["finally"][0].Status != RunStatusSucceeded {
		t.Fatal("limit handlers lost original diagnostic", life)
	}
}

func TestNativeTaskReadOnlyOutputLimitCannotHideIntegrityFailure(t *testing.T) {
	for _, variant := range []string{"missing", "grant"} {
		t.Run(variant, func(t *testing.T) {
			e, _, f, _ := newNativePolicyFixture(t, `jobs: {validate: {steps: [{id: task, uses: agent/run, with: {agent: reviewer.yaml, prompt: Validate}}]}}`, func(SessionRequest) (map[string]any, error) {
				return map[string]any{"text": strings.Repeat("x", 16<<10)}, nil
			})
			f.alterResult = func(result *SessionResult) {
				if variant == "missing" {
					result.TaskPolicy = nil
				} else {
					result.TaskPolicy.Grant.NativeTools = []string{"Read", "Bash"}
				}
			}
			e.Workers.Limits.MaxOutput = 16 << 10
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || run.Status != RunStatusNeedsAttention || run.Jobs["validate"].Steps["task"].FailureClass != "action_integrity" || !strings.Contains(err.Error(), "native task grant evidence is missing or changed") || !strings.Contains(err.Error(), "worker output exceeds limit") {
				t.Fatal("output cap masked invalid successful evidence", run, err)
			}
		})
	}
}
