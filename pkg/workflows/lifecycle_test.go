package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
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
	if err == nil || ctx.Err() != nil || calls != 2 {
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
