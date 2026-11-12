package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WorkerWaitRequest is controller-internal. OperationID is a logical pipeline
// coordinate, never an attempt ID. Reattachment cannot extend Deadline.
type WorkerWaitRequest struct {
	RunID, OperationID, WorkerID, MessageID string
	Deadline                                time.Time
}

type workerWait struct {
	WorkerID  string         `json:"worker_id"`
	MessageID string         `json:"message_id"`
	Deadline  time.Time      `json:"deadline"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	Result    *WorkerMessage `json:"result,omitempty"`
}

func (m *WorkerManager) notifyWaiters() {
	m.waitMu.Lock()
	defer m.waitMu.Unlock()
	if m.waitChanged != nil {
		close(m.waitChanged)
	}
	m.waitChanged = make(chan struct{})
}

func (m *WorkerManager) waitNotification() <-chan struct{} {
	m.waitMu.Lock()
	defer m.waitMu.Unlock()
	return m.waitChanged
}

// WaitMessage holds no CapacityLease, container, or provider process. Pending
// waits occupy the caller goroutine; pipeline integration must retain its bounded
// workflow controller reservation.
// Subscribe before observing durable state so completion before or during
// subscription cannot be lost. Serve's shared sweep repairs missed wakeups.
func (m *WorkerManager) WaitMessage(ctx context.Context, req WorkerWaitRequest) (WorkerMessage, error) {
	if req.RunID == "" || req.OperationID == "" || req.WorkerID == "" || req.MessageID == "" || req.Deadline.IsZero() {
		return WorkerMessage{}, fmt.Errorf("exact-message wait requires run, operation, worker, message and deadline")
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		changed := m.waitNotification()
		record, err := m.observeWait(ctx, req)
		if err != nil {
			return WorkerMessage{}, err
		}
		if record.Result != nil {
			return *record.Result, nil
		}
		switch record.Status {
		case "timed_out":
			return WorkerMessage{}, context.DeadlineExceeded
		case "stopped":
			return WorkerMessage{}, fmt.Errorf("worker %s stopped with unresolved message %s: %s", req.WorkerID, req.MessageID, record.Error)
		}
		if timer == nil {
			timer = time.NewTimer(time.Until(record.Deadline))
		}
		select {
		case <-ctx.Done():
			// Detach the in-memory waiter; retained pending state permits explicit
			// resume against the original deadline. Mailbox evidence is never removed.
			return WorkerMessage{}, ctx.Err()
		case <-timer.C:
			// The next observation commits timeout before returning to the pipeline.
		case <-changed:
		}
	}
}

func (m *WorkerManager) observeWait(ctx context.Context, req WorkerWaitRequest) (workerWait, error) {
	var record workerWait
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return record, fmt.Errorf("%w: %v", ErrActionIntegrity, err)
	}
	if err = checkEffectAdmission(ctx, tx, req.RunID); err != nil {
		return record, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.status='running' AND e.finalizing=0`, req.RunID).Scan(&active); err != nil {
		return record, err
	}
	if active != 1 {
		return record, fmt.Errorf("%w: wait owner is finalizing or inactive", ErrActionIntegrity)
	}
	var raw []byte
	var worker Worker
	if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE run_id=? AND id=?", req.RunID, req.WorkerID).Scan(&raw); err != nil {
		return record, err
	}
	if err = decodeJSONWithNumbers(raw, &worker); err != nil {
		return record, err
	}
	var message WorkerMessage
	if err = tx.QueryRowContext(ctx, "SELECT record FROM messages WHERE worker_id=? AND id=?", req.WorkerID, req.MessageID).Scan(&raw); err != nil {
		return record, err
	}
	if err = decodeJSONWithNumbers(raw, &message); err != nil {
		return record, err
	}
	err = tx.QueryRowContext(ctx, "SELECT record FROM worker_waits WHERE run_id=? AND operation_id=?", req.RunID, req.OperationID).Scan(&raw)
	fresh := errors.Is(err, sql.ErrNoRows)
	if !fresh && err != nil {
		return record, err
	}
	if fresh {
		record = workerWait{WorkerID: req.WorkerID, MessageID: req.MessageID, Deadline: req.Deadline, Status: "pending"}
	} else {
		if err = decodeJSONWithNumbers(raw, &record); err != nil {
			return record, err
		}
		if record.WorkerID != req.WorkerID || record.MessageID != req.MessageID {
			return record, fmt.Errorf("%w: wait operation binding changed", ErrActionIntegrity)
		}
	}
	previous := record.Status
	if record.Status == "pending" {
		// A committed terminal mailbox result is authoritative, including when its
		// completion notification or the waiter's completion acknowledgement is lost.
		switch message.Status {
		case RunStatusSucceeded, RunStatusFailed, "interrupted", RunStatusCanceled:
			if !message.FinishedAt.IsZero() && message.FinishedAt.After(record.Deadline) {
				record.Status = "timed_out"
			} else {
				record.Status, record.Result = "completed", &message
			}
		default:
			if !time.Now().Before(record.Deadline) {
				record.Status = "timed_out"
			} else if worker.Status == "closed" || worker.Status == RunStatusNeedsAttention || (worker.Status == "idle" && worker.Remaining == 0) {
				record.Status = "stopped"
				record.Error = worker.Error
			}
		}
	}
	if fresh || record.Status != previous {
		raw, err = json.Marshal(record)
		if err != nil {
			return record, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO worker_waits VALUES(?,?,?) ON CONFLICT(run_id,operation_id) DO UPDATE SET record=excluded.record", req.RunID, req.OperationID, raw); err != nil {
			return record, err
		}
		if err = addEvent(ctx, tx, req.RunID, "worker.wait_"+record.Status, map[string]any{"operation_id": req.OperationID, "worker_id": req.WorkerID, "message_id": req.MessageID, "deadline": record.Deadline}); err != nil {
			return record, err
		}
	}
	return record, tx.Commit()
}
