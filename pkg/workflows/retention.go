package workflows

import (
	"context"
	"fmt"
)

// Input history belongs to its originating execution even when a physical
// child is deleted directly. The same predicate also fences each DELETE, so a
// preceding inspection cannot authorize deletion after owner/control changes.
const inputRetentionProtectionQuery = `SELECT 1 FROM runs member JOIN runs origin ON origin.id=member.origin_id
 WHERE member.id=? AND EXISTS(SELECT 1 FROM input_gates g WHERE g.origin_id=origin.id)
 AND (origin.status IN ('running','queued','needs_attention','waiting','interrupted')
 OR (NOT EXISTS(SELECT 1 FROM input_controls c WHERE c.run_id=origin.id AND c.canceled=1)
 AND NOT EXISTS(SELECT 1 FROM cancellations WHERE run_id=origin.id)
 AND (EXISTS(SELECT 1 FROM input_gates g WHERE g.origin_id=origin.id AND g.state IN ('pending','accepted'))
 OR (origin.status='canceled' AND json_extract(CAST(origin.record AS TEXT),'$.failure_class')='canceled'))))`

const retentionDeleteRun = `DELETE FROM runs WHERE id=? AND status NOT IN ('running','queued','needs_attention','waiting','interrupted') AND NOT EXISTS(` + inputRetentionProtectionQuery + `)`

func (s *SQLiteStore) checkInputRetention(ctx context.Context, id string) error {
	var protected bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS("+inputRetentionProtectionQuery+")", id).Scan(&protected); err != nil {
		return err
	}
	if protected {
		return fmt.Errorf("run %s retains recoverable external input; cancel before retention", id)
	}
	return nil
}

// OriginRuns returns the complete reusable closure's records for one owner.
func (s *SQLiteStore) OriginRuns(ctx context.Context, id string) ([]Run, error) {
	if err := s.CheckInvocationCleanup(ctx, id); err != nil {
		return nil, err
	}
	run, snap, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	origin := nativeString(snap.Metadata, "origin_run_id")
	if origin == "" {
		origin = run.ID
	}
	if origin != id {
		return nil, fmt.Errorf("cleanup originating run %s", origin)
	}
	if err = s.checkInputRetention(ctx, id); err != nil {
		return nil, err
	}
	runs, err := s.listRecords(ctx, "SELECT record,generation,owner FROM runs WHERE origin_id=? OR id=? ORDER BY rowid", id, id)
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for _, candidate := range runs {
		if candidate.Status == RunStatusRunning || candidate.Status == RunStatusQueued || candidate.Status == RunStatusNeedsAttention || candidate.Status == RunStatusWaiting || candidate.Status == RunStatusInterrupted {
			return nil, fmt.Errorf("run %s still requires execution or recovery", candidate.ID)
		}
		out = append(out, candidate)
	}
	return out, nil
}
func (s *SQLiteStore) DeleteOrigin(ctx context.Context, id string) error {
	runs, err := s.OriginRuns(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, run := range runs {
		res, err := tx.ExecContext(ctx, retentionDeleteRun, run.ID, run.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("run changed during retention cleanup")
		}
	}
	return tx.Commit()
}
