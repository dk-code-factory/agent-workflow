package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type managedAccountingTurn struct {
	ctx context.Context
	req workflows.SessionRequest
}

func TestManagedCodexCancelledBatchCloseJoinsAccounting(t *testing.T) {
	store, _, turn, accounting := managedAccountingFixture(t, 10*time.Second)
	definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
	binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
	lease, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var observations atomic.Int32
	controller, err := newCodexManagedBatchController(turn.ctx, &workflows.InvocationTools{Binding: binding, Lease: lease}, "thread", turn.req.InvocationID, "policy", "build", func(ctx context.Context) error {
		return workflows.CheckManagedCodexAuthority(ctx, accounting.run, accounting.invocation)
	}, func(_ context.Context, batch codexParkedBatch) error {
		if observations.Add(1) != 1 {
			return errors.New("retained batch observed twice")
		}
		close(entered)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return errors.New("accounting fixture gate was not released")
		}
		return accounting.seal(batch)
	}, nil)
	if err != nil {
		lease.Release()
		t.Fatal(err)
	}
	var closed chan struct{}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		controller.close()
		if closed != nil {
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Error("close caller did not join")
			}
		}
		lease.Release()
	})
	callbackCtx, cancelCallback := context.WithCancel(t.Context())
	defer cancelCallback()
	done := make(chan error, 1)
	go func() {
		value, err := controller.handle(callbackCtx, managedCallFrame(definition, "original-call"))
		if value != nil {
			err = errors.New("cancelled callback emitted a permit")
		}
		done <- err
	}()
	managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 1 })
	batch := managedAccountingBatch(turn.req.InvocationID)
	controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
	cancelCallback()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("original callback cancellation changed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not cancel before start")
	}
	if err := controller.bindStart("turn"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("retained response accounting never started")
	}
	closed = make(chan struct{})
	go func() { controller.close(); close(closed) }()
	managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return controller.closed })
	select {
	case <-closed:
		t.Fatal("close returned while admitted accounting was gated")
	default:
	}
	if err := controller.bindStart("turn"); err == nil {
		t.Fatal("closing controller accepted another start")
	}
	late := make(chan struct{})
	go func() {
		defer close(late)
		controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
	}()
	select {
	case <-late:
	case <-time.After(3 * time.Second):
		t.Fatal("notification blocked behind accounting close")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close did not join completed accounting")
	}
	if observations.Load() != 1 || binding.calls.Load() != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
		t.Fatal("cancelled batch repeated accounting or released effects/capacity")
	}
	report, err := store.Usage(t.Context(), accounting.run)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.Complete {
		t.Fatal("close erased the retained incomplete charge", report, err)
	}
}

func TestManagedCodexOversizedBrokerBatchRetainsKnownCharge(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprintf("valid-witness=%v", valid), func(t *testing.T) {
			store, _, turn, accounting := managedAccountingFixture(t, 10*time.Second)
			definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			}}
			pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
			lease, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			controller, err := newCodexManagedBatchController(turn.ctx, &workflows.InvocationTools{Binding: binding, Lease: lease}, "thread", turn.req.InvocationID, "policy", "build", func(ctx context.Context) error {
				return workflows.CheckManagedCodexAuthority(ctx, accounting.run, accounting.invocation)
			}, func(_ context.Context, batch codexParkedBatch) error { return accounting.seal(batch) }, nil)
			if err != nil {
				lease.Release()
				t.Fatal(err)
			}
			t.Cleanup(func() { controller.close(); lease.Release() })
			batch := managedAccountingBatch(turn.req.InvocationID)
			batch.CallIDs = nil
			if !valid {
				batch.Usage = json.RawMessage(`{"input_tokens":11,"cached_input_tokens":12,"output_tokens":3,"total_tokens":14}`)
			}
			// Nine individually permitted 1 MiB JSON strings exceed the 8 MiB
			// broker aggregate bound without approaching the callback count cap.
			arguments := json.RawMessage(`"` + strings.Repeat("x", (1<<20)-2) + `"`)
			type reply struct {
				value any
				err   error
			}
			done := make(chan reply, 9)
			for i := range 9 {
				id := fmt.Sprintf("original-%02d", i)
				batch.CallIDs = append(batch.CallIDs, id)
				frame := managedCallFrame(definition, id)
				var call codexBrokerCall
				if err := json.Unmarshal(frame.Params, &call); err != nil {
					t.Fatal(err)
				}
				call.Arguments = arguments
				frame.Params = managedTestFrame(call)
				go func() { value, err := controller.handle(t.Context(), frame); done <- reply{value, err} }()
			}
			managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 9 })
			controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
			if err := controller.bindStart("turn"); err != nil {
				t.Fatal(err)
			}
			for range 9 {
				select {
				case result := <-done:
					if result.value != nil || result.err == nil {
						t.Fatal("oversized broker batch emitted a permit", result)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("oversized broker callback did not return")
				}
			}
			controller.close()
			if binding.calls.Load() != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
				t.Fatal("oversized batch dispatched or suspended")
			}
			report, err := store.Usage(t.Context(), accounting.run)
			if err != nil || len(report.Invocations) != 1 {
				t.Fatal(report, err)
			}
			observation, present := report.Invocations[0].Observations["codex-response:original-response"]
			if valid {
				if !present || observation.InputTokens == nil || *observation.InputTokens != 11 || observation.Complete || report.Summary.Complete || report.Invocations[0].Finished {
					t.Fatal("broker argument rejection erased known original charge", report)
				}
			} else if present || report.Summary.InputTokens != nil {
				t.Fatal("invalid native counters acquired a charge", report)
			}
		})
	}
}

type managedAccountingSession struct {
	entered chan managedAccountingTurn
	release chan struct{}
}

func (*managedAccountingSession) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (*managedAccountingSession) UsageProvider(string, string) (string, string) {
	return "codex-app-server", "fixture"
}
func (s *managedAccountingSession) RunTurn(ctx context.Context, req workflows.SessionRequest, _ func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	select {
	case s.entered <- managedAccountingTurn{ctx, req}:
	case <-ctx.Done():
		return workflows.SessionResult{}, ctx.Err()
	}
	// Retain the actual admitted worker while the test inspects a completed
	// response after cancellation. Cleanup releases this wait before joining
	// the scheduler; the independent deadline also bounds failure paths.
	select {
	case <-s.release:
	case <-time.After(10 * time.Second):
		return workflows.SessionResult{}, errors.New("accounting fixture was not released")
	}
	return workflows.SessionResult{}, errors.New("fixture completed observation only")
}

func managedAccountingFixture(t *testing.T, duration time.Duration) (*workflows.SQLiteStore, *workflows.WorkerManager, managedAccountingTurn, *codexManagedAccounting) {
	t.Helper()
	store, err := workflows.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	run := &workflows.Run{ID: strings.Repeat("a", 32), Status: workflows.RunStatusRunning}
	if err := store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(ctx, run, workflows.RunStatusRunning); err != nil {
		t.Fatal(err)
	}
	session := &managedAccountingSession{entered: make(chan managedAccountingTurn, 1), release: make(chan struct{})}
	manager := &workflows.WorkerManager{Store: store, Limits: workflows.WorkerLimits{TurnDuration: duration}, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return session, nil, nil, nil
	}}
	if err := manager.Init(ctx); err != nil {
		t.Fatal(err)
	}
	worker, err := manager.Spawn(ctx, workflows.WorkerSpec{RunID: run.ID, Role: "original", Turns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Send(ctx, run.ID, worker.ID, "original-request", "original task"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); manager.Serve(ctx) }()
	t.Cleanup(func() {
		close(session.release)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("accounting worker did not stop")
		}
	})
	var turn managedAccountingTurn
	select {
	case turn = <-session.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("original worker was not admitted")
	}
	if err := workflows.BeginManagedCodexInvocation(turn.ctx, run.ID, turn.req.InvocationID, "build", "policy", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := workflows.BindManagedCodexInvocation(turn.ctx, run.ID, turn.req.InvocationID, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	a := &codexManagedAccounting{ctx: turn.ctx, run: run.ID, invocation: turn.req.InvocationID, thread: "thread", policy: "policy", build: "build", model: "fixture"}
	return store, manager, turn, a
}

func managedAccountingBatch(invocation string) codexParkedBatch {
	return codexParkedBatch{ThreadID: "thread", TurnID: "turn", InvocationID: invocation, PolicyID: "policy", BuildID: "build", BatchID: "original-batch", ResponseID: "original-response", CallIDs: []string{"original-call"}, PermitEpoch: "original-epoch", Usage: json.RawMessage(`{"input_tokens":11,"cached_input_tokens":2,"output_tokens":3,"total_tokens":14}`)}
}

func TestManagedCodexBatchRetainsFencedKnownCharge(t *testing.T) {
	for _, kind := range []string{"deadline", "cancellation", "replacement-owner", "corrupt-witness", "callback-cancellation", "callback-replacement-owner", "callback-corrupt-witness", "callback-wrong-turn", "callback-incomplete", "callback-no-start", "callback-closed"} {
		t.Run(kind, func(t *testing.T) {
			duration := 10 * time.Second
			if kind == "deadline" {
				duration = time.Second
			}
			store, manager, turn, accounting := managedAccountingFixture(t, duration)
			original, err := manager.Get(t.Context(), accounting.run, turn.req.ParentWorkerID)
			if err != nil {
				t.Fatal(err)
			}
			messages, err := manager.Messages(t.Context(), accounting.run, original.ID)
			if err != nil || len(messages) != 1 || messages[0].StartedAt.IsZero() {
				t.Fatal("original admitted message missing", messages, err)
			}
			started := messages[0].StartedAt
			controllerCtx := turn.ctx
			if kind == "deadline" {
				// Keep the controller context live so this case tests the stored
				// original worker deadline, separately from cancellation below.
				controllerCtx = context.WithoutCancel(turn.ctx)
			}
			ctx, cancel := context.WithCancel(controllerCtx)
			defer cancel()
			accounting.ctx = ctx
			definition := workflows.ToolDefinition{Server: "engine", Name: "a"}
			binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(context.Context, workflows.ToolRequest) (json.RawMessage, error) {
				return json.RawMessage(`{}`), nil
			}}
			pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
			lease, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			controller, err := newCodexManagedBatchController(ctx, &workflows.InvocationTools{Binding: binding, Lease: lease}, "thread", turn.req.InvocationID, "policy", "build", func(ctx context.Context) error {
				return workflows.CheckManagedCodexAuthority(ctx, accounting.run, accounting.invocation)
			}, func(_ context.Context, batch codexParkedBatch) error { return accounting.seal(batch) }, nil)
			if err != nil {
				lease.Release()
				t.Fatal(err)
			}
			t.Cleanup(func() { controller.close(); lease.Release() })
			type callbackReply struct {
				value any
				err   error
			}
			callbackCtx, cancelCallback := context.WithCancel(t.Context())
			defer cancelCallback()
			done := make(chan callbackReply, 1)
			go func() {
				value, err := controller.handle(callbackCtx, managedCallFrame(definition, "original-call"))
				done <- callbackReply{value, err}
			}()
			managedWait(t, func() bool { controller.mu.Lock(); defer controller.mu.Unlock(); return len(controller.pending) == 1 })
			batch := managedAccountingBatch(turn.req.InvocationID)
			if kind == "corrupt-witness" || kind == "callback-corrupt-witness" {
				batch.Usage = json.RawMessage(`{"input_tokens":11,"cached_input_tokens":12,"output_tokens":3,"total_tokens":14}`)
			}
			if kind == "callback-incomplete" {
				batch.CallIDs = append(batch.CallIDs, "unreceived-call")
			}
			controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
			callbackFailed := strings.HasPrefix(kind, "callback-")
			if callbackFailed {
				cancelCallback()
				select {
				case reply := <-done:
					if reply.value != nil || !errors.Is(reply.err, context.Canceled) {
						t.Fatal("callback cancellation was not retained", reply)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("callback cancellation did not finish before start")
				}
				controller.mu.Lock()
				cause := controller.err
				controller.mu.Unlock()
				if !errors.Is(cause, context.Canceled) {
					t.Fatal("callback failure was not installed before start", cause)
				}
			}
			switch kind {
			case "deadline":
				select {
				case <-turn.ctx.Done():
				case <-time.After(3 * time.Second):
					t.Fatal("original worker deadline did not expire")
				}
				managedWait(t, func() bool {
					err := workflows.CheckManagedCodexAuthority(ctx, accounting.run, accounting.invocation)
					return err != nil && strings.Contains(err.Error(), "deadline expired")
				})
			case "cancellation":
				cancel()
			case "replacement-owner", "callback-replacement-owner":
				run, _, err := store.Load(t.Context(), accounting.run)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Claim(t.Context(), run, workflows.RunStatusRunning); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "callback-no-start":
				// A native witness alone cannot establish the official turn.
			case "callback-closed":
				controller.close()
				if err := controller.bindStart("turn"); err == nil {
					t.Fatal("closed controller accepted a later start")
				}
			case "callback-wrong-turn":
				if err := controller.bindStart("foreign-turn"); err == nil {
					t.Fatal("foreign official turn was accepted")
				}
			default:
				if err := controller.bindStart("turn"); err != nil {
					t.Fatal(err)
				}
			}
			if callbackFailed {
				// Close must join any retained accounting without clearing the
				// callback failure or allowing another notification to add work.
				controller.close()
				late := make(chan struct{})
				go func() {
					defer close(late)
					controller.notify(rpcFrame{Method: "agentWorkflow/batchParked", Params: managedTestFrame(batch)})
				}()
				select {
				case <-late:
				case <-time.After(3 * time.Second):
					t.Fatal("closed notification did not return")
				}
				if err := controller.bindStart("turn"); err == nil {
					t.Fatal("closed controller accepted another start")
				}
				controller.mu.Lock()
				cause := controller.err
				controller.mu.Unlock()
				if !errors.Is(cause, context.Canceled) {
					t.Fatal("retention cleared original callback failure", cause)
				}
			}
			// The start operation adds its worker before returning. Join it before
			// inspecting accounting; a cancelled callback can return earlier.
			joined := make(chan struct{})
			go func() { controller.wg.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(3 * time.Second):
				t.Fatal("batch execution did not settle")
			}
			if !callbackFailed {
				select {
				case reply := <-done:
					if reply.err == nil || reply.value != nil {
						t.Fatal("rejected batch returned success or a permit", reply)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("callback remained parked")
				}
			}
			if binding.calls.Load() != 0 || pool.Suspended() != 0 || len(pool.Slots) != 1 {
				t.Fatal("rejected batch dispatched, suspended or lost its lease")
			}
			fresh, err := workflows.OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			report, err := fresh.Usage(t.Context(), accounting.run)
			if closeErr := fresh.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || len(report.Invocations) != 1 {
				t.Fatal("original accounting missing", report, err)
			}
			observation, present := report.Invocations[0].Observations["codex-response:original-response"]
			known := kind == "deadline" || kind == "cancellation" || kind == "callback-cancellation"
			if known {
				if !present || observation.InputTokens == nil || *observation.InputTokens != 11 || observation.CachedInputTokens == nil || *observation.CachedInputTokens != 2 || observation.OutputTokens == nil || *observation.OutputTokens != 3 || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || observation.Complete || report.Summary.Complete || report.Invocations[0].Finished || report.Invocations[0].ID != turn.req.InvocationID || report.Invocations[0].SessionID != "thread" {
					t.Fatal("known original charge lost or falsely completed", report)
				}
			} else if present || report.Summary.InputTokens != nil {
				t.Fatal("rejected owner or witness acquired a charge", report)
			}
			worker, err := manager.Get(t.Context(), accounting.run, turn.req.ParentWorkerID)
			if err != nil || worker.Remaining != original.Remaining || worker.Remaining != 0 || worker.SessionID != original.SessionID || worker.Status != workflows.RunStatusRunning {
				t.Fatal("original consumed allowance changed", worker, err)
			}
			messages, err = manager.Messages(t.Context(), accounting.run, original.ID)
			if err != nil || len(messages) != 1 || messages[0].ID != turn.req.InvocationID || !messages[0].StartedAt.Equal(started) || messages[0].Status != workflows.RunStatusRunning {
				t.Fatal("retention changed original message admission", messages, err)
			}
		})
	}
}

func TestManagedCodexAccountingSealUsesOriginalWitness(t *testing.T) {
	store, _, turn, accounting := managedAccountingFixture(t, 10*time.Second)
	batch := managedAccountingBatch(turn.req.InvocationID)
	if err := accounting.observe(codexModelWitness{Batch: codexSemanticBatch(batch), Usage: batch.Usage}); err != nil {
		t.Fatal(err)
	}
	batch.PermitEpoch = "fresh-epoch"
	batch.Usage = json.RawMessage(`{"total_tokens":14,"output_tokens":3,"cached_input_tokens":2,"input_tokens":11}`)
	if err := accounting.seal(batch); err != nil {
		t.Fatal("canonical observation and durable seal differed", err)
	}
	batch.Usage = json.RawMessage(`{"input_tokens":12,"cached_input_tokens":2,"output_tokens":3,"total_tokens":15}`)
	if err := accounting.seal(batch); err == nil {
		t.Fatal("changed completed witness replaced the durable proof")
	}
	report, err := store.Usage(t.Context(), accounting.run)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.Complete {
		t.Fatal("seal replay changed original incomplete charge", report, err)
	}
}
