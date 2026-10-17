package workflows

import (
	"context"
	"database/sql"
	"fmt"
)

type executionFence struct {
	id, owner  string
	generation int64
}
type fenceKey struct{}

func fenceContext(ctx context.Context, run *Run) context.Context {
	return context.WithValue(ctx, fenceKey{}, executionFence{run.ID, run.Owner, run.Generation})
}

// checkFence rejects stale effects and checkpoint writes, including writes to
// reusable children owned by a superseded originating execution.
func checkFence(ctx context.Context, tx *sql.Tx) error {
	f, ok := ctx.Value(fenceKey{}).(executionFence)
	if !ok {
		return nil
	} // Compatibility library calls without an execution owner.
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE id=? AND generation=? AND owner=? AND status='running'", f.id, f.generation, f.owner).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("execution ownership changed for run %s", f.id)
	}
	return nil
}
