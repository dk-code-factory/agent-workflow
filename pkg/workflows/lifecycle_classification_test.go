package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLifecycleRestartPreservesRedactedOrdinaryClasses(t *testing.T) {
	for _, tc := range []struct{ class, message, secret string }{
		{"authentication", "authentication failed", "authentication"},
		{"integration", "conflict occurred", "conflict"},
		{"schema", "structured output invalid", "structured output"},
		{"limit", "limit reached", "limit"},
		{"command", "command failed", "command failed"},
	} {
		for _, phase := range []string{"lifecycle_task_completed", "lifecycle_completed"} {
			for _, reportingFails := range []bool{false, true} {
				t.Run(tc.class+"/"+phase+map[bool]string{false: "/reported", true: "/report-failed"}[reportingFails], func(t *testing.T) {
					ctx := context.Background()
					var calls []string
					e, store := durableExecutor(t, `jobs:
  task:
    steps:
      - id: work
        agent: primary
        prompt: original
        onFailure:
          - agent: observer
            prompt: report
        finally:
          - agent: cleanup
            prompt: cleanup
`, agentFunc(func(_ context.Context, request AgentRequest) (map[string]any, error) {
						calls = append(calls, request.AgentID)
						if reportingFails && request.AgentID == "observer" {
							return nil, errors.New("report failed")
						}
						return map[string]any{"text": "done"}, nil
					}))
					secrets := map[string]string{"token": tc.secret}
					run, err := e.Prepare(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: secrets})
					if err != nil {
						t.Fatal(err)
					}
					run.Status = RunStatusRunning
					if err := store.Save(ctx, run); err != nil {
						t.Fatal(err)
					}
					attempt, _, err := store.Begin(ctx, run.ID, "task", "work", 1, false)
					if err != nil {
						t.Fatal(err)
					}
					attempt.Phase = phase
					attempt.Result = StepExecution{ID: "work", Status: RunStatusFailed, Error: tc.message, FailureClass: tc.class, Lifecycle: &StepLifecycle{OriginalError: tc.message, OriginalClass: tc.class, Deadline: time.Now().Add(time.Minute)}}
					journal := redactedStore{Journal: store, secrets: secrets}
					if err := journal.RecordAttempt(ctx, attempt); err != nil {
						t.Fatal(err)
					}
					if err := store.Recover(ctx); err != nil {
						t.Fatal(err)
					}
					resumed, err := e.Resume(ctx, run.ID, secrets, false)
					if err == nil || resumed == nil || resumed.Status != RunStatusFailed {
						t.Fatalf("lost terminal failure: run=%+v err=%v", resumed, err)
					}
					job := resumed.Jobs["task"]
					step := job.Steps["work"]
					if job.FailureClass != tc.class || step.FailureClass != tc.class || step.Lifecycle.OriginalClass != tc.class {
						t.Fatalf("redaction changed retained class %s: job=%s step=%+v", tc.class, job.FailureClass, step)
					}
					if strings.Contains(step.Error, tc.secret) || strings.Contains(err.Error(), tc.secret) {
						t.Fatal("resumed failure exposed its secret")
					}
					var want []string
					if phase == "lifecycle_task_completed" {
						want = []string{"observer", "cleanup"}
					}
					if !reflect.DeepEqual(calls, want) {
						t.Fatalf("unexpected replay: got %v want %v", calls, want)
					}
				})
			}
		}
	}
}

func TestLifecycleRetainedClassAggregatePrecedence(t *testing.T) {
	retained := func(class string) error {
		return lifecycleError(StepExecution{Status: RunStatusFailed, Error: "[REDACTED]", FailureClass: class})
	}
	for _, tc := range []struct{ first, second, want string }{
		{"authentication", "schema", "authentication"},
		{"schema", "command", "schema"},
		{"limit", "command", "limit"},
		{"authentication", "timeout", "timeout"},
		{"authentication", "storage", "storage"},
	} {
		t.Run(tc.first+"/"+tc.second, func(t *testing.T) {
			for _, combined := range []error{
				errors.Join(retained(tc.first), retained(tc.second)),
				errors.Join(retained(tc.second), retained(tc.first)),
			} {
				if got := classifyFailure(combined); got != tc.want {
					t.Fatalf("aggregate classification: got %s want %s", got, tc.want)
				}
			}
		})
	}
}

func TestLifecycleRetainedCanceledFailureIdentity(t *testing.T) {
	for _, tc := range []struct {
		class string
		cause error
	}{
		{"timeout", context.DeadlineExceeded},
		{"provider_failure", ErrProviderFailure},
		{"usage_exhausted", ErrUsageExhausted},
	} {
		t.Run(tc.class, func(t *testing.T) {
			retained := lifecycleError(StepExecution{
				Status: RunStatusFailed, Error: "[REDACTED]",
				FailureClass: tc.class, UnsafeFailureClass: "canceled",
			})
			for _, err := range []error{retained, errors.Join(retained, nil), errors.Join(errors.New("independent failure"), retained)} {
				if !errors.Is(err, tc.cause) || !errors.Is(err, context.Canceled) {
					t.Fatalf("retained failure lost its original or cancellation identity: %v", err)
				}
				if got := classifyFailure(err); got != tc.class {
					t.Fatalf("retained canceled failure changed class: got %s want %s", got, tc.class)
				}
				if got := unsafeFailureClass(err); got != "canceled" {
					t.Fatalf("retained cancellation lost its admission guard: %s", got)
				}
			}
		})
	}
}
