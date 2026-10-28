package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func syncFileAndDirectory(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
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
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func createBackup(ctx context.Context, db *sql.DB, destination string) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return syncFileAndDirectory(destination)
}

// Backup includes committed WAL content. It never exports provider credentials
// or assumes a database-only backup can restore external session/workspace data.
func (s *SQLiteStore) Backup(ctx context.Context, destination string, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("backup requires exclusive workspace ownership")
	}
	abs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if abs == filepath.Join(s.Dir, "state.db") {
		return fmt.Errorf("backup cannot overwrite the authoritative database")
	}
	return createBackup(ctx, s.db, abs)
}

func openBackupSource(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var integrity string
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("backup database integrity verification failed")
	}
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version < 1 || version > schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("backup schema %d is unsupported", version)
	}
	checks := [][]string{{"runs", "admission_requests", "cancellations", "attempts", "artifacts", "events"}, {"workers", "messages", "external_intents", "executions"}, {"tool_calls", "invocation_grants"}, {"invocations"}, {"usage_invocations", "usage_observations", "usage_session_counters"}, {"retention_groups"}, {}, {"action_attempts"}, {"iteration_plans", "iteration_attempts"}, {"input_gates", "input_controls"}}
	for index := 0; index < version; index++ {
		for _, table := range checks[index] {
			var found int
			if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&found); err != nil || found != 1 {
				_ = db.Close()
				return nil, fmt.Errorf("backup schema is incomplete")
			}
		}
	}
	if version >= 7 {
		rows, columnErr := db.QueryContext(ctx, "SELECT finalizing FROM executions LIMIT 0")
		if columnErr != nil {
			_ = db.Close()
			return nil, fmt.Errorf("backup execution schema is incomplete")
		}
		_ = rows.Close()
	}
	violations, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	invalid := violations.Next()
	checkErr := violations.Err()
	_ = violations.Close()
	if invalid || checkErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backup foreign-key integrity verification failed")
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM runs").Scan(&count); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backup is not a workflow database")
	}
	return db, nil
}

// RestoreDatabase installs a validated consistent snapshot atomically. Existing
// records are first retained in a consistent private pre-restore backup.
func RestoreDatabase(ctx context.Context, dir, source string, owner *ownership.Lock) (string, error) {
	if !owner.Holds(dir) {
		return "", fmt.Errorf("restore requires exclusive workspace ownership")
	}
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	destination, err := filepath.Abs(filepath.Join(dir, "state.db"))
	if err != nil {
		return "", err
	}
	if sourceAbs == destination {
		return "", fmt.Errorf("restore source cannot be the authoritative database")
	}
	input, err := openBackupSource(ctx, sourceAbs)
	if err != nil {
		return "", err
	}
	defer input.Close()
	temporary := filepath.Join(dir, "restore-"+newID()+".db")
	if err = createBackup(ctx, input, temporary); err != nil {
		return "", err
	}
	defer os.Remove(temporary)
	if err = quarantineRestore(ctx, temporary); err != nil {
		return "", err
	}
	retained := ""
	if _, err = os.Stat(destination); err == nil {
		current, openErr := sql.Open("sqlite", destination)
		if openErr != nil {
			return "", openErr
		}
		current.SetMaxOpenConns(1)
		var running int
		if err = current.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE status='running'").Scan(&running); err != nil {
			_ = current.Close()
			return "", err
		}
		if running > 0 {
			_ = current.Close()
			return "", fmt.Errorf("restore requires stopped execution and recovered owner-loss records")
		}
		var currentVersion int
		if err = current.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
			_ = current.Close()
			return "", err
		}
		if currentVersion >= 4 {
			var pending int
			if err = current.QueryRowContext(ctx, "SELECT count(*) FROM invocations WHERE state!='cleaned'").Scan(&pending); err != nil {
				_ = current.Close()
				return "", err
			}
			if pending > 0 {
				_ = current.Close()
				return "", fmt.Errorf("restore requires resolved invocation cleanup")
			}
		}
		retained = filepath.Join(dir, "state.pre-restore-"+newID()+".db")
		if err = createBackup(ctx, current, retained); err != nil {
			_ = current.Close()
			return "", err
		}
		// Switching to DELETE requires existing WAL readers to release their
		// snapshots. Refuse a busy database instead of replacing live WAL state.
		var mode string
		if err = current.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil || mode != "delete" {
			_ = current.Close()
			return retained, fmt.Errorf("restore requires closed database readers and writers")
		}
		if err = current.Close(); err != nil {
			return retained, err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(destination + suffix); err != nil && !os.IsNotExist(err) {
			return retained, err
		}
	}
	if err = os.Rename(temporary, destination); err != nil {
		return retained, err
	}
	return retained, syncFileAndDirectory(destination)
}

func quarantineRestore(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id,record FROM runs WHERE status!='succeeded' AND status!='skipped'")
	if err != nil {
		return err
	}
	type saved struct {
		id  string
		raw []byte
	}
	var records []saved
	for rows.Next() {
		var item saved
		if err = rows.Scan(&item.id, &item.raw); err != nil {
			_ = rows.Close()
			return err
		}
		records = append(records, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, item := range records {
		var run Run
		if err = decodeJSONWithNumbers(item.raw, &run); err != nil {
			return err
		}
		if version >= 2 {
			if err = tx.QueryRowContext(ctx, "SELECT generation FROM runs WHERE id=?", item.id).Scan(&run.Generation); err != nil {
				return err
			}
		}
		run.Status = RunStatusNeedsAttention
		run.FailureClass = "restore_uncertainty"
		run.Error = "restored database snapshot; inspect effects and workspace/session state before explicit retry acknowledgement"
		raw, err := json.Marshal(run)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE runs SET status=?,record=? WHERE id=?", run.Status, raw, item.id); err != nil {
			return err
		}
		if version >= 2 {
			if _, err = tx.ExecContext(ctx, "UPDATE runs SET owner='' WHERE id=?", item.id); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE executions SET status=?,record=? WHERE run_id=? AND generation=(SELECT generation FROM runs WHERE id=?)", run.Status, raw, item.id, item.id); err != nil {
				return err
			}
		}
	}
	// A restored mailbox may already have executed after the backup. It must
	// not automatically dispatch when the originating run is resumed.
	var hasWorkers int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workers'").Scan(&hasWorkers); err != nil {
		return err
	}
	if hasWorkers > 0 {
		rows, err = tx.QueryContext(ctx, "SELECT id,record FROM workers WHERE status!='closed'")
		if err != nil {
			return err
		}
		records = nil
		for rows.Next() {
			var item saved
			if err = rows.Scan(&item.id, &item.raw); err != nil {
				_ = rows.Close()
				return err
			}
			records = append(records, item)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, item := range records {
			var worker Worker
			if err = decodeJSONWithNumbers(item.raw, &worker); err != nil {
				return err
			}
			worker.Status = RunStatusNeedsAttention
			worker.Error = "restored session/mailbox state requires explicit recovery"
			raw, _ := json.Marshal(worker)
			if _, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", worker.Status, raw, item.id); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, "SELECT id,record FROM messages WHERE status IN ('queued','running')")
		if err != nil {
			return err
		}
		records = nil
		for rows.Next() {
			var item saved
			if err = rows.Scan(&item.id, &item.raw); err != nil {
				_ = rows.Close()
				return err
			}
			records = append(records, item)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, item := range records {
			var message WorkerMessage
			if err = decodeJSONWithNumbers(item.raw, &message); err != nil {
				return err
			}
			message.Status = "interrupted"
			message.Error = "restored mailbox entry may have executed after this snapshot; explicit new send is required"
			raw, _ := json.Marshal(message)
			if _, err = tx.ExecContext(ctx, "UPDATE messages SET status=?,record=? WHERE id=?", message.Status, raw, item.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
