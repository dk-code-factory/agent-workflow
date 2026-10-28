package workflows

import (
	"context"
	"database/sql"
	"errors"
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

// checkEffectAdmission fences only new effects, never checkpoint/completion or
// cleanup persistence. Resolve physical children to their durable root control.
func checkEffectAdmission(ctx context.Context, tx *sql.Tx, run string) error {
	origin := run
	err := tx.QueryRowContext(ctx, "SELECT origin_id FROM runs WHERE id=?", run).Scan(&origin)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if origin == "" {
		origin = run
	}
	control, err := inputControlTx(ctx, tx, origin)
	if err != nil {
		return err
	}
	var requested int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM cancellations WHERE run_id=?", origin).Scan(&requested); err != nil {
		return err
	}
	if control.Canceled || requested > 0 {
		return ErrExternalInputCanceled
	}
	return nil
}

// Complete external-input journals expose the same root control to embedded
// executors. This pre-dispatch check complements transactional store admission.
func (e *Executor) checkEffectAdmission(ctx context.Context, run string) error {
	if fence, ok := ctx.Value(fenceKey{}).(executionFence); ok {
		run = fence.id
	}
	journal := externalInputJournal(e.Store)
	if journal == nil || run == "" {
		return nil
	}
	control, err := journal.InputControl(ctx, run)
	if err != nil {
		return err
	}
	if control.Canceled {
		return ErrExternalInputCanceled
	}
	return nil
}
