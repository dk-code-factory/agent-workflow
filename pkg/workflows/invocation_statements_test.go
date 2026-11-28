package workflows

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func TestInvocationStatementCanceledCallerDoesNotWaitForConnection(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(fmt.Sprint(update), func(t *testing.T) {
			store := taskStateStore(t)
			run, ctx := taskStateRun(t, store, "")
			blocked, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			defer func() { _ = tx.Rollback(); cancel(); wg.Wait() }()
			call := func(ctx context.Context) error {
				record := runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, State: "intent"}
				if update {
					return store.UpdateInvocation(ctx, record)
				}
				return store.AdmitInvocation(ctx, record)
			}
			waiting := store.db.Stats().WaitCount
			wg.Add(1)
			go func() { defer wg.Done(); _ = call(blocked) }()
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for store.db.Stats().WaitCount == waiting {
				select {
				case <-deadline.C:
					t.Fatal("cold invocation did not reach the occupied connection")
				case <-time.After(time.Millisecond):
				}
			}
			canceled, cancelCaller := context.WithCancel(ctx)
			cancelCaller()
			result := make(chan error, 1)
			wg.Add(1)
			go func() { defer wg.Done(); result <- call(canceled) }()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled invocation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled invocation waited for another caller's connection")
			}
		})
	}
}

func TestInvocationStatementsConcurrentParametersStayIndependent(t *testing.T) {
	store := taskStateStore(t)
	run, ctx := taskStateRun(t, store, "")
	const count = 8
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record := runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, LogicalID: fmt.Sprint(i), State: "intent"}
			if err := store.AdmitInvocation(ctx, record); err != nil {
				t.Error(err)
				return
			}
			record.State = "cleaned"
			if err := store.UpdateInvocation(ctx, record); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	records, err := store.Invocations(ctx, run.ID, false)
	if err != nil || len(records) != count {
		t.Fatal(records, err)
	}
	seen := map[string]bool{}
	for _, record := range records {
		if record.State != "cleaned" || record.RunID != run.ID || record.Generation != run.Generation || seen[record.LogicalID] {
			t.Fatal("concurrent invocation parameters crossed", record)
		}
		seen[record.LogicalID] = true
	}
	if err = store.BeginFinalization(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err = store.AdmitInvocation(ctx, runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, State: "intent"}); err == nil {
		t.Fatal("reused statement admitted an invocation after finalization")
	}
}

func TestInvocationStatementCancellationDoesNotPoisonLaterCalls(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint(warm), func(t *testing.T) {
			store := taskStateStore(t)
			run, ctx := taskStateRun(t, store, "")
			record := runtimeexec.Record{ID: randomID(), RunID: run.ID, Generation: run.Generation, Owner: run.Owner, State: "intent"}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if warm {
				first := record
				first.ID = randomID()
				if err := store.AdmitInvocation(ctx, first); err != nil {
					t.Fatal(err)
				}
				first.State = "cleaned"
				if err := store.UpdateInvocation(ctx, first); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.AdmitInvocation(canceled, record); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled admission", err)
			}
			if err := store.AdmitInvocation(ctx, record); err != nil {
				t.Fatal("later admission", err)
			}
			record.State = "cleaned"
			if err := store.UpdateInvocation(canceled, record); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled update", err)
			}
			if err := store.UpdateInvocation(ctx, record); err != nil {
				t.Fatal("later update", err)
			}
			if err := store.CheckInvocationCleanup(ctx, run.ID); err != nil {
				t.Fatal("cleanup state was not retained", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateInvocation(ctx, record); err == nil {
				t.Fatal("invocation statement survived its database closing")
			}
		})
	}
}
