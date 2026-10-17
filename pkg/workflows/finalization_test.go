package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

func TestResumedExecutionStaysRunningDuringFinalCleanup(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "uncertain-cleanup"}[cleanupFailure], func(t *testing.T) {
			e, store := durableExecutor(t, "jobs: {x: {steps: [{argv: [true]}]}}", nil)
			run, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			run.Status = RunStatusFailed
			if err = store.Save(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			if _, err = store.QueueResume(context.Background(), run.ID, "finalize", false, 128); err != nil {
				t.Fatal(err)
			}
			cleaning := make(chan struct{})
			release := make(chan struct{})
			e.BeforeFinalize = func(ctx context.Context, id string) error {
				close(cleaning)
				<-release
				if ctx.Err() != nil {
					t.Error("cleanup inherited canceled execution context")
				}
				if cleanupFailure {
					return process.ErrCleanup
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := e.Resume(ctx, run.ID, nil, false); done <- err }()
			select {
			case <-cleaning:
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not start")
			}
			current, _, err := store.Load(context.Background(), run.ID)
			if err != nil || current.Status != RunStatusRunning {
				t.Fatalf("terminal status published during cleanup: %+v %v", current, err)
			}
			cancel()
			close(release)
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not finish")
			}
			if cleanupFailure && !errors.Is(err, process.ErrCleanup) {
				t.Fatalf("cleanup cause lost: %v", err)
			}
			current, _, err = store.Load(context.Background(), run.ID)
			expected := RunStatusCanceled
			if cleanupFailure {
				expected = RunStatusNeedsAttention
			}
			if err != nil || current.Status != expected {
				t.Fatalf("wrong final status: %+v %v", current, err)
			}
		})
	}
}
