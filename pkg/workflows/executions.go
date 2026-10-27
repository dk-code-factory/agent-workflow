package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *SQLiteStore) ResumeRequest(ctx context.Context, id, requestID string, retry bool) (*Run, error) {
	var b []byte
	var previous bool
	if requestID == "" {
		return nil, fmt.Errorf("run is already active")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT record,retry_uncertain FROM executions WHERE run_id=? AND request_id=?", id, requestID).Scan(&b, &previous); err != nil {
		return nil, err
	}
	if previous != retry {
		return nil, fmt.Errorf("request_id reused with different content")
	}
	var run Run
	if err := decodeJSONWithNumbers(b, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// QueueResume atomically admits a new execution without modifying the snapshot
// or any attempt. Duplicate request identities refer to the original generation.
func (s *SQLiteStore) QueueResume(ctx context.Context, id, requestID string, retry bool, maxQueued int) (*Run, error) {
	if len(requestID) > 128 {
		return nil, fmt.Errorf("request_id exceeds 128 bytes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var b []byte
	if requestID != "" {
		var previousRetry bool
		err = tx.QueryRowContext(ctx, "SELECT record,retry_uncertain FROM executions WHERE run_id=? AND request_id=?", id, requestID).Scan(&b, &previousRetry)
		if err == nil {
			if previousRetry != retry {
				return nil, fmt.Errorf("request_id reused with different content")
			}
			var run Run
			if err = decodeJSONWithNumbers(b, &run); err != nil {
				return nil, err
			}
			return &run, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	var cleanupPending int
	var retiring int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM retention_groups WHERE origin_id=? AND state!='complete'", id).Scan(&retiring); err != nil {
		return nil, err
	}
	if retiring > 0 {
		return nil, fmt.Errorf("run retention is in progress; finish cleanup instead of resuming")
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM invocations i JOIN runs r ON r.id=i.run_id WHERE i.state IN ('intent','launching','created','running','uncertain') AND (i.state='uncertain' OR r.status!='running')`).Scan(&cleanupPending); err != nil {
		return nil, err
	}
	if cleanupPending > 0 {
		return nil, fmt.Errorf("invocation cleanup must be reconciled before resume")
	}
	var run Run
	var origin string
	if err = tx.QueryRowContext(ctx, "SELECT record,generation,origin_id FROM runs WHERE id=?", id).Scan(&b, &run.Generation, &origin); err != nil {
		return nil, err
	}
	if err = decodeJSONWithNumbers(b, &run); err != nil {
		return nil, err
	}
	if origin != id {
		return nil, fmt.Errorf("control the originating run %s", origin)
	}
	if run.Status == RunStatusQueued || run.Status == RunStatusRunning {
		return nil, fmt.Errorf("run is already active")
	}
	if run.Status == RunStatusSucceeded && !iterationRetry(&run) {
		return &run, nil
	}
	if run.FailureClass == "restore_uncertainty" && !retry {
		return nil, fmt.Errorf("restored effects require explicit retry_uncertain acknowledgement")
	}
	if !retry {
		var uncertain int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE run_id IN (SELECT id FROM runs WHERE origin_id=?) AND status NOT IN ('succeeded','skipped') AND NOT EXISTS (SELECT 1 FROM action_attempts a WHERE a.attempt_id=attempts.id) AND COALESCE(json_extract(CAST(record AS TEXT),'$.phase'),'') NOT IN ('task_completed','repair_admitted','repair_completed')`, id).Scan(&uncertain); err != nil {
			return nil, err
		}
		if uncertain != 0 {
			return nil, fmt.Errorf("explicit retry_uncertain acknowledgement required for uncertain attempts")
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN workers w ON w.id=m.worker_id WHERE w.run_id=? AND m.status IN ('running','failed','interrupted')`, id).Scan(&uncertain); err != nil {
			return nil, err
		}
		if uncertain != 0 {
			return nil, fmt.Errorf("explicit retry_uncertain acknowledgement required for uncertain worker turns")
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tool_calls WHERE run_id=? AND status IN ('dispatched','uncertain')", id).Scan(&uncertain); err != nil {
			return nil, err
		}
		if uncertain != 0 {
			return nil, fmt.Errorf("explicit retry_uncertain acknowledgement required for uncertain tool effects")
		}
	}
	if maxQueued > 0 {
		var queued int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE status='queued'").Scan(&queued); err != nil {
			return nil, err
		}
		if queued >= maxQueued {
			return nil, fmt.Errorf("queued run limit reached")
		}
	}
	previous := run.Status
	run.Generation++
	run.Owner = ""
	run.Status = RunStatusQueued
	run.Error, run.FailureClass = "", ""
	run.FinishedAt = time.Time{}
	b, err = json.Marshal(run)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, "UPDATE runs SET status=?,record=?,generation=?,owner='',admitted=?,updated=? WHERE id=? AND generation=? AND status=?", run.Status, b, run.Generation, now.UnixNano(), now.Format(time.RFC3339Nano), id, run.Generation-1, previous)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("run changed during resume admission")
	}
	var request any
	if requestID != "" {
		request = requestID
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO executions(run_id,generation,request_id,retry_uncertain,admitted,status,record) VALUES(?,?,?,?,?,?,?)", id, run.Generation, request, retry, now.UnixNano(), run.Status, b); err != nil {
		return nil, err
	}
	// The previous generation retains its cancellation observation in executions.
	if _, err = tx.ExecContext(ctx, "DELETE FROM cancellations WHERE run_id=?", id); err != nil {
		return nil, err
	}
	if err = addEvent(ctx, tx, id, "run.resume_admitted", map[string]any{"execution_generation": run.Generation, "request_id": requestID, "retry_uncertain": retry}); err != nil {
		return nil, err
	}
	return &run, tx.Commit()
}

func (s *SQLiteStore) ExecutionRetry(ctx context.Context, id string, generation int64) (bool, error) {
	var retry bool
	err := s.db.QueryRowContext(ctx, "SELECT retry_uncertain FROM executions WHERE run_id=? AND generation=?", id, generation).Scan(&retry)
	return retry, err
}

// BeginFinalization closes durable worker/tool admission before joining work.
// Run status stays running until the entire generation has stopped.
func (s *SQLiteStore) BeginFinalization(ctx context.Context, run *Run) error {
	result, err := s.db.ExecContext(ctx, `UPDATE executions SET finalizing=1 WHERE run_id=? AND generation=? AND EXISTS(SELECT 1 FROM runs WHERE id=? AND generation=? AND owner=? AND status='running')`, run.ID, run.Generation, run.ID, run.Generation, run.Owner)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("execution ownership changed before finalization")
	}
	return nil
}
func (s *SQLiteStore) CheckWorkerAdmission(ctx context.Context, run string) error {
	var allowed int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.status='running' AND e.finalizing=0`, run).Scan(&allowed)
	if err != nil {
		return err
	}
	if allowed != 1 {
		return fmt.Errorf("originating execution is finalizing or inactive")
	}
	return nil
}

// Queued returns at most 100 candidates without decoding terminal history.
func (s *SQLiteStore) Queued(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM runs WHERE status='queued' ORDER BY admitted,id LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var b []byte
		var run Run
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &run); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) QueueDepth(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE status='queued'").Scan(&count)
	return count, err
}
