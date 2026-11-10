package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// WorkerFailureAcknowledgement records a controller's disposition without
// changing the failed message, original output, or failure classification.
// It neither grants successful coverage nor reopens the worker for dispatch.
type WorkerFailureAcknowledgement struct {
	RequestID      string    `json:"request_id"`
	MessageID      string    `json:"message_id"`
	Generation     int64     `json:"execution_generation"`
	Reason         string    `json:"reason"`
	Evidence       string    `json:"evidence"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

type WorkerFailureRequest struct {
	RunID, WorkerID, MessageID, Owner, RequestID, Reason, Evidence string
	Generation                                                     int64
}

// AcknowledgeFailure is controller-only; it is deliberately absent from native
// worker tools. Only settled, ordinary failures can be handled. Uncertain
// cleanup, storage, integrity, cancellation, and authorization failures retain
// the existing finalization barrier.
func (m *WorkerManager) AcknowledgeFailure(ctx context.Context, req WorkerFailureRequest) (WorkerFailureAcknowledgement, error) {
	var zero WorkerFailureAcknowledgement
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 128 || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 4096 || strings.TrimSpace(req.Evidence) == "" || len(req.Evidence) > 4096 {
		return zero, fmt.Errorf("failure acknowledgement requires bounded request identity, reason, and evidence")
	}
	if !utf8.ValidString(req.RequestID) || !utf8.ValidString(req.Reason) || !utf8.ValidString(req.Evidence) {
		return zero, fmt.Errorf("failure acknowledgement requires valid UTF-8 request identity, reason, and evidence")
	}
	// Terminal mailbox persistence precedes the provider goroutine's final join.
	m.mu.Lock()
	done := m.done[req.WorkerID]
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, active := m.active[req.WorkerID]; active {
		return zero, fmt.Errorf("worker is active")
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err = checkEffectAdmission(ctx, tx, req.RunID); err != nil {
		return zero, err
	}
	var allowed int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.owner=? AND r.generation=? AND r.status='running' AND e.finalizing=0`, req.RunID, req.Owner, req.Generation).Scan(&allowed)
	if err != nil {
		return zero, err
	}
	if allowed != 1 {
		return zero, fmt.Errorf("failure acknowledgement execution owner changed or admission closed")
	}
	var raw []byte
	var worker Worker
	if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND run_id=?", req.WorkerID, req.RunID).Scan(&raw); err != nil {
		return zero, err
	}
	if err = decodeJSONWithNumbers(raw, &worker); err != nil {
		return zero, err
	}
	var msg WorkerMessage
	if err = tx.QueryRowContext(ctx, "SELECT record FROM messages WHERE id=? AND worker_id=?", req.MessageID, req.WorkerID).Scan(&raw); err != nil {
		return zero, err
	}
	if err = decodeJSONWithNumbers(raw, &msg); err != nil {
		return zero, err
	}
	if msg.Generation != req.Generation || msg.Status != RunStatusFailed || msg.FinishedAt.IsZero() || msg.UnsafeFailureClass != "" || !acknowledgeableFailure(msg.FailureClass) {
		return zero, fmt.Errorf("native failure is not a settled ordinary failure in this execution")
	}
	if prior := msg.FailureAcknowledgement; prior != nil {
		if prior.RequestID != req.RequestID || prior.MessageID != req.MessageID || prior.Generation != req.Generation || prior.Reason != req.Reason || prior.Evidence != req.Evidence {
			return zero, fmt.Errorf("conflicting failure acknowledgement")
		}
		return *prior, nil
	}
	var pending int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE worker_id=? AND status IN ('queued','running')", req.WorkerID).Scan(&pending); err != nil {
		return zero, err
	}
	if pending != 0 || worker.Status == RunStatusRunning {
		return zero, fmt.Errorf("worker mailbox has unsettled turns")
	}
	// These checks share the receipt transaction. Parallel, unrelated workers do
	// not prevent acknowledgement; every invocation/effect of this worker must settle.
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM invocations WHERE run_id=? AND state!='cleaned' AND json_extract(CAST(record AS TEXT),'$.logical_invocation_id') IN (SELECT id FROM messages WHERE worker_id=?)`, req.RunID, req.WorkerID).Scan(&pending); err != nil {
		return zero, err
	}
	if pending != 0 {
		return zero, fmt.Errorf("%w: worker invocations require reconciliation", process.ErrCleanup)
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_calls WHERE run_id=? AND invocation_id IN (SELECT id FROM messages WHERE worker_id=?) AND status IN ('dispatched','uncertain')`, req.RunID, req.WorkerID).Scan(&pending); err != nil {
		return zero, err
	}
	if pending != 0 {
		return zero, ErrUncertainTool
	}
	ack := WorkerFailureAcknowledgement{RequestID: req.RequestID, MessageID: msg.ID, Generation: req.Generation, Reason: req.Reason, Evidence: req.Evidence, AcknowledgedAt: time.Now().UTC()}
	msg.FailureAcknowledgement = &ack
	raw, err = json.Marshal(msg)
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET record=? WHERE id=? AND worker_id=?", raw, msg.ID, worker.ID); err != nil {
		return zero, err
	}
	var last string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM messages WHERE worker_id=? ORDER BY sequence DESC LIMIT 1", worker.ID).Scan(&last); err != nil {
		return zero, err
	}
	if last == msg.ID && worker.FailureGeneration == msg.Generation && worker.FailureClass == msg.FailureClass {
		worker.FailureAcknowledgement = &ack
	}
	worker.Status = "closed"
	raw, err = json.Marshal(worker)
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", worker.Status, raw, worker.ID); err != nil {
		return zero, err
	}
	if err = addEvent(ctx, tx, req.RunID, "worker.failure_acknowledged", ack); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	return ack, nil
}

func acknowledgeableFailure(class string) bool {
	switch class {
	case "", "execution", "provider_failure", "usage_exhausted", "limit", "schema", "command", "integration":
		return true
	}
	return false
}

func unsafeWorkerFailureClass(err error) string {
	if err == nil {
		return ""
	}
	if class := unsafeFailureClass(err); class != "" {
		return class
	}
	var storageErr *runtimeexec.StorageError
	if errors.As(err, &storageErr) {
		return "storage"
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "403") || strings.Contains(text, "forbidden") || strings.Contains(text, "authorization") || strings.Contains(text, "permission denied") || strings.Contains(text, "access denied") || strings.Contains(text, "unauthorized") {
		return "authentication"
	}
	if class := classifyFailure(err); !acknowledgeableFailure(class) {
		return class
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if class := unsafeWorkerFailureClass(cause); class != "" {
				return class
			}
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return unsafeWorkerFailureClass(wrapped.Unwrap())
	}
	return ""
}

func workerPersistenceError(err error) error {
	if err == nil || cancellationOnly(err) || onlyError(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", errStepPersistence, err)
}

type workerInvocationJournal struct {
	runtimeexec.Journal
	observe func(error)
}

func (j workerInvocationJournal) retain(err error) error {
	if err != nil {
		if unsafeWorkerFailureClass(err) == "" {
			err = workerPersistenceError(err)
		}
		j.observe(err)
	}
	return err
}

func (j workerInvocationJournal) AdmitInvocation(ctx context.Context, record runtimeexec.Record) error {
	return j.retain(j.Journal.AdmitInvocation(ctx, record))
}

func (j workerInvocationJournal) UpdateInvocation(ctx context.Context, record runtimeexec.Record) error {
	return j.retain(j.Journal.UpdateInvocation(ctx, record))
}
