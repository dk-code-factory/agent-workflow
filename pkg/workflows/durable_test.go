package workflows

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func durableExecutor(t *testing.T, source string, runner AgentRunner) (*Executor, *SQLiteStore) {
	t.Helper()
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", source)
	store, err := OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &Executor{WorkspaceDir: root, Agents: runner, Store: store}, store
}

func TestJournalAdmissionCompletionAndGuardedResume(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	e, s := durableExecutor(t, `jobs:
  build:
    steps:
      - id: edit
        uses: agent/builder
        with: {prompt: edit}
      - id: check
        uses: agent/builder
        with: {prompt: check}
`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		calls.Add(1)
		if req.Prompt == "check" && calls.Load() == 2 {
			return nil, errors.New("lost terminal result")
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("expected failure")
	}
	attempts, err := s.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].Status != RunStatusSucceeded || attempts[1].Status != RunStatusFailed {
		t.Fatalf("bad attempts: %+v", attempts)
	}
	// Change the on-disk workflow; only the original snapshot may be resumed.
	writeWorkflow(t, e.WorkspaceDir, "main.yml", "jobs: {}")
	_, err = e.Resume(ctx, run.ID, nil, false)
	if err == nil {
		t.Fatal("silently repeated failed effect")
	}
	if calls.Load() != 2 {
		t.Fatal("unguarded effect executed")
	}
	run, err = e.Resume(ctx, run.ID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || calls.Load() != 3 {
		t.Fatalf("completed edit was replayed: calls=%d run=%+v", calls.Load(), run)
	}
	attempts, _ = s.Attempts(ctx, run.ID)
	if len(attempts) != 3 || attempts[2].Number != 2 {
		t.Fatalf("attempt history lost: %+v", attempts)
	}
	events, _ := s.Events(ctx, run.ID, 0)
	if len(events) < 7 {
		t.Fatalf("missing transitions: %+v", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			t.Fatal("unordered events")
		}
	}
}

func TestRecoveryDoesNotReplayRunningAttempt(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, "jobs: {x: {steps: [{id: edit, uses: agent/a, with: {prompt: hi}}]}}", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		t.Fatal("uncertain effect repeated")
		return nil, nil
	}))
	run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = RunStatusRunning
	if err = s.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Begin(ctx, run.ID, "x", "edit", 1, false); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.Load(ctx, run.ID)
	if saved.Status != RunStatusNeedsAttention {
		t.Fatal(saved.Status)
	}
	if _, err = e.Resume(ctx, run.ID, nil, false); err == nil {
		t.Fatal("missing explicit retry guard")
	}
}

func TestSQLiteDuplicateAdmissionAndTransition(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, "jobs: {x: {steps: [{argv: [true]}]}}", nil)
	run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, _ := s.Load(ctx, run.ID)
	if err = s.Admit(ctx, run, snap); err == nil {
		t.Fatal("duplicate run admitted")
	}
	a, _, err := s.Begin(ctx, run.ID, "x", "step_1", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Begin(ctx, run.ID, "x", "step_1", 1, false); err == nil {
		t.Fatal("duplicate attempt admitted")
	}
	result := StepExecution{ID: "step_1", Status: RunStatusSucceeded}
	if err = s.Complete(ctx, a, result); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, a, result); err == nil {
		t.Fatal("duplicate completion accepted")
	}
}

func TestSecretRedactionAndResumeReferences(t *testing.T) {
	ctx := context.Background()
	secret := "private\"token\n"
	e, s := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/a, with: {prompt: hi}}]}}", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": secret, "response": secret}, nil
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": secret}, SecretRefs: map[string]string{"token": "TEST_TOKEN"}})
	if err != nil {
		t.Fatal(err)
	}
	saved, snap, err := s.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Jobs["x"].Steps["step_1"].Outputs["text"] != "[REDACTED]" {
		t.Fatalf("credential persisted: %+v", saved)
	}
	if snap.SecretRefs["token"] != "TEST_TOKEN" {
		t.Fatal(snap)
	}
	attempts, _ := s.Attempts(ctx, run.ID)
	if attempts[0].Result.Outputs["text"] != "[REDACTED]" {
		t.Fatal("attempt leaked credential")
	}
	// The database, including WAL and event records, must not contain the secret.
	rows, err := s.db.Query("SELECT data FROM events WHERE run_id=?", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		_ = rows.Scan(&b)
		if strings.Contains(string(b), "private") {
			t.Fatal("event leaked credential")
		}
	}
}

func TestCommandArgvRepeatAndFailureOutputs(t *testing.T) {
	e, _ := durableExecutor(t, `jobs:
  retry:
    repeat: {max-attempts: 3, until: '${{ steps.number.outputs.stdout == "3" }}'}
    outputs: {last: '${{ steps.number.outputs.stdout }}'}
    steps:
      - id: number
        argv: [printf, '%s', '${{ iteration.number }}']
`, nil)
	// Interpolation preserves strings in argv. Numeric exact expressions are
	// deliberately rejected rather than being coerced into executable arguments.
	source := `jobs:
  retry:
    repeat: {max-attempts: 3, until: '${{ iteration.number == 3 }}'}
    outputs: {last: '${{ steps.number.outputs.stdout }}'}
    steps:
      - id: number
        argv: [printf, '%s', 'attempt=${{ iteration.number }}']
`
	writeWorkflow(t, e.WorkspaceDir, "main.yml", source)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	job := run.Jobs["retry"]
	if len(job.Iterations) != 3 || job.Outputs["last"] != "attempt=3" {
		t.Fatalf("bad repeat %+v", job)
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.Replace(source, "iteration.number == 3", "iteration.number == 4", 1))
	run, err = e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || len(run.Jobs["retry"].Iterations) != 3 {
		t.Fatal("exhaustion did not fail")
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", `jobs: {x: {steps: [{id: fail, shell: sh, run: 'echo stdout; echo stderr >&2; exit 7'}]}}`)
	run, err = e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("command failure ignored")
	}
	step := run.Jobs["x"].Steps["fail"]
	if step.Outputs["exit_code"] != 7 || step.Outputs["stdout"] != "stdout\n" || step.Outputs["stderr"] != "stderr\n" {
		t.Fatalf("failure output lost: %+v", step)
	}
}

func TestStructuredRepairDoesNotRepeatTaskPrompt(t *testing.T) {
	var calls int
	e, _ := durableExecutor(t, `jobs:
  x:
    steps:
      - uses: agent/a
        with:
          prompt: 'IMPLEMENT A SECRET FEATURE'
          output: {format: json, schema: {type: object, required: [ok], properties: {ok: {type: boolean}}}}
`, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"text": `{"ok": "yes"}`}, nil
		}
		if !r.RepairOnly || strings.Contains(r.Prompt, "IMPLEMENT A SECRET FEATURE") {
			t.Fatal("repair repeated implementation prompt")
		}
		return map[string]any{"text": `{"ok":true}`}, nil
	}))
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRequiresSecretsAgain(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, "jobs: {x: {steps: [{argv: [false]}]}}", nil)
	r, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"key": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	r.Status = RunStatusNeedsAttention
	_ = s.Save(ctx, r)
	if _, err = e.Resume(ctx, r.ID, nil, true); err == nil {
		t.Fatal("resume allowed without secret")
	}
}

func TestSQLitePermissions(t *testing.T) {
	_, s := durableExecutor(t, "jobs: {x: {steps: [{argv: [true]}]}}", nil)
	info, err := os.Stat(filepath.Join(s.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("database mode %v", info.Mode())
	}
}

func TestRedactionPreservesControlFields(t *testing.T) {
	r := &Run{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: RunStatusRunning, Jobs: map[string]JobExecution{"a": {ID: "a", Status: RunStatusSucceeded, Steps: map[string]StepExecution{"a": {ID: "a", Status: RunStatusSucceeded, AttemptID: "aaaa", Outputs: map[string]any{"text": "a"}}}}}}
	safe := redactRun(r, map[string]string{"token": "a"})
	if safe.ID != r.ID || safe.Status != RunStatusRunning || safe.Jobs["a"].Steps["a"].ID != "a" || safe.Jobs["a"].Steps["a"].AttemptID != "aaaa" {
		t.Fatal("credential redaction corrupted execution identity")
	}
	if safe.Jobs["a"].Steps["a"].Outputs["text"] != "[REDACTED]" || r.Jobs["a"].Steps["a"].Outputs["text"] != "a" {
		t.Fatal("data was not independently redacted")
	}
}

func TestCredentialLiteralsRejectAdmissionWithoutChangingMeaning(t *testing.T) {
	e, s := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/a, with: {prompt: super-secret-value}}]}}", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		t.Fatal("secret literal admitted")
		return nil, nil
	}))
	_, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"key": "super-secret-value"}})
	if err == nil {
		t.Fatal("credential literal persisted")
	}
	runs, err := s.List(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatal("rejected admission left a run")
	}
}

func TestNativeStructuredOutputRetainsTextMeaning(t *testing.T) {
	e, _ := durableExecutor(t, `jobs:
  x:
    steps:
      - id: native
        uses: agent/a
        with:
          prompt: reply
          output: {format: json, repair-attempts: 0, schema: {type: object, required: [ok], properties: {ok: {type: boolean}}}}
`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": "Native structured output is attached", "response": "Native structured output is attached", "structured": map[string]any{"ok": true}}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	outputs := run.Jobs["x"].Steps["native"].Outputs
	if outputs["text"] != "Native structured output is attached" || outputs["structured"].(map[string]any)["ok"] != true {
		t.Fatal("native structured validation changed legacy text meaning", outputs)
	}
}
