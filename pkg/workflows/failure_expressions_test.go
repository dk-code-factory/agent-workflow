package workflows

import (
	"context"
	"fmt"
	"testing"
)

func TestFailureClassificationExpressionsUseAuthoritativeMetadata(t *testing.T) {
	for _, test := range []struct{ status, class, unsafe string }{
		{RunStatusSucceeded, "", ""},
		{RunStatusFailed, "provider_failure", ""},
		{RunStatusFailed, "execution", "storage"},
		{RunStatusCanceled, "canceled", "canceled"},
	} {
		t.Run(test.status+"/"+test.class+"/"+test.unsafe, func(t *testing.T) {
			step := StepExecution{ID: "task", Status: test.status, FailureClass: test.class, UnsafeFailureClass: test.unsafe,
				Outputs: map[string]any{"structured": map[string]any{"failure_class": "provider-controlled", "unsafe_failure_class": "provider-controlled"}}}
			job := JobExecution{Steps: map[string]StepExecution{"task": step}}
			ctx := expressionContext{Steps: job.Steps, Needs: map[string]JobExecution{"work": job}, Jobs: map[string]JobExecution{"work": job}}
			for _, prefix := range []string{"steps.task", "needs.work.steps.task", "jobs.work.steps.task"} {
				for field, want := range map[string]string{"failure_class": test.class, "unsafe_failure_class": test.unsafe} {
					got, err := lookupPath(prefix+"."+field, ctx)
					if err != nil || got != want {
						t.Fatalf("%s.%s = %#v, %v; want %q", prefix, field, got, err, want)
					}
					if _, err = lookupPath(prefix+"."+field+".nested", ctx); err == nil {
						t.Fatal("accepted a nested classification field")
					}
				}
			}
			whole, err := lookupPath("steps.task", ctx)
			if err != nil || len(whole.(map[string]any)) != 3 {
				t.Fatal("changed legacy whole-step object", whole, err)
			}
		})
	}
}

func TestObservedNativeFailureClassificationReachesFollowingStep(t *testing.T) {
	e, _, _ := agentTaskFixture(t, `jobs:
  work:
    steps:
      - id: task
        uses: agent/run
        continue-on-error: true
        with: {agent: reviewer.toml, prompt: Work}
      - id: observe
        argv: [echo, '${{ steps.task.failure_class }}', '${{ steps.task.unsafe_failure_class }}']
`, func(SessionRequest) (map[string]any, error) {
		return nil, fmt.Errorf("%w: deterministic provider failure", ErrProviderFailure)
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.Jobs["work"].Steps["observe"].Outputs["stdout"]; got != "provider_failure \n" {
		t.Fatalf("observed classification: %q", got)
	}
}
