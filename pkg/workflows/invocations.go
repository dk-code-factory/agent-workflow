package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func (s *SQLiteStore) AdmitInvocation(ctx context.Context, record runtimeexec.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkEffectAdmission(ctx, tx, record.RunID); err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM invocations i JOIN runs r ON r.id=i.run_id WHERE i.state IN ('intent','launching','created','running','uncertain') AND (i.state='uncertain' OR r.status!='running')`).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return fmt.Errorf("%w: previous invocation cleanup is unresolved", process.ErrCleanup)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO invocations(id,run_id,generation,state,record,updated) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.generation=? AND r.owner=? AND r.status='running' AND e.finalizing=0)`, record.ID, record.RunID, record.Generation, record.State, data, time.Now().UTC().UnixNano(), record.RunID, record.Generation, record.Owner)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("invocation execution owner changed before launch")
	}
	return tx.Commit()
}

func (s *SQLiteStore) CheckCleanupPrerequisites(ctx context.Context) error {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM invocations i JOIN runs r ON r.id=i.run_id WHERE i.state IN ('intent','launching','created','running','uncertain') AND (i.state='uncertain' OR r.status!='running')`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: cleanup prerequisites unavailable for %d invocations", process.ErrCleanup, count)
	}
	return nil
}
func (s *SQLiteStore) UpdateInvocation(ctx context.Context, record runtimeexec.Record) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE invocations SET state=?,record=?,updated=? WHERE id=? AND run_id=? AND generation=? AND (state!='cleaned' OR ?='cleaned')", record.State, data, time.Now().UTC().UnixNano(), record.ID, record.RunID, record.Generation, record.State)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		var state string
		if err := s.db.QueryRowContext(ctx, "SELECT state FROM invocations WHERE id=? AND run_id=? AND generation=?", record.ID, record.RunID, record.Generation).Scan(&state); err == nil && state == "cleaned" {
			return nil
		}
		return fmt.Errorf("invocation was not admitted")
	}
	return nil
}
func (s *SQLiteStore) Invocations(ctx context.Context, run string, pending bool) ([]runtimeexec.Record, error) {
	return s.listInvocations(ctx, run, pending, false)
}

func (s *SQLiteStore) listInvocations(ctx context.Context, run string, pending, reconcile bool) ([]runtimeexec.Record, error) {
	query := "SELECT i.record FROM invocations i JOIN runs r ON r.id=i.run_id WHERE (i.run_id=? OR ?='')"
	if pending {
		query += " AND i.state IN ('intent','launching','created','running','uncertain')"
	}
	if reconcile && run == "" {
		query += " AND (i.state='uncertain' OR r.status!='running')"
	}
	query += " ORDER BY i.updated,i.id LIMIT 100"
	rows, err := s.db.QueryContext(ctx, query, run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []runtimeexec.Record{}
	for rows.Next() {
		var raw []byte
		var record runtimeexec.Record
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(raw, &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

// ReconcileInvocations is called with exclusive workspace ownership. It never
// kills by a persisted host PID or treats runtime failure as confirmed absence.
func (s *SQLiteStore) ReconcileInvocations(ctx context.Context, run string) error {
	ctx, cancelBatch := context.WithTimeout(ctx, 15*time.Second)
	defer cancelBatch()
	records, err := s.listInvocations(ctx, run, true, true)
	if err != nil {
		return err
	}
	var failures error
	for _, record := range records {
		if record.State == "launching" || record.State == "intent" || record.State == "created" || record.State == "running" {
			// During ordinary scheduling these may still belong to the live owner.
			var status string
			if err = s.db.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=?", record.RunID).Scan(&status); err != nil {
				return err
			}
			if status == RunStatusRunning && run == "" {
				continue
			}
		}
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		updated, reconcileErr := runtimeexec.Reconcile(probeCtx, record)
		cancel()
		if err = s.UpdateInvocation(ctx, updated); err != nil {
			return err
		}
		if reconcileErr == nil {
			current, _, loadErr := s.Load(ctx, record.RunID)
			if loadErr != nil {
				return loadErr
			}
			if current.Status == RunStatusNeedsAttention && current.FailureClass == "owner_loss" && current.Generation == record.Generation {
				requested, cancelErr := s.CancellationRequested(ctx, record.RunID)
				if cancelErr != nil {
					return cancelErr
				}
				if requested && s.CheckInvocationCleanup(ctx, record.RunID) == nil {
					current.Status = RunStatusCanceled
					current.FailureClass = "canceled"
					current.FinishedAt = time.Now().UTC()
					if err = s.Save(ctx, current); err != nil {
						return err
					}
				}
			}
		}
		failures = errors.Join(failures, reconcileErr)
	}
	return failures
}
func (s *SQLiteStore) CheckInvocationCleanup(ctx context.Context, run string) error {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM invocations WHERE state!='cleaned' AND (run_id=? OR ?='')", run, run).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: %d invocation records still require reconciliation", process.ErrCleanup, count)
	}
	return nil
}

func invocationContext(ctx context.Context, journal RunStore, attempt *Attempt) context.Context {
	if attempt == nil {
		return ctx
	}
	store := invocationStore(journal)
	if store == nil {
		return ctx
	}
	fence, ok := ctx.Value(fenceKey{}).(executionFence)
	if !ok {
		return ctx
	}
	return runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: fence.id, Owner: fence.owner, Generation: fence.generation, LogicalID: attempt.ID})
}

func invocationStore(journal RunStore) *SQLiteStore {
	switch value := journal.(type) {
	case *SQLiteStore:
		return value
	case redactedStore:
		store, _ := value.Journal.(*SQLiteStore)
		return store
	}
	return nil
}
