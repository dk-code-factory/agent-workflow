package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type managedBatchBinding struct {
	catalog []workflows.ToolDefinition
	call    func(context.Context, workflows.ToolRequest) (json.RawMessage, error)
	calls   atomic.Int32
}

func (b *managedBatchBinding) Catalog() []workflows.ToolDefinition { return b.catalog }
func (b *managedBatchBinding) Call(ctx context.Context, req workflows.ToolRequest) (json.RawMessage, error) {
	b.calls.Add(1)
	return b.call(ctx, req)
}
func managedTestFrame(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
func managedWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("managed fixture condition timed out")
}
func managedFixture(t *testing.T, binding *managedBatchBinding, authorize func(context.Context) error) (*codexManagedBatchController, *workflows.CapacityLease, *workflows.Capacity) {
	t.Helper()
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := newCodexManagedBatchController(context.Background(), &workflows.InvocationTools{Binding: binding, Lease: lease}, "thread", "original-invocation", "policy", "build", authorize, func(context.Context, codexParkedBatch) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.close(); lease.Release() })
	return controller, lease, pool
}
func managedCallFrame(definition workflows.ToolDefinition, id string) rpcFrame {
	return rpcFrame{Method: "item/tool/call", Params: managedTestFrame(codexBrokerCall{ThreadID: "thread", TurnID: "turn", CallID: id, Tool: workflows.ToolWireName(definition), Arguments: json.RawMessage(`{"original":true}`)})}
}
func managedAttestation(ids ...string) rpcFrame {
	return rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(codexParkedBatch{ThreadID: "thread", TurnID: "turn", InvocationID: "original-invocation", PolicyID: "policy", BuildID: "build", BatchID: "original-batch", ResponseID: "original-response", CallIDs: ids, PermitEpoch: "fresh-owner-epoch", Usage: json.RawMessage(`{"input_tokens":11,"output_tokens":3,"total_tokens":14}`)})}
}

func TestManagedCodexParallelBatchCapacityOneWaitsForAllResults(t *testing.T) {
	definitions := []workflows.ToolDefinition{{Server: "engine", Name: "a"}, {Server: "engine", Name: "b"}}
	binding := &managedBatchBinding{catalog: definitions}
	var authorized atomic.Int32
	var accounted atomic.Bool
	controller, _, pool := managedFixture(t, binding, func(context.Context) error { authorized.Add(1); return nil })
	controller.observe = func(_ context.Context, batch codexParkedBatch) error {
		if batch.ResponseID != "original-response" || !validCodexNativeUsage(batch.Usage) {
			return errors.New("foreign usage")
		}
		accounted.Store(true)
		return nil
	}
	started := make(chan string, 2)
	gates := map[string]chan struct{}{"original-a": make(chan struct{}), "original-b": make(chan struct{})}
	binding.call = func(ctx context.Context, req workflows.ToolRequest) (json.RawMessage, error) {
		if !accounted.Load() {
			return nil, errors.New("effect before usage commit")
		}
		child, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer child.Release()
		started <- req.CallID
		select {
		case <-gates[req.CallID]:
			return json.RawMessage(fmt.Sprintf(`{"receipt":%q}`, req.CallID)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	type reply struct {
		value any
		err   error
	}
	replies := make(chan reply, 2)
	for index, id := range []string{"original-a", "original-b"} {
		frame := managedCallFrame(definitions[index], id)
		go func() { value, err := controller.handle(context.Background(), frame); replies <- reply{value, err} }()
	}
	managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 2 })
	controller.notify(managedAttestation("original-a", "original-b"))
	if binding.calls.Load() != 0 || pool.Suspended() != 0 {
		t.Fatal("attestation before official start reply dispatched effects")
	}
	if err := controller.bindStart("turn"); err != nil {
		t.Fatal(err)
	}
	var first, second string
	select {
	case first = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("parent lease did not release after complete verified batch")
	}
	close(gates[first])
	select {
	case second = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("parent reacquired before second parallel child could acquire capacity 1")
	}
	select {
	case result := <-replies:
		t.Fatalf("partial batch emitted a permit: %+v", result)
	default:
	}
	if second == first {
		t.Fatal("logical call was dispatched twice")
	}
	close(gates[second])
	for range 2 {
		select {
		case result := <-replies:
			if result.err != nil {
				t.Fatal(result.err)
			}
			permit := result.value.(map[string]any)["agentWorkflowPermit"].(codexParkedBatch)
			if permit.InvocationID != "original-invocation" || len(permit.CallIDs) != 2 {
				t.Fatalf("partial/foreign permit %+v", permit)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("complete result batch remained parked")
		}
	}
	if binding.calls.Load() != 2 || authorized.Load() != 2 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
		t.Fatal("batch lost identities, lease or exact owner revalidation")
	}
}

func TestManagedCodexMalformedCompleteBatchDispatchesNoEffects(t *testing.T) {
	definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
	for _, kind := range []string{"omitted", "duplicate", "wrong-invocation", "late-unplanned"} {
		t.Run(kind, func(t *testing.T) {
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			}}
			controller, _, pool := managedFixture(t, binding, func(context.Context) error { return nil })
			if err := controller.bindStart("turn"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			go func() {
				_, err := controller.handle(context.Background(), managedCallFrame(definition, "original-a"))
				done <- err
			}()
			managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 1 })
			frame := managedAttestation("original-a", "original-b")
			switch kind {
			case "omitted":
				frame = managedAttestation("foreign")
			case "duplicate":
				frame = managedAttestation("original-a", "original-a")
			case "wrong-invocation":
				var batch codexParkedBatch
				_ = json.Unmarshal(frame.Params, &batch)
				batch.InvocationID = "foreign"
				frame.Params = managedTestFrame(batch)
			}
			controller.notify(frame)
			if kind == "late-unplanned" {
				go func() {
					_, err := controller.handle(context.Background(), managedCallFrame(definition, "foreign"))
					done <- err
				}()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("malformed batch yielded permit")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("malformed batch did not cancel parked handler")
			}
			if binding.calls.Load() != 0 || pool.Suspended() != 0 {
				t.Fatal("malformed complete plan dispatched effects or released capacity")
			}
		})
	}
}

func TestManagedCodexUncertainBrokerErrorNeverReceivesPermit(t *testing.T) {
	definition := workflows.ToolDefinition{Server: "external", Name: "a"}
	binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
		return nil, workflows.ErrUncertainTool
	}}
	controller, _, _ := managedFixture(t, binding, func(context.Context) error { return nil })
	if err := controller.bindStart("turn"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := controller.handle(context.Background(), managedCallFrame(definition, "original-a"))
		done <- err
	}()
	controller.notify(managedAttestation("original-a"))
	select {
	case err := <-done:
		if !errors.Is(err, workflows.ErrUncertainTool) {
			t.Fatalf("uncertain effect became ordinary failure: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("uncertain callback remained live")
	}
}

func TestManagedCodexUsageCommitFailureDispatchesNoEffect(t *testing.T) {
	for _, kind := range []string{"budget", "storage", "unknown-accounting"} {
		t.Run(kind, func(t *testing.T) {
			definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			}}
			controller, _, pool := managedFixture(t, binding, func(context.Context) error { return nil })
			failure := errors.New(kind)
			var observed atomic.Int32
			controller.observe = func(_ context.Context, batch codexParkedBatch) error {
				if batch.ResponseID != "original-response" {
					t.Error("usage key changed")
				}
				observed.Add(1)
				return failure
			}
			if err := controller.bindStart("turn"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := controller.handle(context.Background(), managedCallFrame(definition, "original-a"))
				done <- err
			}()
			controller.notify(managedAttestation("original-a"))
			select {
			case err := <-done:
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("accounting failure did not hold callbacks")
			}
			if observed.Load() != 1 || binding.calls.Load() != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
				t.Fatal("accounting failure dispatched effect or released capacity")
			}
		})
	}
}
