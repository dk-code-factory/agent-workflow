package prfix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func terminalEngine(ctx context.Context, root, engine string) error {
	if engine == "" {
		return fmt.Errorf("reconcile original admission response before replacement")
	}
	store, e := workflows.OpenSQLiteInspection(filepath.Join(root, ".agent-workflow"))
	if e != nil {
		return e
	}
	defer store.Close()
	run, _, e := store.Load(ctx, engine)
	if e != nil {
		return e
	}
	switch run.Status {
	case workflows.RunStatusSucceeded, workflows.RunStatusFailed, workflows.RunStatusCanceled, workflows.RunStatusNeedsAttention:
	default:
		return fmt.Errorf("execution %s is still %s; settle it before replacement", engine, run.Status)
	}
	if e = store.CheckInvocationCleanup(ctx, engine); e != nil {
		return e
	}
	return store.CheckToolEffects(ctx, engine)
}

// Replacement reconciles observations only. It never retries a native turn or
// dispatches a Git/GitHub effect. Unfinished unchanged work uses engine resume.
func reconcileReplacement(ctx context.Context, l *Ledger, provider Provider, old Plan) error {
	_, engine, e := l.LatestExecution(ctx, old.LogicalRun)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = terminalEngine(ctx, l.Root, engine); e != nil {
		return e
	}
	store, e := workflows.OpenSQLiteInspection(filepath.Join(l.Root, ".agent-workflow"))
	if e != nil {
		return e
	}
	defer store.Close()
	runtime := &Runtime{Root: l.Root, Provider: provider, Store: store, Workers: &workflows.WorkerManager{Store: store}}
	if e = runtime.verifyReviewRequestDispositions(ctx, old); e != nil {
		return e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT id FROM effects WHERE run=? AND state IN ('dispatched','unknown') ORDER BY rowid", old.LogicalRun)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		intent, e := l.Intent(ctx, old.LogicalRun, id)
		if e != nil {
			return e
		}
		state, receipt, e := runtime.reconcileIntent(ctx, intent)
		if e != nil {
			return e
		}
		switch state {
		case "completed":
			if _, e = runtime.completeEffect(ctx, l, intent, receipt); e != nil {
				return e
			}
		default:
			return fmt.Errorf("operation %s is still uncertain; replacement cannot dispatch another effect", id)
		}
	}
	rows, e = l.db.QueryContext(ctx, "SELECT id FROM claims WHERE run=? AND state IN ('claimed','recheck','recover_required') ORDER BY rowid", old.LogicalRun)
	if e != nil {
		return e
	}
	ids = nil
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		c, e := getClaim(ctx, l.db, old.LogicalRun, id)
		if e != nil {
			return e
		}
		for round := 0; round <= c.Checks; round++ {
			m, e := runtime.message(ctx, c, round)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			if m.Status == workflows.RunStatusQueued || m.Status == workflows.RunStatusRunning {
				return fmt.Errorf("native turn %s has not settled", m.ID)
			}
			if m.Status != workflows.RunStatusSucceeded {
				continue
			}
			if _, e = parseReport(m); e != nil {
				continue
			}
			if _, e = l.db.ExecContext(ctx, "INSERT INTO submissions VALUES(?,?,?,?,?) ON CONFLICT(run,claim,round) DO NOTHING", old.LogicalRun, c.ID, round, m.ID, "stale"); e != nil {
				return e
			}
			if round == 0 {
				if _, e = l.db.ExecContext(ctx, "UPDATE claims SET report_message=? WHERE run=? AND id=? AND report_message=''", m.ID, old.LogicalRun, c.ID); e != nil {
					return e
				}
			}
		}
		if _, e = l.db.ExecContext(ctx, "UPDATE claims SET state='stale',reason='pinned inputs changed after original execution settled' WHERE run=? AND id=?", old.LogicalRun, c.ID); e != nil {
			return e
		}
	}
	_, e = l.db.ExecContext(ctx, "UPDATE effects SET state='superseded' WHERE run=? AND state='prepared'", old.LogicalRun)
	return e
}
