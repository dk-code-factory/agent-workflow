package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

const lifecycleWorkflow = `jobs:
  task:
    outputs:
      result: ${{ steps.work.outputs.text }}
    steps:
      - id: work
        agent: primary
        prompt: original
        onError:
          - id: backup
            agent: backup
            prompt: '${{ failed.prompt }}|${{ error.class }}'
        onFailure:
          - agent: observer
            prompt: report
        finally:
          - agent: cleanup
            prompt: cleanup
`

func TestLifecycleRecoveredResultAndOrder(t *testing.T) {
	var calls []string
	e, s := durableExecutor(t, lifecycleWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.AgentID)
		if r.AgentID == "primary" {
			return nil, errors.New("ordinary failure")
		}
		if r.AgentID == "backup" && r.Prompt != "original|execution" {
			t.Errorf("wrong failed context: %s", r.Prompt)
		}
		return map[string]any{"text": r.AgentID}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Jobs["task"].Outputs["result"] != "backup" || !reflect.DeepEqual(calls, []string{"primary", "backup", "cleanup"}) {
		t.Fatalf("wrong recovery: %+v calls=%v", run, calls)
	}
	life := run.Jobs["task"].Steps["work"].Lifecycle
	if life.OriginalError != "ordinary failure" {
		t.Fatalf("original failure lost: %+v", life)
	}
	attempts, _ := s.Attempts(context.Background(), run.ID)
	if len(attempts) != 3 {
		t.Fatalf("missing durable handler attempts: %+v", attempts)
	}
}

func TestLifecycleReportingAndNestedFailuresPreserveOriginal(t *testing.T) {
	source := strings.Replace(lifecycleWorkflow, "            prompt: '${{ failed.prompt }}|${{ error.class }}'", `            prompt: fallback
            onFailure:
              - agent: nested
                prompt: nested`, 1)
	var calls []string
	e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.AgentID)
		if r.AgentID == "primary" || r.AgentID == "backup" || r.AgentID == "observer" {
			return nil, errors.New(r.AgentID + " failed")
		}
		return map[string]any{"text": "ok"}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("reporting recovered failure")
	}
	if !strings.Contains(err.Error(), "primary failed") || !strings.Contains(err.Error(), "backup failed") || !strings.Contains(err.Error(), "observer failed") {
		t.Fatal(err)
	}
	if run.Status != RunStatusFailed || !reflect.DeepEqual(calls, []string{"primary", "backup", "nested", "observer", "cleanup"}) {
		t.Fatalf("wrong order/status: %s %v", run.Status, calls)
	}
	// A completed failed lifecycle receipt is terminal and never automatically
	// reruns the original task or its failed observers on resume.
	before := len(calls)
	_, _ = e.Resume(context.Background(), run.ID, nil, false)
	if len(calls) != before {
		t.Fatalf("replayed terminal handler effects: %v", calls)
	}
}

func TestLifecycleOriginalStructuredContract(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "valid"}[valid], func(t *testing.T) {
			source := strings.Replace(lifecycleWorkflow, "        prompt: original", `        prompt: original
        with:
          output:
            format: json
            repair-attempts: 0
            schema:
              type: object
              required: [ok]
              properties:
                ok: {type: boolean}`, 1)
			e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				if r.AgentID == "primary" {
					return nil, errors.New("primary failed")
				}
				value := map[string]any{"wrong": true}
				if valid {
					value = map[string]any{"ok": true}
				}
				return map[string]any{"text": "fallback", "structured": value}, nil
			}))
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if (err == nil) != valid {
				t.Fatalf("valid=%v run=%+v err=%v", valid, run, err)
			}
		})
	}
}

func TestLifecycleBypassesUnsafeFailuresAndCancellation(t *testing.T) {
	for _, failure := range []error{errors.New("uncertain provider effect"), errors.New("storage failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e, _ := durableExecutor(t, lifecycleWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls++
				if failure == context.Canceled {
					cancel()
				}
				return nil, failure
			}))
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || calls != 1 {
				t.Fatalf("unsafe handlers ran: %d %v", calls, err)
			}
			life := run.Jobs["task"].Steps["work"].Lifecycle
			if life == nil || life.Skipped["finally"] == "" {
				t.Fatalf("missing finalization skip: %+v", run.Jobs["task"])
			}
		})
	}
}

func TestLifecycleHardHandlerFailuresBypassLaterLists(t *testing.T) {
	for _, original := range []error{errors.New("authentication failed"), context.DeadlineExceeded} {
		for _, failing := range []string{"backup", "observer"} {
			t.Run(original.Error()+"/"+failing, func(t *testing.T) {
				var calls []string
				e, _ := durableExecutor(t, lifecycleWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
					calls = append(calls, r.AgentID)
					if r.AgentID == "primary" {
						return nil, original
					}
					if r.AgentID == failing {
						return nil, errors.New("storage failed")
					}
					return nil, errors.New("ordinary handler failure")
				}))
				run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
				want := []string{"primary", "backup"}
				if failing == "observer" {
					want = append(want, "observer")
				}
				if err == nil || !reflect.DeepEqual(calls, want) || run.Jobs["task"].FailureClass != "storage" {
					t.Fatalf("hard failure lost: calls=%v job=%+v err=%v", calls, run.Jobs["task"], err)
				}
				if run.Jobs["task"].Steps["work"].Lifecycle.Skipped["finally"] != "unsafe failure" {
					t.Fatal("hard failure did not record skipped finalization")
				}
			})
		}
	}
}

func TestLifecycleMixedCancellationRetainsDiagnosticAndSafety(t *testing.T) {
	ctx := context.Background()
	var calls []string
	source := strings.Replace(lifecycleWorkflow, "        prompt: original", "        prompt: original\n        continue-on-error: true", 1)
	source += "      - agent: downstream\n        prompt: downstream\n"
	e, s := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.AgentID)
		if r.AgentID == "primary" {
			return nil, errors.New("primary failed independently")
		}
		if r.AgentID == "backup" {
			return nil, context.Canceled
		}
		return map[string]any{"text": "unexpected"}, nil
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || ctx.Err() != nil || !reflect.DeepEqual(calls, []string{"primary", "backup"}) {
		t.Fatalf("mixed cancellation admitted effects: calls=%v err=%v", calls, err)
	}
	result := run.Jobs["task"].Steps["work"]
	if result.FailureClass != "execution" || result.UnsafeFailureClass != "canceled" || !strings.Contains(result.Error, "primary failed independently") || result.Lifecycle.Skipped["finally"] != "unsafe failure" {
		t.Fatalf("mixed cancellation lost diagnostic or safety: %+v", result)
	}
	attempts, err := s.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, attempt := range attempts {
		if attempt.StepID != "work" {
			continue
		}
		found = true
		retained := lifecycleError(attempt.Result)
		if !hardExecutionFailure(retained) || !errors.Is(retained, context.Canceled) || cancellationOnly(retained) || classifyFailure(retained) != "execution" {
			t.Fatalf("journal lost mixed failure identity: %+v %v", attempt.Result, retained)
		}
	}
	if !found {
		t.Fatal("missing mixed failure receipt")
	}
}

func TestLifecycleOrderedUsagePersistenceBypassesHandlers(t *testing.T) {
	var calls []string
	var store *SQLiteStore
	e, s := iterationExecutor(t, agentFunc(func(ctx context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.AgentID)
		if r.AgentID == "primary" {
			_, err := store.db.ExecContext(ctx, "CREATE TRIGGER fail_primary_usage BEFORE INSERT ON usage_observations WHEN NEW.invocation_id='"+r.InvocationID+"' BEGIN SELECT RAISE(ABORT, 'sqlite injected usage failure'); END")
			if err != nil {
				return nil, err
			}
		}
		return map[string]any{"text": "done"}, nil
	}))
	store = s
	writeWorkflow(t, e.WorkspaceDir, "child.yml", "on:\n  workflow_call:\n    inputs:\n      task: {type: string}\n      ordinal: {type: number}\n      identity: {type: string}\n"+lifecycleWorkflow)
	run, err := e.Run(context.Background(), iterationRequest("first", "second"))
	if err == nil || !errors.Is(err, errIterationBlocked) || !reflect.DeepEqual(calls, []string{"primary"}) {
		t.Fatalf("ordered persistence failure admitted handlers: calls=%v run=%+v err=%v", calls, run, err)
	}
	found := false
	for _, item := range run.Jobs["review"].Items {
		if item.Jobs == nil {
			continue
		}
		result := item.Jobs["task"].Steps["work"]
		found = true
		if result.FailureClass != "iteration_blocked" || result.Lifecycle == nil || result.Lifecycle.Skipped["finally"] != "unsafe failure" || !errors.Is(lifecycleError(result), errIterationBlocked) {
			t.Fatalf("ordered failure receipt lost safety: %+v", result)
		}
	}
	if !found {
		t.Fatal("missing ordered failure receipt")
	}
}

func TestLifecycleRestartPreservesHardFailures(t *testing.T) {
	for _, failure := range []error{ErrActionIntegrity, process.ErrCleanup, operational.ErrUnavailable, errors.New("storage failed"), errors.New("uncertain effect"), iterationPersistence(errors.New("sqlite failed"), true), errors.Join(errors.New("command failed independently"), context.Canceled)} {
		for _, phase := range []string{"lifecycle_task_completed", "lifecycle_completed"} {
			t.Run(classifyFailure(failure)+"/"+phase, func(t *testing.T) {
				ctx := context.Background()
				source := strings.Replace(lifecycleWorkflow, "  task:\n", "  task:\n    continue-on-error: true\n", 1)
				source += "  dependent:\n    needs: task\n    steps:\n      - agent: downstream\n        prompt: downstream\n"
				calls := 0
				e, s := durableExecutor(t, source, agentFunc(func(_ context.Context, _ AgentRequest) (map[string]any, error) {
					calls++
					return map[string]any{"text": "unexpected"}, nil
				}))
				run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "storage"}})
				if err != nil {
					t.Fatal(err)
				}
				run.Status = RunStatusRunning
				if err := s.Save(ctx, run); err != nil {
					t.Fatal(err)
				}
				attempt, _, err := s.Begin(ctx, run.ID, "task", "work", 1, false)
				if err != nil {
					t.Fatal(err)
				}
				status, class := executionFailureStatus(ctx, failure), classifyFailure(failure)
				attempt.Phase = phase
				attempt.Result = StepExecution{ID: "work", Status: status, Error: failure.Error(), FailureClass: class, UnsafeFailureClass: unsafeFailureClass(failure), Lifecycle: &StepLifecycle{OriginalError: failure.Error(), OriginalClass: class, Deadline: time.Now().Add(time.Minute)}}
				journal := redactedStore{Journal: s, secrets: map[string]string{"token": "storage"}}
				if err := journal.RecordAttempt(ctx, attempt); err != nil {
					t.Fatal(err)
				}
				if err := s.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				resumed, resumeErr := e.Resume(ctx, run.ID, map[string]string{"token": "storage"}, false)
				if resumed == nil {
					t.Fatalf("resume failed before lifecycle execution: %v", resumeErr)
				}
				job := resumed.Jobs["task"]
				if job.Status != status || job.FailureClass != class || job.Steps["work"].Status != status || job.Steps["work"].FailureClass != class {
					t.Fatalf("retained failure changed: expected %s/%s, got %+v", status, class, job)
				}
				if status == RunStatusNeedsAttention {
					if calls != 0 || resumed.Status != RunStatusNeedsAttention || resumed.Jobs["dependent"].Status != RunStatusSkipped {
						t.Fatalf("retained hard failure admitted effects: calls=%d run=%+v", calls, resumed)
					}
				} else if calls != 1 || resumed.Jobs["dependent"].Status != RunStatusSucceeded {
					t.Fatalf("continued failed job changed behavior: calls=%d run=%+v", calls, resumed)
				}
				if phase == "lifecycle_task_completed" && job.Steps["work"].Lifecycle.Skipped["finally"] != "unsafe failure" {
					t.Fatal("retained unsafe failure admitted finalization")
				}
			})
		}
	}
}

func TestLifecycleTimeoutBoundsHandlers(t *testing.T) {
	source := strings.Replace(lifecycleWorkflow, "        prompt: original", "        prompt: original\n        handlerTimeout: 20ms", 1)
	calls := 0
	e, _ := durableExecutor(t, source, agentFunc(func(ctx context.Context, r AgentRequest) (map[string]any, error) {
		calls++
		if r.AgentID == "primary" {
			return nil, errors.New("failed")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || ctx.Err() != nil || calls < 1 || calls > 2 {
		t.Fatalf("handler not locally bounded: calls=%d err=%v", calls, err)
	}
	if run.Jobs["task"].Steps["work"].Lifecycle.Skipped["finally"] != "handler deadline exhausted" {
		t.Fatal("missing skipped finalization")
	}
}

func TestLifecycleAdmissionRejectsUnsupportedAndDuplicateScopes(t *testing.T) {
	for _, source := range []string{
		"onError: []\njobs: {x: {steps: [{argv: [true]}]}}",
		"jobs: {x: {finally: [], steps: [{argv: [true]}]}}",
		"jobs: {x: {steps: [{uses: input/wait, with: {prompt: hi, evidence-id: hi}, finally: [{argv: [true]}]}]}}",
		"jobs: {x: {steps: [{argv: [true], handlerTimeout: 6m, finally: [{argv: [true]}]}]}}",
		"jobs: {x: {steps: [{argv: [true], finally: [{id: a, argv: [true]}, {id: a, argv: [true]}]}]}}",
	} {
		w, err := Parse([]byte(source))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Fatalf("invalid lifecycle admitted: %s", source)
		}
	}
}

func TestLifecycleRestartReusesCompletedTaskAndHandler(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "uncertain"}[uncertain], func(t *testing.T) {
			ctx := context.Background()
			var calls []string
			e, s := durableExecutor(t, lifecycleWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.AgentID)
				return map[string]any{"text": r.AgentID}, nil
			}))
			run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			run.Status = RunStatusRunning
			if err = s.Save(ctx, run); err != nil {
				t.Fatal(err)
			}
			original, _, err := s.Begin(ctx, run.ID, "task", "work", 1, false)
			if err != nil {
				t.Fatal(err)
			}
			original.Phase = "lifecycle_task_completed"
			original.Result = StepExecution{ID: "work", Status: RunStatusFailed, Error: "primary failed", FailureClass: "execution", Lifecycle: &StepLifecycle{OriginalError: "primary failed", OriginalClass: "execution", Deadline: time.Now().Add(time.Minute)}}
			if err = s.RecordAttempt(ctx, original); err != nil {
				t.Fatal(err)
			}
			backup, _, err := s.Begin(ctx, run.ID, "task", "work/onError/backup", 1, false)
			if err != nil {
				t.Fatal(err)
			}
			if !uncertain {
				backup.Phase = "lifecycle_completed"
				if err = s.Complete(ctx, backup, StepExecution{ID: backup.StepID, Status: RunStatusSucceeded, Outputs: map[string]any{"text": "retained"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			resumed, err := e.Resume(ctx, run.ID, nil, false)
			if uncertain {
				if err == nil || len(calls) != 0 {
					t.Fatalf("uncertain handler replayed: calls=%v err=%v", calls, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"cleanup"}) || resumed.Jobs["task"].Outputs["result"] != "retained" {
				t.Fatalf("completed effects replayed: calls=%v run=%+v", calls, resumed)
			}
		})
	}
}

func TestLifecycleFailureContextAndJournalAreRedacted(t *testing.T) {
	source := strings.Replace(lifecycleWorkflow, "        prompt: original", "        prompt: '${{ secrets.token }}'", 1)
	source = strings.Replace(source, "            prompt: '${{ failed.prompt }}|${{ error.class }}'", "            prompt: '${{ failed.prompt }}|${{ error.message }}'", 1)
	e, s := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		if r.AgentID == "primary" {
			return nil, errors.New("failure private-token")
		}
		if strings.Contains(r.Prompt, "private-token") {
			t.Error("handler received unredacted failure context")
		}
		return map[string]any{"text": "ok"}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "private-token"}})
	if err != nil {
		t.Fatal(err)
	}
	attempts, _ := s.Attempts(context.Background(), run.ID)
	for _, attempt := range attempts {
		if strings.Contains(attempt.Result.Lifecycle.OriginalError, "private-token") {
			t.Fatal("failure persisted credential")
		}
		break
	}
}

func TestLifecycleReferenceScopes(t *testing.T) {
	source := `jobs:
  x:
    steps:
      - id: before
        agent: a
        prompt: before
      - id: owner
        agent: a
        prompt: owner
        finally:
          - id: first
            agent: a
            prompt: '${{ steps.before.text }}'
          - id: second
            agent: a
            prompt: '${{ steps.first.text }}'
`
	w, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if err = Validate(w); err != nil {
		t.Fatal(err)
	}
	e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		return map[string]any{"text": r.Prompt}, nil
	}))
	if _, err = e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"steps.owner.text", "steps.second.text", "steps.missing.text"} {
		w, err = Parse([]byte(strings.Replace(source, "steps.before.text", bad, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if err = Validate(w); err == nil {
			t.Fatalf("in-flight result admitted: %s", bad)
		}
	}
}

func TestLifecycleNamedSchemaReferenceAdmission(t *testing.T) {
	const source = `schemas:
  Result: {type: string, enum: ['${{ steps.before.text }}']}
jobs:
  task:
    steps:
      - {id: before, agent: a, prompt: before}
      - id: owner
        agent: a
        prompt: owner
        onError:
          - id: fallback
            agent: a
            prompt: fallback
            with: {output: {schema: Result}}
          - {id: later, agent: a, prompt: later}
`
	for _, reference := range []string{"before", "owner", "fallback", "later", "missing"} {
		t.Run(reference, func(t *testing.T) {
			calls := 0
			e, _ := durableExecutor(t, strings.Replace(source, "steps.before.text", "steps."+reference+".text", 1), agentFunc(func(_ context.Context, _ AgentRequest) (map[string]any, error) {
				calls++
				return map[string]any{"text": "before"}, nil
			}))
			if reference == "before" {
				if _, err := e.Load("workflows/main.yml"); err != nil {
					t.Fatal(err)
				}
				return
			}
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || calls != 0 {
				t.Fatalf("unavailable named reference admitted effects: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestLifecycleNestingAndCountBounds(t *testing.T) {
	ptr := func(value string) *string { return &value }
	leaf := Step{Agent: ptr("a"), Prompt: ptr("leaf")}
	tree := leaf
	for depth := 0; depth < 8; depth++ {
		parent := leaf
		parent.Finally = []Step{tree}
		tree = parent
	}
	w := &Workflow{Jobs: map[string]Job{"x": {Steps: []Step{tree}}}}
	if err := Validate(w); err != nil {
		t.Fatalf("eight levels rejected: %v", err)
	}
	parent := leaf
	parent.Finally = []Step{tree}
	w.Jobs["x"] = Job{Steps: []Step{parent}}
	if err := Validate(w); err == nil {
		t.Fatal("nine levels admitted")
	}
	children := make([]Step, 101)
	for i := range children {
		children[i] = leaf
	}
	parent = leaf
	parent.Finally = children
	w.Jobs["x"] = Job{Steps: []Step{parent}}
	if err := Validate(w); err == nil {
		t.Fatal("101 handler children admitted")
	}
}
