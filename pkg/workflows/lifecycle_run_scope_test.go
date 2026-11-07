package workflows

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type interruptRootLifecycleJournal struct{ Journal }

func (s interruptRootLifecycleJournal) RecordAttempt(ctx context.Context, attempt Attempt) error {
	if err := s.Journal.RecordAttempt(ctx, attempt); err != nil {
		return err
	}
	if attempt.Phase == "lifecycle_task_completed" && attempt.Result.Error == "root task failed" {
		return errors.New("simulated interruption after root task receipt")
	}
	return nil
}

func TestLifecycleRestartIgnoresReusableChildReceipts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := `jobs:
  call:
    uses: workflows/child.yml
    continue-on-error: true
  task:
    needs: call
    outputs:
      result: ${{ steps.work.text }}
    steps:
      - id: work
        agent: a
        prompt: root task
        onError:
          - {id: backup, agent: a, prompt: root fallback}
        finally:
          - {agent: a, prompt: root cleanup}
`
	var calls []string
	e, store := durableExecutor(t, source, agentFunc(func(_ context.Context, request AgentRequest) (map[string]any, error) {
		calls = append(calls, request.Prompt)
		switch request.Prompt {
		case "child task":
			return nil, errors.New("child task failed")
		case "child fallback":
			return nil, errors.New("storage failed in child fallback")
		case "root task":
			return nil, errors.New("root task failed")
		default:
			return map[string]any{"text": request.Prompt}, nil
		}
	}))
	writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs:
  task:
    steps:
      - id: work
        agent: a
        prompt: child task
        onError:
          - {id: backup, agent: a, prompt: child fallback}
`)
	e.Store = interruptRootLifecycleJournal{Journal: store}
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil {
		t.Fatalf("root task was not interrupted: run=%+v err=%v", run, err)
	}
	childID := run.Jobs["call"].ChildRunID
	if childID == "" || childID == run.ID {
		t.Fatalf("missing reusable child: %+v", run.Jobs["call"])
	}
	attempts, err := store.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rootReceipt, foreignHardReceipt bool
	for _, attempt := range attempts {
		if attempt.JobID != "task" || attempt.Iteration != 1 {
			continue
		}
		if attempt.RunID == run.ID && attempt.StepID == "work" && attempt.Phase == "lifecycle_task_completed" {
			rootReceipt = true
		}
		if attempt.RunID == childID && attempt.StepID == "work/onError/backup" && attempt.Phase == "lifecycle_completed" && attempt.Result.FailureClass == "storage" {
			foreignHardReceipt = true
		}
	}
	if !rootReceipt || !foreignHardReceipt {
		t.Fatalf("missing colliding physical-run receipts: %+v", attempts)
	}
	e.Store = store
	resumed, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || resumed == nil || resumed.Status != RunStatusSucceeded {
		t.Fatalf("foreign child failure prevented root recovery: run=%+v err=%v", resumed, err)
	}
	work := resumed.Jobs["task"].Steps["work"]
	if resumed.Jobs["task"].Outputs["result"] != "root fallback" || work.Lifecycle.OriginalError != "root task failed" || len(work.Lifecycle.Handlers["onError"]) != 1 || work.Lifecycle.Handlers["onError"][0].Status != RunStatusSucceeded || len(work.Lifecycle.Handlers["finally"]) != 1 {
		t.Fatalf("root lifecycle evidence changed: %+v", work)
	}
	if want := []string{"child task", "child fallback", "root task", "root fallback", "root cleanup"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("completed tasks replayed or root handlers skipped: calls=%v want=%v", calls, want)
	}
}
