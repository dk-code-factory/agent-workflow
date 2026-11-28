package workflows

import (
	"context"
	"database/sql"
	"sync"
)

type invocationStatement int

const (
	admitInvocationStatement invocationStatement = iota
	updateInvocationStatement
)

var invocationQueries = [...]string{
	`INSERT INTO invocations(id,run_id,generation,state,record,updated) SELECT ?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.generation=? AND r.owner=? AND r.status='running' AND e.finalizing=0)`,
	`UPDATE invocations SET state=?,record=?,updated=? WHERE id=? AND run_id=? AND generation=? AND (state!='cleaned' OR ?='cleaned')`,
}

// Cache only these two fixed statements, never their parameters or results.
// database/sql binds a prepared statement to the transaction's connection and
// closes its native statements with the owning DB. Every execution still reads
// current ownership and finalization state inside the original transaction.
type invocationStatementCache struct {
	mu         sync.Mutex
	statements [len(invocationQueries)]*sql.Stmt
}

func (s *SQLiteStore) invocationStatement(ctx context.Context, which invocationStatement) (*sql.Stmt, error) {
	cache := s.invocationSQL
	cache.mu.Lock()
	if statement := cache.statements[which]; statement != nil {
		cache.mu.Unlock()
		return statement, nil
	}
	cache.mu.Unlock()
	// Preparing may wait for this store's single connection. Do not hold the
	// cache lock while waiting: another canceled caller must return promptly.
	statement, err := s.db.PrepareContext(ctx, invocationQueries[which])
	if err != nil {
		// Cancellation or an unavailable historical schema must not poison a
		// future call after its context or schema has been repaired.
		return nil, err
	}
	cache.mu.Lock()
	if existing := cache.statements[which]; existing != nil {
		cache.mu.Unlock()
		_ = statement.Close()
		return existing, nil
	}
	cache.statements[which] = statement
	cache.mu.Unlock()
	return statement, nil
}
