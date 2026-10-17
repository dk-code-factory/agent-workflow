package workflows

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func executionStore(t *testing.T) (*SQLiteStore, *Run) {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &Run{ID: randomID(), Status: RunStatusFailed, Version: 1, Jobs: map[string]JobExecution{}}
	if err = s.Admit(context.Background(), r, Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestResumeGenerationsAreAtomicAndDeduplicated(t *testing.T) {
	s, initial := executionStore(t)
	ctx := context.Background()
	if err := s.RequestCancel(ctx, initial.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var admitted [8]*Run
	var failures [8]error
	for i := range admitted {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admitted[i], failures[i] = s.QueueResume(ctx, initial.ID, "same", true, 128)
		}(i)
	}
	wg.Wait()
	for i, run := range admitted {
		if failures[i] != nil || run.Generation != 2 || run.Status != RunStatusQueued {
			t.Fatalf("admission %d: %+v %v", i, run, failures[i])
		}
	}
	if _, err := s.QueueResume(ctx, initial.ID, "same", false, 128); err == nil {
		t.Fatal("conflicting acknowledgement accepted")
	}
	if _, err := s.QueueResume(ctx, initial.ID, "competitor", true, 128); err == nil {
		t.Fatal("competing resume accepted")
	}
	if canceled, err := s.CancellationRequested(ctx, initial.ID); err != nil || canceled {
		t.Fatalf("old cancellation leaked: %v %v", canceled, err)
	}
	var priorCanceled bool
	if err := s.db.QueryRow("SELECT cancellation_requested FROM executions WHERE run_id=? AND generation=1", initial.ID).Scan(&priorCanceled); err != nil || !priorCanceled {
		t.Fatalf("old cancellation erased: %v %v", priorCanceled, err)
	}
	if err := s.Save(ctx, initial); err == nil {
		t.Fatal("superseded generation overwrote admitted resume")
	}
	current, _, err := s.Load(ctx, initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Status = RunStatusRunning
	if err = s.Claim(ctx, current, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	stale := *current
	stale.Owner = "wrong-owner"
	if err = s.Save(ctx, &stale); err == nil {
		t.Fatal("stale owner write accepted")
	}
	staleContext := fenceContext(ctx, &stale)
	if _, _, err = s.Begin(staleContext, initial.ID, "job", "step", 1, true); err == nil {
		t.Fatal("stale owner admitted an effect")
	}
	current.Status = RunStatusFailed
	if err = s.Save(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err = s.QueueResume(ctx, initial.ID, "next", false, 128); err != nil {
		t.Fatal(err)
	}
	if err = s.RequestCancelGeneration(ctx, initial.ID, 2); err != ErrStaleExecution {
		t.Fatalf("stale cancellation accepted: %v", err)
	}
	duplicate, err := s.QueueResume(ctx, initial.ID, "same", true, 128)
	if err != nil || duplicate.Generation != 2 {
		t.Fatalf("retry identity changed across generations: %+v %v", duplicate, err)
	}
}

func TestRecoveryRetainsClaimedOwnerAndRejectsStaleCompletion(t *testing.T) {
	s, run := executionStore(t)
	ctx := context.Background()
	queued, err := s.QueueResume(ctx, run.ID, "owner", false, 128)
	if err != nil {
		t.Fatal(err)
	}
	queued.Status = RunStatusRunning
	if err = s.Claim(ctx, queued, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	staleContext := fenceContext(ctx, queued)
	a, _, err := s.Begin(staleContext, run.ID, "job", "step", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.Load(ctx, run.ID)
	if err != nil || current.Status != RunStatusNeedsAttention {
		t.Fatalf("claimed owner recovery failed: %+v %v", current, err)
	}
	if err = s.Complete(staleContext, a, StepExecution{Status: RunStatusSucceeded}); err == nil {
		t.Fatal("lost owner completed effect")
	}
	queued.Status = RunStatusSucceeded
	if err = s.Save(ctx, queued); err == nil {
		t.Fatal("recovered owner overwrote terminal run state")
	}
}

func TestInspectionPaginationAndReadOnlyOpen(t *testing.T) {
	s, _ := executionStore(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		run := &Run{ID: randomID(), Status: RunStatusSucceeded, Jobs: map[string]JobExecution{}}
		if err := s.Admit(ctx, run, Snapshot{Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	view, err := OpenSQLiteInspection(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := view.ListPage(ctx, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 2 {
			t.Fatal("unbounded inspection")
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate continuation item")
			}
			seen[item.ID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 7 {
		t.Fatalf("missing records: %d", len(seen))
	}
	if _, err = view.ListPage(ctx, 1001, ""); err == nil {
		t.Fatal("oversized page accepted")
	}
	if _, err = view.ListPage(ctx, 2, nextCursor("wrong-scope", 1)); err == nil {
		t.Fatal("wrong scope cursor accepted")
	}
	if _, err = view.db.Exec("DELETE FROM runs"); err == nil {
		t.Fatal("inspection opened writable storage")
	}
}

func TestQueuedResumeSurvivesReopenAndRecovery(t *testing.T) {
	s, run := executionStore(t)
	ctx := context.Background()
	if _, err := s.QueueResume(ctx, run.ID, "durable", true, 128); err != nil {
		t.Fatal(err)
	}
	other, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = other.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	queued, err := other.Queued(ctx)
	if err != nil || len(queued) != 1 || queued[0].Generation != 2 {
		t.Fatalf("lost accepted resume: %+v %v", queued, err)
	}
	if retry, err := other.ExecutionRetry(ctx, run.ID, 2); err != nil || !retry {
		t.Fatalf("lost acknowledgement: %v %v", retry, err)
	}
}

func TestMigrationPreservesSnapshotAndConsistentBackup(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV1 + "PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	record := `{"id":"original","status":"failed","version":1}`
	snapshot := `{"version":1,"metadata":{"origin_run_id":"original"},"roles":{"immutable":true}}`
	if _, err = db.Exec("INSERT INTO runs VALUES('original','failed',?,?,'old')", record, snapshot); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("inspection implicitly migrated", version, err)
	}

	if err = s.Migrate(context.Background(), nil); err == nil {
		t.Fatal("migration without ownership accepted")
	}
	lock, err := ownership.Own(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = s.Migrate(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	var saved, origin string
	if err = s.db.QueryRow("SELECT snapshot,origin_id FROM runs WHERE id='original'").Scan(&saved, &origin); err != nil || saved != snapshot || origin != "original" {
		t.Fatalf("migration changed snapshot: %s %s %v", saved, origin, err)
	}
	backup := filepath.Join(dir, fmt.Sprintf("state.pre-v%d.db", schemaVersion))
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("missing private backup", err)
	}
	old, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err = old.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatal("backup is not pre-migration state", version, err)
	}
	if err = old.QueryRow("SELECT snapshot FROM runs WHERE id='original'").Scan(&saved); err != nil || saved != snapshot {
		t.Fatal("backup lost records", err)
	}
	if err = s.Migrate(context.Background(), lock); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}

func TestFailedMigrationRollsBackAndRejectsNewerSchemas(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schemaV1 + "PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO runs VALUES('broken','failed','{}','malformed','old')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lock, err := ownership.Own(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = s.Migrate(context.Background(), lock); err == nil {
		t.Fatal("malformed migration fixture accepted")
	}
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("failed migration advanced schema: %d %v", version, err)
	}
	if _, err = s.db.Exec("SELECT generation FROM runs"); err == nil {
		t.Fatal("failed migration retained partial schema")
	}
	if _, err = s.db.Exec("UPDATE runs SET snapshot='{\"version\":1}'"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(context.Background(), lock); err != nil {
		t.Fatal("failed migration could not be retried", err)
	}
	if _, err = s.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenSQLite(dir); err == nil {
		_ = other.Close()
		t.Fatal("newer schema accepted")
	}
	if other, err := OpenSQLiteInspection(dir); err == nil {
		_ = other.Close()
		t.Fatal("newer schema inspected as compatible")
	}
}

func TestQueuedQueryIsBoundedAndIgnoresCorruptTerminalHistory(t *testing.T) {
	s, run := executionStore(t)
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		if _, err := s.db.Exec("INSERT INTO runs(id,status,record,snapshot,updated) VALUES(?,'succeeded','invalid-json','{}','old')", randomID()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.QueueResume(ctx, run.ID, "bounded", false, 128); err != nil {
		t.Fatal(err)
	}
	queued, err := s.Queued(ctx)
	if err != nil || len(queued) != 1 {
		t.Fatalf("decoded terminal history: %+v %v", queued, err)
	}
}
