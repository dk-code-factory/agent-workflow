package workflows

import (
	"context"
	"fmt"
)

// OriginRuns returns the complete reusable closure's records for one owner.
func (s *SQLiteStore) OriginRuns(ctx context.Context, id string) ([]Run, error) {
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
	runs, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := []Run{}
	for _, candidate := range runs {
		_, snapshot, err := s.Load(ctx, candidate.ID)
		if err != nil {
			return nil, err
		}
		owner := nativeString(snapshot.Metadata, "origin_run_id")
		if candidate.ID == id || owner == id {
			if candidate.Status == RunStatusRunning || candidate.Status == RunStatusQueued || candidate.Status == RunStatusNeedsAttention {
				return nil, fmt.Errorf("run %s still requires execution or recovery", candidate.ID)
			}
			out = append(out, candidate)
		}
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
		res, err := tx.ExecContext(ctx, "DELETE FROM runs WHERE id=? AND status NOT IN ('running','queued','needs_attention')", run.ID)
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
