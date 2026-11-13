package workflows

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNativeContinuedFailureLostCompletionRetainsObservation(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"usage_exhausted", ErrUsageExhausted},
		{"provider_failure", ErrProviderFailure},
	} {
		t.Run(failure.name, func(t *testing.T) {
			e, store, fixture := agentTaskFixture(t, `jobs:
  native:
    outputs:
      status: '${{ steps.task.status }}'
      error: '${{ steps.task.error }}'
      failure_class: '${{ steps.task.failure_class }}'
    steps:
      - id: task
        uses: agent/run
        continue-on-error: true
        with: {agent: reviewer.toml, prompt: work, turns: 8}
  disposition:
    needs: native
    steps:
      - argv: [true]
`, func(SessionRequest) (map[string]any, error) {
				return map[string]any{"text": "original partial analysis"}, failure.err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			e.Store = &blockCompletionFault{SQLiteStore: store, failID: "task"}
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || !strings.Contains(err.Error(), "lost checkpoint write") {
				t.Fatalf("did not fault completion: run=%+v err=%v", run, err)
			}
			workers, err := e.Workers.List(ctx, run.ID)
			if err != nil || len(workers) != 1 {
				t.Fatal(workers, err)
			}
			messages, err := e.Workers.Messages(ctx, run.ID, workers[0].ID)
			if err != nil || len(messages) != 1 || messages[0].FailureClass != failure.name || messages[0].FailureAcknowledgement == nil {
				t.Fatalf("original failure was not durably observed: %+v %v", messages, err)
			}
			original := messages[0]
			if disposition, admitted := run.Jobs["disposition"]; admitted && disposition.Status != RunStatusSkipped {
				t.Fatal("lost completion admitted downstream disposition")
			}
			e.Store = store
			resumed, err := e.Resume(ctx, run.ID, nil, true)
			if err != nil || resumed.Status != RunStatusSucceeded {
				t.Fatalf("resume: %+v %v", resumed, err)
			}
			result := resumed.Jobs["native"].Steps["task"]
			if result.Status != RunStatusFailed || result.FailureClass != failure.name || result.Error != original.Error || result.Outputs["text"] != "original partial analysis" || result.AttemptID == original.TaskAttemptID {
				t.Fatal("physical recovery changed the original failure", result, original)
			}
			if resumed.Jobs["native"].Outputs["status"] != RunStatusFailed || resumed.Jobs["native"].Outputs["failure_class"] != failure.name || resumed.Jobs["disposition"].Status != RunStatusSucceeded {
				t.Fatal("downstream did not observe the continued failure", resumed.Jobs)
			}
			retained, err := e.Workers.Messages(ctx, run.ID, workers[0].ID)
			if err != nil || len(retained) != 1 || !reflect.DeepEqual(original, retained[0]) || retained[0].Generation == resumed.Generation {
				t.Fatal("recovery rewrote the original message or acknowledgement", retained, err)
			}
			fixture.mu.Lock()
			calls := len(fixture.requests)
			fixture.mu.Unlock()
			if calls != 1 {
				t.Fatalf("lost completion retried a continued failure: %d calls", calls)
			}
			ec := expressionContext{RunID: run.ID, JobID: "native"}
			step := Step{ID: "task", Uses: "agent/run"}
			if !e.observedNativeTaskFailure(ctx, step, result, ec) {
				t.Fatal("recovered physical attempt lost exact dispatch/observation binding")
			}
			attempts, err := store.Attempts(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var attempt Attempt
			for _, candidate := range attempts {
				if candidate.ID == result.AttemptID {
					attempt = candidate
				}
			}
			if attempt.AgentTask == nil {
				t.Fatal("recovered attempt has no dispatch")
			}
			for _, field := range []string{"operation", "reference", "prompt"} {
				changed := attempt
				dispatch := *attempt.AgentTask
				changed.AgentTask = &dispatch
				switch field {
				case "operation":
					dispatch.Operation = strings.Repeat("a", 64)
				case "reference":
					dispatch.Reference = "agent_" + strings.Repeat("a", 32)
				case "prompt":
					dispatch.Input.Prompt = "another assignment"
				}
				encoded, encodeErr := json.Marshal(changed)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				if _, err = store.db.ExecContext(ctx, "UPDATE attempts SET record=? WHERE id=?", encoded, changed.ID); err != nil {
					t.Fatal(err)
				}
				if e.observedNativeTaskFailure(ctx, step, result, ec) {
					t.Fatalf("forged %s dispatch borrowed a failure observation", field)
				}
			}
			encoded, encodeErr := json.Marshal(attempt)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if _, err = store.db.ExecContext(ctx, "UPDATE attempts SET record=? WHERE id=?", encoded, attempt.ID); err != nil {
				t.Fatal(err)
			}
			changed := retained[0]
			ack := *changed.FailureAcknowledgement
			ack.Evidence = "another-result"
			changed.FailureAcknowledgement = &ack
			if nativeTaskFailureAcknowledged(changed) {
				t.Fatal("forged failure acknowledgement borrowed original result")
			}
		})
	}
}

func TestNativeContinuedFailureExplicitNextCoordinateKeepsSession(t *testing.T) {
	e, _, fixture := agentTaskFixture(t, `jobs:
  task:
    steps:
      - id: original
        uses: agent/run
        continue-on-error: true
        with: {agent: reviewer.toml, prompt: first, turns: 3}
      - id: retry
        uses: agent/run
        with: {agent: '${{ steps.original.outputs.agent }}', prompt: explicit-retry}
`, func(request SessionRequest) (map[string]any, error) {
		if request.Prompt == "first" {
			return map[string]any{"text": "retained partial analysis"}, ErrUsageExhausted
		}
		return map[string]any{"text": "completed explicit retry"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusSucceeded {
		t.Fatal(run, err)
	}
	steps := run.Jobs["task"].Steps
	if steps["original"].FailureClass != "usage_exhausted" || steps["retry"].Status != RunStatusSucceeded || steps["original"].Outputs["agent"] != steps["retry"].Outputs["agent"] {
		t.Fatal("explicit retry changed original failure/session binding", steps)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 2 || fixture.requests[0].ParentWorkerID != fixture.requests[1].ParentWorkerID || fixture.requests[1].SessionID == "" || fixture.requests[0].SessionID != "" {
		t.Fatal("explicit retry did not retain the original session", fixture.requests)
	}
}
