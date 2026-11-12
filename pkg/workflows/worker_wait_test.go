package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFixture(t *testing.T) (*WorkerManager, WorkerWaitRequest) {
	t.Helper()
	m, run := workerFixture(t, &fakeSession{})
	w, err := m.Spawn(context.Background(), WorkerSpec{RunID: run, Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Send(context.Background(), run, w.ID, "request", "first")
	if err != nil {
		t.Fatal(err)
	}
	return m, WorkerWaitRequest{RunID: run, WorkerID: w.ID, MessageID: msg.ID, OperationID: "job/step/1", Deadline: time.Now().Add(5 * time.Second)}
}

func TestWorkerWaitCapacityOneAndConcurrentExactMessages(t *testing.T) {
	m, req := waitFixture(t)
	next, err := m.Send(context.Background(), req.RunID, req.WorkerID, "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	serveWorkers(t, m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := req
			request.OperationID = fmt.Sprint(i)
			if i%2 == 1 {
				request.MessageID = next.ID
			}
			msg, err := m.WaitMessage(ctx, request)
			if err != nil {
				t.Error(err)
				return
			}
			if msg.ID != request.MessageID || msg.Status != RunStatusSucceeded {
				t.Errorf("wrong outcome: %+v", msg)
			}
		}(i)
	}
	wg.Wait()
	if len(m.Slots) != 0 {
		t.Fatal("wait retained execution capacity")
	}
}

func TestWorkerWaitRestartRetainsDeadlineAndBinding(t *testing.T) {
	m, req := waitFixture(t)
	req.Deadline = time.Now().Add(80 * time.Millisecond)
	record, err := m.observeWait(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// A new manager reattaches without resending the queued provider message.
	dir := m.Store.Dir
	if err = m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	m.Store = reopened
	other := &WorkerManager{Store: reopened, Factory: m.Factory, Slots: m.Slots}
	if err = other.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	extended := req
	extended.Deadline = time.Now().Add(time.Hour)
	_, err = other.WaitMessage(context.Background(), extended)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	retained, err := other.observeWait(context.Background(), req)
	if err != nil || !retained.Deadline.Equal(record.Deadline) || retained.Status != "timed_out" {
		t.Fatal(retained, err)
	}
	msgs, err := m.Messages(context.Background(), req.RunID, req.WorkerID)
	if err != nil || len(msgs) != 1 || msgs[0].Status != RunStatusQueued {
		t.Fatal(msgs, err)
	}
	req.MessageID = "foreign"
	if _, err = m.WaitMessage(context.Background(), req); err == nil {
		t.Fatal("changed binding accepted")
	}
}

func TestWorkerWaitCancellationAndStoppedProducer(t *testing.T) {
	m, req := waitFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := m.observeWait(ctx, req); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := m.WaitMessage(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.Interrupt(context.Background(), req.RunID, req.WorkerID, true); err != nil {
		t.Fatal(err)
	}
	worker, err := m.Get(context.Background(), req.RunID, req.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	worker.Error = "fixture producer setup failed"
	if err := m.saveWorker(context.Background(), worker); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WaitMessage(context.Background(), req); err == nil || !strings.Contains(err.Error(), worker.Error) {
		t.Fatal("closed producer did not retain its failure", err)
	}
	msgs, err := m.Messages(context.Background(), req.RunID, req.WorkerID)
	if err != nil || len(msgs) != 1 {
		t.Fatal(msgs, err)
	}
}

func TestWorkerWaitTerminalBeforeSubscribeLostNotificationAndCompletion(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(fmt.Sprint(before), func(t *testing.T) {
			m, req := waitFixture(t)
			// A provider cannot consume the slot; the shared scheduler sweep still
			// repairs a missed notification after an externally committed receipt.
			m.Slots <- struct{}{}
			serveWorkers(t, m)
			if _, err := m.observeWait(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			complete := func() {
				msg := WorkerMessage{ID: req.MessageID, WorkerID: req.WorkerID, Status: RunStatusSucceeded, Result: SessionResult{Status: RunStatusSucceeded, Outputs: map[string]any{"text": "retained"}}}
				raw, _ := json.Marshal(msg)
				if _, err := m.Store.db.Exec("UPDATE messages SET status=?,record=? WHERE id=?", msg.Status, raw, msg.ID); err != nil {
					t.Error(err)
				}
			}
			if before {
				complete()
			} else {
				time.AfterFunc(30*time.Millisecond, complete)
			}
			msg, err := m.WaitMessage(context.Background(), req)
			if err != nil || msg.Result.Outputs["text"] != "retained" {
				t.Fatal(msg, err)
			}
			// Simulate a lost wait completion commit, keeping the mailbox receipt.
			if _, err = m.Store.db.Exec("DELETE FROM worker_waits WHERE run_id=?", req.RunID); err != nil {
				t.Fatal(err)
			}
			again, err := m.WaitMessage(context.Background(), req)
			if err != nil || again.ID != msg.ID {
				t.Fatal(again, err)
			}
			<-m.Slots
		})
	}
}

func TestWorkerWaitRejectsStaleOwnersAndForeignMessages(t *testing.T) {
	m, req := waitFixture(t)
	run, _, err := m.Store.Load(context.Background(), req.RunID)
	if err != nil {
		t.Fatal(err)
	}
	stale := *run
	stale.Owner = "stale"
	if _, err = m.WaitMessage(fenceContext(context.Background(), &stale), req); !errors.Is(err, ErrActionIntegrity) {
		t.Fatal(err)
	}
	if err = m.Store.BeginFinalization(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err = m.WaitMessage(context.Background(), req); !errors.Is(err, ErrActionIntegrity) {
		t.Fatal(err)
	}
}
