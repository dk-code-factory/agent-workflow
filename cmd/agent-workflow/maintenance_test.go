package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkropachev/agent-workflow/internal/ownership"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func TestDatabaseMaintenanceCLIRequiresOwnershipAndPreservesBackup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-workflow")
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := &workflows.Run{ID: strings.Repeat("a", 32), Status: workflows.RunStatusQueued}
	if err = store.Admit(context.Background(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	backup := filepath.Join(t.TempDir(), "state.db")
	lock, err := ownership.Own(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	if err = command(context.Background(), []string{"backup", "--workspace", root, backup}, &out, &diagnostic); err == nil {
		t.Fatal("backup bypassed execution owner")
	}
	_ = lock.Close()
	out.Reset()
	if err = command(context.Background(), []string{"backup", "--workspace", root, backup}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "database-only") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err = command(context.Background(), []string{"restore", "--workspace", root, backup}, &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(backup); err != nil {
		t.Fatal("restore consumed the source backup", err)
	}
	view, err := workflows.OpenSQLiteInspection(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	result, _, err := view.Load(context.Background(), run.ID)
	if err != nil || result.FailureClass != "restore_uncertainty" {
		t.Fatal(result, err)
	}
}
