package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestManagedCodexCapacityAdmissionAfterAuthorization(t *testing.T) {
	for _, kind := range []string{"callback-cancellation", "parent-cancellation", "closed-controller", "explicit-failure", "legitimate"} {
		t.Run(kind, func(t *testing.T) {
			store, manager, turn, accounting := managedAccountingFixture(t, 10*time.Second)
			original, err := manager.Get(t.Context(), accounting.run, turn.req.ParentWorkerID)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := manager.Messages(t.Context(), accounting.run, original.ID)
			if err != nil || len(messages) != 1 || messages[0].StartedAt.IsZero() {
				t.Fatal("original admission missing", messages, err)
			}
			started := messages[0].StartedAt
			parent, cancelParent := context.WithCancel(turn.ctx)
			defer cancelParent()
			callback, cancelCallback := context.WithCancel(t.Context())
			defer cancelCallback()
			definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(ctx context.Context, _ workflows.ToolRequest) (json.RawMessage, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return json.RawMessage(`{}`), nil
			}}
			pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
			lease, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			authorized, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var authorizations atomic.Int32
			controller, err := newCodexManagedBatchController(parent, &workflows.InvocationTools{Binding: binding, Lease: lease}, "thread", turn.req.InvocationID, "policy", "build", func(ctx context.Context) error {
				if err := workflows.CheckManagedCodexAuthority(ctx, accounting.run, accounting.invocation); err != nil {
					return err
				}
				if authorizations.Add(1) == 1 {
					// The real durable authorization has succeeded. Delay its nil
					// return so failure can occupy the interval before suspension.
					close(authorized)
					select {
					case <-release:
					case <-time.After(5 * time.Second):
						return errors.New("authorization fixture was not released")
					}
				}
				return nil
			}, func(_ context.Context, batch codexParkedBatch) error { return accounting.seal(batch) }, nil)
			if err != nil {
				lease.Release()
				t.Fatal(err)
			}
			var closed, joined chan struct{}
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				controller.close()
				for _, done := range []chan struct{}{closed, joined} {
					if done != nil {
						select {
						case <-done:
						case <-time.After(3 * time.Second):
							t.Error("capacity fixture caller did not join")
						}
					}
				}
				lease.Release()
			})
			type reply struct {
				value any
				err   error
			}
			done := make(chan reply, 1)
			go func() {
				value, err := controller.handle(callback, managedCallFrame(definition, "original-call"))
				done <- reply{value, err}
			}()
			managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 1 })
			batch := managedAccountingBatch(turn.req.InvocationID)
			controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
			if err := controller.bindStart("turn"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-authorized:
			case <-time.After(3 * time.Second):
				t.Fatal("real original authorization did not succeed")
			}
			report, err := store.Usage(t.Context(), accounting.run)
			if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.Complete {
				t.Fatal("known response was not committed before authority", report, err)
			}
			if pool.Suspended() != 0 || len(pool.Slots) != 1 {
				t.Fatal("lease released before authorization returned")
			}
			var expected error
			switch kind {
			case "callback-cancellation":
				expected = context.Canceled
				cancelCallback()
			case "parent-cancellation":
				expected = context.Canceled
				cancelParent()
			case "closed-controller":
				closed = make(chan struct{})
				go func() { controller.close(); close(closed) }()
				managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return controller.closed })
				controller.mu.Lock()
				expected = controller.err
				controller.mu.Unlock()
			case "explicit-failure":
				expected = errors.New("original batch failed after authorization")
				controller.fail(expected)
			}
			if expected != nil {
				select {
				case result := <-done:
					if result.value != nil || !errors.Is(result.err, expected) {
						t.Fatal("late original failure was not retained", result, expected)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("failure did not finish callback before authorization return")
				}
				if kind != "parent-cancellation" {
					controller.mu.Lock()
					cause := controller.err
					controller.mu.Unlock()
					if !errors.Is(cause, expected) {
						t.Fatal("failure was not installed before authorization return", cause)
					}
				}
			}
			releaseOnce.Do(func() { close(release) })
			joined = make(chan struct{})
			go func() { controller.wg.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(3 * time.Second):
				t.Fatal("authorized batch did not finish")
			}
			if closed != nil {
				select {
				case <-closed:
				case <-time.After(3 * time.Second):
					t.Fatal("close did not join authorized batch")
				}
			}
			// Inspect before owner cleanup releases the lease: cancellation must
			// leave the originally held slot occupied, without a suspended slot.
			if pool.Suspended() != 0 || len(pool.Slots) != 1 {
				t.Fatalf("late failure released held capacity: suspended=%d occupied=%d", pool.Suspended(), len(pool.Slots))
			}
			if expected == nil {
				select {
				case result := <-done:
					if result.err != nil || result.value == nil {
						t.Fatal("legitimate batch did not complete", result)
					}
					permit, ok := result.value.(map[string]any)["agentWorkflowPermit"].(codexParkedBatch)
					if !ok || permit.InvocationID != batch.InvocationID || permit.ResponseID != batch.ResponseID {
						t.Fatal("legitimate original permit changed", result)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("legitimate callback did not finish")
				}
				if binding.calls.Load() != 1 || authorizations.Load() != 2 {
					t.Fatal("legitimate batch bypassed dispatch or final authority")
				}
			} else if binding.calls.Load() != 0 || authorizations.Load() != 1 {
				t.Fatal("failed batch dispatched after authorization")
			}
			fresh, err := workflows.OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			report, err = fresh.Usage(t.Context(), accounting.run)
			if closeErr := fresh.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || len(report.Invocations) != 1 {
				t.Fatal(report, err)
			}
			observation := report.Invocations[0].Observations["codex-response:original-response"]
			if observation.InputTokens == nil || *observation.InputTokens != 11 || observation.CachedInputTokens == nil || *observation.CachedInputTokens != 2 || observation.OutputTokens == nil || *observation.OutputTokens != 3 || observation.Complete || report.Summary.Complete || report.Invocations[0].Finished || report.Invocations[0].SessionID != "thread" {
				t.Fatal("late failure changed known incomplete response", report)
			}
			worker, err := manager.Get(t.Context(), accounting.run, original.ID)
			if err != nil || worker.Remaining != original.Remaining || worker.Remaining != 0 || worker.SessionID != original.SessionID || worker.Status != workflows.RunStatusRunning {
				t.Fatal("late failure changed original worker", worker, err)
			}
			messages, err = manager.Messages(t.Context(), accounting.run, original.ID)
			if err != nil || len(messages) != 1 || messages[0].ID != turn.req.InvocationID || !messages[0].StartedAt.Equal(started) || messages[0].Status != workflows.RunStatusRunning {
				t.Fatal("late failure changed original message", messages, err)
			}
		})
	}
}
