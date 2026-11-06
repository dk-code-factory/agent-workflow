package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLifecycleArgumentFailurePreservesPromptContext(t *testing.T) {
	for _, field := range []string{"prompt", "message", "ordered/prompt", "ordered/message"} {
		t.Run(field, func(t *testing.T) {
			ordered := strings.HasPrefix(field, "ordered/")
			field = strings.TrimPrefix(field, "ordered/")
			source := `jobs:
  task:
    steps:
      - id: seed
        agent: seed
        prompt: Seed
      - id: work
        uses: agent/primary
        with:
          ` + field + `: 'original ${{ secrets.token }}'
          workspace: '${{ steps.seed.text.foo }}'
        onFailure:
          - agent: observer
            prompt: '${{ failed.prompt }}'
        finally:
          - agent: cleanup
            prompt: '${{ failed.prompt }}'
`
			var calls []string
			e, _ := lifecycleArgumentExecutor(t, source, ordered, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.AgentID)
				if r.AgentID != "seed" && (!strings.HasPrefix(r.Prompt, "original ") || strings.Contains(r.Prompt, "private-token")) {
					t.Errorf("lost or unredacted failed prompt: %q", r.Prompt)
				}
				return map[string]any{"text": "seed"}, nil
			}))
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "private-token"}})
			if err == nil || !strings.Contains(err.Error(), `cannot access result field "foo" on non-object`) {
				t.Fatalf("original argument failure lost: run=%+v err=%v", run, err)
			}
			if !reflect.DeepEqual(calls, []string{"seed", "observer", "cleanup"}) {
				t.Fatalf("failure context prevented handlers: calls=%v err=%v", calls, err)
			}
			_, _ = e.Resume(context.Background(), run.ID, nil, false)
			if !reflect.DeepEqual(calls, []string{"seed", "observer", "cleanup"}) {
				t.Fatalf("terminal argument failure replayed effects: %v", calls)
			}
		})
	}
}

func TestLifecycleFallbackRecoversArgumentFailure(t *testing.T) {
	for _, contract := range []string{"none", "valid", "invalid", "ordered/none", "ordered/valid", "ordered/invalid"} {
		t.Run(contract, func(t *testing.T) {
			ordered := strings.HasPrefix(contract, "ordered/")
			contract = strings.TrimPrefix(contract, "ordered/")
			source := `jobs:
  task:
    outputs:
      result: ${{ steps.work.text }}
    steps:
      - id: seed
        agent: seed
        prompt: Seed
      - id: work
        agent: primary
        prompt: original
        with:
          workspace: '${{ steps.seed.text.foo }}'
        onError:
          - agent: backup
            prompt: Recover
`
			if contract != "none" {
				source = strings.Replace(source, "        with:\n", `        with:
          output:
            schema:
              type: object
              required: [ok]
              properties:
                ok: {type: boolean}
`, 1)
			}
			var calls []string
			e, _ := lifecycleArgumentExecutor(t, source, ordered, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.AgentID)
				value := map[string]any{"ok": true}
				if contract == "invalid" {
					value = map[string]any{"wrong": true}
				}
				return map[string]any{"text": r.AgentID, "structured": value}, nil
			}))
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if contract == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "structured output fallback") {
					t.Fatalf("invalid replacement contract accepted or not checked: run=%+v err=%v", run, err)
				}
			} else if err != nil || run.Jobs["task"].Outputs["result"] != "backup" {
				t.Fatalf("successful fallback did not replace argument failure: run=%+v err=%v", run, err)
			}
			if !reflect.DeepEqual(calls, []string{"seed", "backup"}) {
				t.Fatalf("wrong recovery effects: %v", calls)
			}
			if original := run.Jobs["task"].Steps["work"].Lifecycle.OriginalError; !strings.Contains(original, `cannot access result field "foo" on non-object`) {
				t.Fatalf("original argument failure not retained: %q", original)
			}
			resumed, resumeErr := e.Resume(context.Background(), run.ID, nil, false)
			if (resumeErr == nil) != (contract != "invalid") || resumed == nil || !reflect.DeepEqual(calls, []string{"seed", "backup"}) {
				t.Fatalf("argument recovery replay changed: run=%+v err=%v calls=%v", resumed, resumeErr, calls)
			}
		})
	}
}

func lifecycleArgumentExecutor(t *testing.T, source string, ordered bool, runner AgentRunner) (*Executor, *SQLiteStore) {
	t.Helper()
	if ordered {
		source += `  iteration:
    needs: task
    uses: workflows/child.yml
    for-each: {items: [], key: item.id}
`
	}
	e, store := durableExecutor(t, source, runner)
	if ordered {
		writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\njobs: {child: {steps: [{agent: unused, prompt: unused}]}}\n")
	}
	return e, store
}

func TestLifecycleOrderedArgumentFailureRetainsAdmissionGuards(t *testing.T) {
	ctx := context.Background()
	store, run := executionStore(t)
	e := &Executor{Store: store}
	step := Step{ID: "work", Uses: "agent/a", With: map[string]any{
		"prompt": "${{ inputs.prompt }}", "workspace": "${{ steps.seed.text.foo }}",
	}, OnError: []Step{{Uses: "agent/backup"}}}
	ec := expressionContext{Ordered: true, RunID: run.ID, JobID: "task", IterationNumber: 1,
		Inputs: map[string]any{"prompt": "original"}, Steps: map[string]StepExecution{"seed": {ID: "seed", Status: RunStatusSucceeded, Outputs: map[string]any{"text": "seed"}}}}
	attempt, reused, err := e.beginIterationStep(ctx, store, step, ec, false)
	if err != nil || reused || attempt.InputFingerprint == "" {
		t.Fatalf("failed arguments did not receive durable admission: %+v reused=%v err=%v", attempt, reused, err)
	}
	if _, _, err = e.beginIterationStep(ctx, store, step, ec, false); err == nil || !strings.Contains(err.Error(), "retry-uncertain") {
		t.Fatalf("admitted argument failure bypassed uncertainty: %v", err)
	}
	attempt.Phase = "lifecycle_task_completed"
	attempt.Result = StepExecution{ID: step.ID, Status: RunStatusFailed, Error: "argument failure", FailureClass: "execution"}
	if err = store.RecordAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if restored, _, err := e.beginIterationStep(ctx, store, step, ec, false); err != nil || restored.ID != attempt.ID {
		t.Fatalf("identical failed arguments did not reuse receipt: %+v %v", restored, err)
	}
	ec.Inputs["prompt"] = "changed"
	if _, _, err = e.beginIterationStep(ctx, store, step, ec, false); !errors.Is(err, ErrActionIntegrity) {
		t.Fatalf("changed independently rendered argument bypassed fingerprint: %v", err)
	}
	ec.Inputs["prompt"] = "original"
	ec.Steps["seed"] = StepExecution{ID: "seed", Status: RunStatusSucceeded, Outputs: map[string]any{"text": map[string]any{"foo": "now valid"}}}
	if _, _, err = e.beginIterationStep(ctx, store, step, ec, false); !errors.Is(err, ErrActionIntegrity) {
		t.Fatalf("changed failing argument bypassed fingerprint: %v", err)
	}
}

type interruptArgumentLifecycleJournal struct{ *SQLiteStore }

func (s interruptArgumentLifecycleJournal) RecordAttempt(ctx context.Context, attempt Attempt) error {
	if err := s.SQLiteStore.RecordAttempt(ctx, attempt); err != nil {
		return err
	}
	if attempt.StepID == "work" && attempt.Phase == "lifecycle_task_completed" {
		return errors.New("interrupted after argument failure receipt")
	}
	return nil
}

func TestLifecycleOrderedArgumentFailureResumesOriginalReceipt(t *testing.T) {
	source := `jobs:
  task:
    outputs: {result: '${{ steps.work.text }}'}
    steps:
      - {id: seed, agent: seed, prompt: Seed}
      - id: work
        agent: primary
        prompt: original
        with: {workspace: '${{ steps.seed.text.foo }}'}
        onError: [{agent: backup, prompt: Recover}]
        finally: [{agent: cleanup, prompt: Cleanup}]
`
	var calls []string
	e, store := lifecycleArgumentExecutor(t, source, true, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.AgentID)
		return map[string]any{"text": r.AgentID}, nil
	}))
	e.Store = interruptArgumentLifecycleJournal{store}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil || !reflect.DeepEqual(calls, []string{"seed"}) {
		t.Fatalf("argument failure did not stop after durable task receipt: run=%+v err=%v calls=%v", run, err, calls)
	}
	e.Store = store
	run, err = e.Resume(context.Background(), run.ID, nil, false)
	if err != nil || run.Jobs["task"].Outputs["result"] != "backup" || !reflect.DeepEqual(calls, []string{"seed", "backup", "cleanup"}) {
		t.Fatalf("argument failure receipt did not resume handlers: run=%+v err=%v calls=%v", run, err, calls)
	}
	if life := run.Jobs["task"].Steps["work"].Lifecycle; life == nil || !strings.Contains(life.OriginalError, `cannot access result field "foo" on non-object`) {
		t.Fatalf("resume lost original argument failure: %+v", life)
	}
}
