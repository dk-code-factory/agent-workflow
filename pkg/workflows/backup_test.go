package workflows

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func TestBackupSourceRejectsMissingFinalizationBarrier(t *testing.T) {
	store, _ := executionStore(t)
	ctx := context.Background()
	owner, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	path := filepath.Join(t.TempDir(), "incomplete.db")
	if err = store.Backup(ctx, path, owner); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("ALTER TABLE executions DROP COLUMN finalizing"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if source, err := openBackupSource(ctx, path); err == nil {
		_ = source.Close()
		t.Fatal("schema-7 backup accepted without its finalization barrier")
	}
}

func TestConsistentBackupRestoreQuarantinesRunnableSnapshots(t *testing.T) {
	store, run := executionStore(t)
	ctx := context.Background()
	run.Status = RunStatusQueued
	if err := store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	owner, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err = store.Backup(ctx, backup, nil); err == nil {
		t.Fatal("backup without ownership accepted")
	}
	if err = store.Backup(ctx, backup, owner); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup is not private", err)
	}
	run.Status = RunStatusSucceeded
	if err = store.Save(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := RestoreDatabase(ctx, store.Dir, backup, owner)
	if err != nil {
		t.Fatal(err)
	}
	if retained == "" {
		t.Fatal("pre-restore state was discarded")
	}
	restored, err := OpenSQLite(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	record, _, err := restored.Load(ctx, run.ID)
	if err != nil || record.Status != RunStatusNeedsAttention || record.FailureClass != "restore_uncertainty" {
		t.Fatalf("restored queued work was replayable: %+v %v", record, err)
	}
	if _, err = restored.QueueResume(ctx, run.ID, "", false, 128); err == nil {
		t.Fatal("restored effects resumed without acknowledgement")
	}
	old, err := openBackupSource(ctx, retained)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var status string
	if err = old.QueryRowContext(ctx, "SELECT status FROM runs WHERE id=?", run.ID).Scan(&status); err != nil || status != RunStatusSucceeded {
		t.Fatal("retained pre-restore snapshot was inconsistent", status, err)
	}
}

func TestRestoreRefusesUnsupportedSourceBeforeReplacingState(t *testing.T) {
	store, run := executionStore(t)
	ctx := context.Background()
	owner, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	invalid := filepath.Join(t.TempDir(), "invalid.db")
	if err = os.WriteFile(invalid, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = RestoreDatabase(ctx, store.Dir, invalid, owner); err == nil {
		t.Fatal("invalid source restored")
	}
	if _, _, err = store.Load(ctx, run.ID); err != nil {
		t.Fatal("invalid restore changed authoritative state", err)
	}
}
