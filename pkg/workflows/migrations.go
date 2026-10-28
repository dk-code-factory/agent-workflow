package workflows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

const schemaVersion = 10

const schemaV1 = `
CREATE TABLE runs(id TEXT PRIMARY KEY,status TEXT NOT NULL,record BLOB NOT NULL,snapshot BLOB NOT NULL,updated TEXT NOT NULL);
CREATE TABLE admission_requests(request_id TEXT PRIMARY KEY,run_id TEXT NOT NULL UNIQUE REFERENCES runs(id) ON DELETE CASCADE);
CREATE TABLE cancellations(run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,requested_at TEXT NOT NULL);
CREATE TABLE attempts(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,job_id TEXT NOT NULL,step_id TEXT NOT NULL,iteration INTEGER NOT NULL,number INTEGER NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL,UNIQUE(run_id,job_id,step_id,iteration,number));
CREATE TABLE artifacts(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,attempt_id TEXT NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,name TEXT NOT NULL,sha256 TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE events(sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,time TEXT NOT NULL,type TEXT NOT NULL,data BLOB);
`

const schemaV2 = `
CREATE TABLE IF NOT EXISTS workers(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,parent_id TEXT NOT NULL,role TEXT NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,worker_id TEXT NOT NULL REFERENCES workers(id) ON DELETE CASCADE,request_id TEXT NOT NULL,sequence INTEGER NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL,UNIQUE(worker_id,request_id),UNIQUE(worker_id,sequence));
CREATE TABLE IF NOT EXISTS external_intents(key TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,kind TEXT NOT NULL,record BLOB NOT NULL);
ALTER TABLE runs ADD COLUMN origin_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE runs ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN admitted INTEGER NOT NULL DEFAULT 0;
UPDATE runs SET origin_id=COALESCE(NULLIF(json_extract(CAST(snapshot AS TEXT),'$.metadata.origin_run_id'),''),id),admitted=rowid;
CREATE TABLE executions(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,generation INTEGER NOT NULL,request_id TEXT,retry_uncertain INTEGER NOT NULL,admitted INTEGER NOT NULL,cancellation_requested INTEGER NOT NULL DEFAULT 0,status TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(run_id,generation),UNIQUE(run_id,request_id));
INSERT INTO executions(run_id,generation,retry_uncertain,admitted,cancellation_requested,status,record) SELECT id,1,0,admitted,EXISTS(SELECT 1 FROM cancellations WHERE run_id=runs.id),status,record FROM runs;
CREATE INDEX runs_queue ON runs(status,admitted,id);
CREATE INDEX runs_origin ON runs(origin_id,id);
CREATE INDEX workers_eligible ON workers(status,run_id,id);
CREATE INDEX workers_run ON workers(run_id);
CREATE INDEX messages_eligible ON messages(worker_id,status,sequence);
CREATE INDEX attempts_run ON attempts(run_id);
CREATE INDEX events_run ON events(run_id,sequence);
CREATE INDEX artifacts_run ON artifacts(run_id);
CREATE INDEX intents_run ON external_intents(run_id);
`

const schemaV3 = `
CREATE TABLE tool_calls(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,generation INTEGER NOT NULL,invocation_id TEXT NOT NULL,call_id TEXT NOT NULL,server TEXT NOT NULL,tool TEXT NOT NULL,fingerprint TEXT NOT NULL,status TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(run_id,invocation_id,call_id));
CREATE INDEX tool_calls_unresolved ON tool_calls(status,run_id);
CREATE TABLE invocation_grants(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,invocation_id TEXT NOT NULL,remaining INTEGER NOT NULL,PRIMARY KEY(run_id,invocation_id));
`

const schemaV4 = `
CREATE TABLE invocations(id TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,generation INTEGER NOT NULL,state TEXT NOT NULL,record BLOB NOT NULL,updated INTEGER NOT NULL);
CREATE INDEX invocations_cleanup ON invocations(state,updated,run_id);
CREATE INDEX invocations_run ON invocations(run_id);
`

const schemaV5 = `
CREATE TABLE usage_invocations(id TEXT PRIMARY KEY,origin_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,record BLOB NOT NULL);
CREATE INDEX usage_origin ON usage_invocations(origin_id);
CREATE TABLE usage_observations(invocation_id TEXT NOT NULL REFERENCES usage_invocations(id) ON DELETE CASCADE,observation_key TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(invocation_id,observation_key));
CREATE TABLE usage_session_counters(origin_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,scope_key TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(origin_id,scope_key));
`

const schemaV6 = `CREATE TABLE retention_groups(origin_id TEXT PRIMARY KEY,state TEXT NOT NULL,record BLOB NOT NULL);`
const schemaV7 = `ALTER TABLE executions ADD COLUMN finalizing INTEGER NOT NULL DEFAULT 0;`

const schemaV8 = `CREATE TABLE action_attempts(attempt_id TEXT PRIMARY KEY REFERENCES attempts(id) ON DELETE CASCADE,revision INTEGER NOT NULL,record BLOB NOT NULL);`

const schemaV9 = `
CREATE TABLE iteration_plans(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,job_id TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(run_id,job_id));
CREATE TABLE iteration_attempts(run_id TEXT NOT NULL,job_id TEXT NOT NULL,item_key TEXT NOT NULL,number INTEGER NOT NULL,phase TEXT NOT NULL,record BLOB NOT NULL,PRIMARY KEY(run_id,job_id,item_key,number,phase),FOREIGN KEY(run_id,job_id) REFERENCES iteration_plans(run_id,job_id) ON DELETE CASCADE);
`

const schemaV10 = `
CREATE TABLE input_controls(run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,paused INTEGER NOT NULL DEFAULT 0,canceled INTEGER NOT NULL DEFAULT 0,continuation INTEGER NOT NULL DEFAULT 0);
CREATE TABLE input_gates(id TEXT PRIMARY KEY,origin_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,state TEXT NOT NULL,answer_id TEXT UNIQUE,record BLOB NOT NULL);
CREATE INDEX input_gates_origin ON input_gates(origin_id,state);
` + schemaV10InputIndexes

const schemaV10InputIndexes = `
CREATE INDEX IF NOT EXISTS input_controls_continuations ON input_controls(run_id) WHERE continuation=1 AND paused=0 AND canceled=0;
CREATE INDEX IF NOT EXISTS input_gates_run ON input_gates(run_id);
`

// Migrate must be called while holding exclusive workspace ownership. Existing
// snapshots and attempt history are never rewritten. The consistent pre-upgrade
// backup is deliberately retained for operator verification and rollback.
func (s *SQLiteStore) Migrate(ctx context.Context, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("schema migration requires exclusive ownership of this workspace")
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("unsupported state schema version %d", version)
	}
	backupName := fmt.Sprintf("state.pre-v%d.db", schemaVersion)
	if version == schemaVersion {
		var indexed int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name IN ('input_controls_continuations','input_gates_run')").Scan(&indexed); err != nil {
			return err
		}
		if indexed == 2 {
			return nil
		}
		// Same-version repair remains explicit and owned. Preserve each pre-repair
		// state, including after restoring an earlier v10 backup without these indexes.
		backupName = "state.pre-v10-input-continuations.db"
		if _, err := os.Stat(filepath.Join(s.Dir, backupName)); err == nil {
			backupName = "state.pre-v10-input-continuations-" + newID() + ".db"
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if version > 0 {
		backup := filepath.Join(s.Dir, backupName)
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			// VACUUM INTO includes committed WAL content, unlike copying state.db.
			tmp := backup + ".partial"
			if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
				return err
			}
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
				return err
			}
			f, err = os.OpenFile(tmp, os.O_RDWR, 0600)
			if err != nil {
				return err
			}
			err = f.Sync()
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if err := os.Rename(tmp, backup); err != nil {
				return err
			}
			directory, err := os.Open(s.Dir)
			if err != nil {
				return err
			}
			err = directory.Sync()
			_ = directory.Close()
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == schemaVersion {
		if _, err = tx.ExecContext(ctx, schemaV10InputIndexes); err != nil {
			return err
		}
		return tx.Commit()
	}
	migrations := []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6, schemaV7, schemaV8, schemaV9, schemaV10}
	for next := version + 1; next <= schemaVersion; next++ {
		if _, err = tx.ExecContext(ctx, migrations[next-1]); err != nil {
			return fmt.Errorf("schema migration %d: %w", next, err)
		}
		if next == 5 {
			if err = backfillUsage(ctx, tx); err != nil {
				return fmt.Errorf("usage history migration: %w", err)
			}
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", next)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func originID(r *Run, snap Snapshot) string {
	if id := nativeString(snap.Metadata, "origin_run_id"); id != "" {
		return id
	}
	return r.ID
}
