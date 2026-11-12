package workflows

import (
	"context"
	"errors"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

func (m *WorkerManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *WorkerManager) eligible(ctx context.Context) ([]Worker, error) {
	rows, err := m.Store.db.QueryContext(ctx, `SELECT w.record FROM workers w
WHERE w.status='idle' AND w.run_id IN (SELECT r.id FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.status='running' AND e.finalizing=0)
AND EXISTS(SELECT 1 FROM messages WHERE worker_id=w.id AND status='queued')
AND NOT EXISTS(SELECT 1 FROM messages WHERE worker_id=w.id AND status='running')
ORDER BY w.rowid LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		var b []byte
		var w Worker
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// StopRun closes admission first, then joins every active turn. A terminal run
// is not published until these provider invocations have stopped.
func (m *WorkerManager) StopRun(ctx context.Context, run string) error {
	err := m.CancelRun(ctx, run)
	m.mu.Lock()
	var stopped []chan struct{}
	for id, origin := range m.activeRuns {
		if origin == run {
			m.active[id]()
			stopped = append(stopped, m.done[id])
		}
	}
	m.mu.Unlock()
	for _, done := range stopped {
		select {
		case <-done:
		case <-ctx.Done():
			return errors.Join(err, process.ErrCleanup, ctx.Err())
		}
	}
	// Closed workers retain their message outcome, including cleanup uncertainty.
	var uncertain int
	queryErr := m.Store.db.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN workers w ON w.id=m.worker_id JOIN runs r ON r.id=w.run_id
WHERE w.run_id=? AND COALESCE(json_extract(CAST(m.record AS TEXT),'$.execution_generation'),1)=r.generation
AND (m.status='running' OR json_extract(CAST(m.record AS TEXT),'$.failure_class')='needs_attention'
OR (m.status='failed' AND NOT COALESCE(
json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.execution_generation')=r.generation
AND json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.message_id')=m.id
AND length(CAST(json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.request_id') AS BLOB))>0,0)))`, run).Scan(&uncertain)
	if queryErr != nil {
		return errors.Join(err, process.ErrCleanup, queryErr)
	}
	if uncertain != 0 {
		return errors.Join(err, process.ErrCleanup, fmt.Errorf("worker cleanup remains uncertain"))
	}
	queryErr = m.Store.db.QueryRowContext(ctx, `SELECT count(*) FROM workers w JOIN runs r ON r.id=w.run_id
WHERE w.run_id=? AND COALESCE(json_extract(CAST(w.record AS TEXT),'$.failure_generation'),1)=r.generation
AND COALESCE(json_extract(CAST(w.record AS TEXT),'$.failure_class'),'') NOT IN ('','canceled')
AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.worker_id=w.id AND m.status='failed'
AND m.id=json_extract(CAST(w.record AS TEXT),'$.failure_acknowledgement.message_id')
AND json_extract(CAST(w.record AS TEXT),'$.failure_acknowledgement.execution_generation')=r.generation
AND json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.execution_generation')=r.generation
AND json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.message_id')=m.id
AND json_extract(CAST(m.record AS TEXT),'$.failure_acknowledgement.request_id')=json_extract(CAST(w.record AS TEXT),'$.failure_acknowledgement.request_id'))`, run).Scan(&uncertain)
	if queryErr != nil {
		return errors.Join(err, process.ErrCleanup, queryErr)
	}
	if uncertain != 0 {
		return errors.Join(err, process.ErrCleanup, fmt.Errorf("worker failure requires attention"))
	}
	if err != nil {
		return errors.Join(err, process.ErrCleanup)
	}
	m.mu.Lock()
	delete(m.taskSecrets, run)
	m.mu.Unlock()

	return nil
}
