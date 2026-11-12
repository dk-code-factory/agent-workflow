package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

type messageWaitOutcome struct {
	message WorkerMessage
	err     error
}

// startMessageWait returns only after the caller has subscribed and committed a
// pending registration. No provider has to run to establish that barrier.
func startMessageWait(t *testing.T, m *WorkerManager, req WorkerWaitRequest) (<-chan messageWaitOutcome, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	outcome := make(chan messageWaitOutcome, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		message, err := m.WaitMessage(ctx, req)
		outcome <- messageWaitOutcome{message, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("message waiter did not stop")
		}
	})
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var raw []byte
		err := m.Store.db.QueryRow("SELECT record FROM worker_waits WHERE run_id=? AND operation_id=?", req.RunID, req.OperationID).Scan(&raw)
		if err == nil {
			var record workerWait
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			if record.Status != "pending" {
				t.Fatalf("wait was not pending at registration: %+v", record)
			}
			return outcome, cancel
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case result := <-outcome:
			t.Fatalf("wait exited before pending registration: %+v", result)
		case <-timeout.C:
			t.Fatal("wait did not register")
		case <-tick.C:
		}
	}
}

func awaitMessageWait(t *testing.T, outcome <-chan messageWaitOutcome) messageWaitOutcome {
	t.Helper()
	select {
	case result := <-outcome:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("message wait did not finish")
		return messageWaitOutcome{}
	}
}

func TestWorkerWaitCapacityOneAndConcurrentExactMessages(t *testing.T) {
	m, req := waitFixture(t)
	next, err := m.Send(context.Background(), req.RunID, req.WorkerID, "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	waits := make([]<-chan messageWaitOutcome, 8)
	for i := range waits {
		request := req
		request.OperationID = fmt.Sprint(i)
		if i%2 == 1 {
			request.MessageID = next.ID
		}
		waits[i], _ = startMessageWait(t, m, request)
	}
	if len(m.Slots) != 0 {
		t.Fatal("pending waits retained execution capacity")
	}
	serveWorkers(t, m)
	for i, outcome := range waits {
		result := awaitMessageWait(t, outcome)
		if result.err != nil {
			t.Fatal(result.err)
		}
		want := req.MessageID
		if i%2 == 1 {
			want = next.ID
		}
		if result.message.ID != want || result.message.Status != RunStatusSucceeded {
			t.Errorf("wrong outcome: %+v", result.message)
		}
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
	next, err := m.Send(context.Background(), req.RunID, req.WorkerID, "second", "second")
	if err != nil {
		t.Fatal(err)
	}
	req.MessageID = next.ID
	if _, err = m.WaitMessage(context.Background(), req); !errors.Is(err, ErrActionIntegrity) {
		t.Fatal("changed binding accepted")
	}
}

func TestWorkerWaitCancellationAndStoppedProducer(t *testing.T) {
	m, req := waitFixture(t)
	outcome, cancel := startMessageWait(t, m, req)
	cancel()
	if result := awaitMessageWait(t, outcome); !errors.Is(result.err, context.Canceled) {
		t.Fatal(result.err)
	}
	retained, err := m.observeWait(context.Background(), req)
	if err != nil || retained.Status != "pending" || !retained.Deadline.Equal(req.Deadline) {
		t.Fatal("cancellation changed durable wait evidence", retained, err)
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
			complete := func() {
				msg := WorkerMessage{ID: req.MessageID, WorkerID: req.WorkerID, Status: RunStatusSucceeded, Result: SessionResult{Status: RunStatusSucceeded, Outputs: map[string]any{"text": "retained"}}}
				raw, _ := json.Marshal(msg)
				if _, err := m.Store.db.Exec("UPDATE messages SET status=?,record=? WHERE id=?", msg.Status, raw, msg.ID); err != nil {
					t.Fatal(err)
				}
			}
			var outcome <-chan messageWaitOutcome
			if !before {
				outcome, _ = startMessageWait(t, m, req)
			}
			complete()
			var msg WorkerMessage
			var err error
			if before {
				msg, err = m.WaitMessage(context.Background(), req)
			} else {
				result := awaitMessageWait(t, outcome)
				msg, err = result.message, result.err
			}
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
