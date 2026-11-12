package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type iterationCapacitySession struct {
	fakeSession
	active  atomic.Int32
	maximum atomic.Int32
}

func (s *iterationCapacitySession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	current := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.maximum.Load(); current > old && !s.maximum.CompareAndSwap(old, current); old = s.maximum.Load() {
	}
	return s.fakeSession.RunTurn(ctx, req, emit)
}

func TestParallelIterationControllerActionsWaitAtCapacityOne(t *testing.T) {
	for _, blanketLease := range []bool{false, true} {
		t.Run(fmt.Sprint("blanket_handler_lease=", blanketLease), func(t *testing.T) {
			e, store := parallelIterationExecutor(t, 2, 1, nil)
			pool := &Capacity{Slots: e.Slots}
			e.Capacity = pool
			providerRelease := make(chan struct{})
			session := &iterationCapacitySession{fakeSession: fakeSession{
				started: make(chan string, 4), release: providerRelease,
			}}
			workers := &WorkerManager{Store: store, Capacity: pool, Factory: func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return session, []string{"Read"}, nil, nil
			}}
			if err := workers.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			serveWorkers(t, workers)
			e.BeforeFinalize = workers.StopRun
			entered := make(chan string, 4)
			var active, maximum atomic.Int32
			var unused atomic.Int32
			spec := readSpec(&unused)
			spec.Handler = func(ctx context.Context, ac ActionContext, in actionInput) (ActionResult[actionOutput], error) {
				if blanketLease {
					// Negative control: a lease held while waiting prevents the
					// queued worker from acquiring the sole execution slot.
					lease, err := pool.Acquire(ctx)
					if err != nil {
						return ActionResult[actionOutput]{}, err
					}
					defer lease.Release()
				}
				current := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
				}
				_, snap, err := store.Load(ctx, ac.RunID)
				if err != nil {
					return ActionResult[actionOutput]{}, err
				}
				origin := nativeString(snap.Metadata, "origin_run_id")
				worker, err := workers.Spawn(ctx, WorkerSpec{RunID: origin, Role: "reviewer", Turns: 1})
				if err != nil {
					return ActionResult[actionOutput]{}, err
				}
				message, err := workers.Send(ctx, origin, worker.ID, "review", in.Value)
				if err != nil {
					return ActionResult[actionOutput]{}, err
				}
				entered <- in.Value
				message, err = workers.WaitMessage(ctx, WorkerWaitRequest{
					RunID: origin, WorkerID: worker.ID, MessageID: message.ID,
					OperationID: ac.OperationKey, Deadline: time.Now().Add(5 * time.Second),
				})
				if err != nil {
					return ActionResult[actionOutput]{}, err
				}
				if message.Status != RunStatusSucceeded {
					return ActionResult[actionOutput]{}, fmt.Errorf("worker ended as %s", message.Status)
				}
				return ActionResult[actionOutput]{Output: actionOutput{nativeString(message.Result.Outputs, "text")}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}, nil
			}
			registerTestAction(t, e, spec)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
      ordinal: {type: number}
      identity: {type: string}
    outputs:
      text: {value: '${{ jobs.review.outputs.text }}'}
jobs:
  review:
    outputs: {text: '${{ steps.run.outputs.value }}'}
    steps:
      - id: run
        uses: action/capture
        with: {value: '${{ inputs.task }}'}
`)
			ctx, cancel, done := startParallelIteration(t, e, iterationRequest("a", "b", "c", "d"))
			iterationReceive(t, ctx, entered)
			if blanketLease {
				if len(pool.Slots) != 1 || session.maximum.Load() != 0 {
					t.Fatal("negative control did not retain the worker's required slot")
				}
				cancel()
				joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				result := iterationReceive(t, joined, done)
				if result.err == nil || result.run.Jobs["review"].Complete || active.Load() != 0 {
					t.Fatal("negative control unexpectedly completed", result.run, result.err)
				}
				return
			}
			iterationReceive(t, ctx, entered)
			iterationReceive(t, ctx, session.started)
			if active.Load() != 2 || maximum.Load() != 2 || session.active.Load() != 1 || len(pool.Slots) != 1 {
				t.Fatal("controller callbacks and provider capacity were conflated", active.Load(), maximum.Load(), session.active.Load(), len(pool.Slots))
			}
			close(providerRelease)
			result := iterationReceive(t, ctx, done)
			if result.err != nil || !result.run.Jobs["review"].Complete || maximum.Load() != 2 || active.Load() != 0 || session.maximum.Load() != 1 || session.active.Load() != 0 || len(pool.Slots) != 0 {
				t.Fatal(result.run, result.err, maximum.Load(), active.Load(), session.maximum.Load(), session.active.Load(), len(pool.Slots))
			}
			for i, task := range []string{"a", "b", "c", "d"} {
				item := result.run.Jobs["review"].Items[i]
				if item.Status != "completed" || item.Outputs["text"] != task {
					t.Fatal(result.run.Jobs["review"].Items)
				}
			}
		})
	}
}
