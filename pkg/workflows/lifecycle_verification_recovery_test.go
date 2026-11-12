package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type interruptVerificationJournal struct {
	*SQLiteStore
	ordinal     int
	interrupted bool
}

func (s *interruptVerificationJournal) RecordAttempt(ctx context.Context, attempt Attempt) error {
	if err := s.SQLiteStore.RecordAttempt(ctx, attempt); err != nil {
		return err
	}
	if attempt.StepID == "task" && attempt.Phase == "lifecycle_task_completed" && attempt.Result.Lifecycle != nil && len(attempt.Result.Lifecycle.Verifications) == s.ordinal {
		s.interrupted = true
		return errors.New("simulated interruption after verification receipt")
	}
	return nil
}

func (s *interruptVerificationJournal) Complete(ctx context.Context, attempt Attempt, result StepExecution) error {
	if s.interrupted && attempt.StepID == "task" {
		return errors.New("simulated owner loss before lifecycle completion")
	}
	return s.SQLiteStore.Complete(ctx, attempt, result)
}

func TestNativeOrderedVerificationReplayPreservesCandidateEvent(t *testing.T) {
	for _, ordinal := range []int{1, 2} {
		t.Run(fmt.Sprintf("candidate_%d", ordinal), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			source := `jobs:
  task:
    steps:
      - id: task
        uses: agent/run
        handlerTimeout: 1m
        with:
          agent: reviewer.toml
          prompt: work
          output: {format: json, repair-attempts: 0, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}
        onResult:
          - id: verify
            uses: agent/run
            with: {agent: reviewer.toml, prompt: verify}
        onIncomplete:
          - id: followup
            uses: agent/run
            with: {agent: reviewer.toml, prompt: followup}
  iteration:
    needs: task
    uses: workflows/child.yml
    for-each: {items: [], key: item.id}
`
			e, store, fixture := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
				if strings.HasPrefix(req.Prompt, "verify") {
					event, ok := req.Input.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("verifier lost event: %v", req.Input)
					}
					candidate, ok := event["result"].(map[string]any)
					if !ok {
						return nil, fmt.Errorf("verifier lost candidate: %v", event)
					}
					if candidate["complete"] == false && event["verification"] != nil {
						return nil, fmt.Errorf("first candidate observed future verdict: %v", event)
					}
					if ordinal == 2 && candidate["complete"] == true {
						previous, ok := event["verification"].(map[string]any)
						if !ok || previous["complete"] != false {
							return nil, fmt.Errorf("replacement lost previous verdict: %v", event)
						}
					}
					return map[string]any{"text": fmt.Sprintf(`{"complete":%t}`, candidate["complete"] == true)}, nil
				}
				complete := ordinal == 1 || strings.HasPrefix(req.Prompt, "followup")
				return map[string]any{"text": fmt.Sprintf(`{"complete":%t}`, complete)}, nil
			})
			writeWorkflow(t, e.WorkspaceDir, "child.yml", "on: {workflow_call: {}}\njobs: {child: {steps: [{argv: [true]}]}}\n")
			fault := &interruptVerificationJournal{SQLiteStore: store, ordinal: ordinal}
			e.Store = fault
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || !fault.interrupted {
				t.Fatalf("verification receipt boundary not interrupted: run=%+v err=%v", run, err)
			}
			attempts, err := store.Attempts(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var retained Attempt
			for _, attempt := range attempts {
				if attempt.StepID == "task" {
					retained = attempt
				}
			}
			if retained.Phase != "lifecycle_task_completed" || retained.InputFingerprint == "" || retained.Result.Lifecycle == nil || len(retained.Result.Lifecycle.Verifications) != ordinal {
				t.Fatalf("missing ordered verification receipt: %+v", retained)
			}
			e.Store = store
			resumed, err := e.Resume(ctx, run.ID, nil, true)
			if err != nil || resumed == nil || resumed.Status != RunStatusSucceeded {
				t.Fatalf("verified candidate could not resume: run=%+v err=%v", resumed, err)
			}
			result := resumed.Jobs["task"].Steps["task"]
			if result.Outputs["complete"] != true || len(result.Lifecycle.Verifications) != ordinal || !result.Lifecycle.Deadline.Equal(retained.Result.Lifecycle.Deadline) {
				t.Fatalf("verification replay changed retained evidence: %+v", result)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if want := ordinal * 2; len(fixture.requests) != want {
				t.Fatalf("verification replay duplicated provider turns: got %d want %d", len(fixture.requests), want)
			}
		})
	}
}
