package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestTaskStateCommandInspectsAdmittedGroupOffline(t *testing.T) {
	root := t.TempDir()
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	run := &workflows.Run{ID: strings.Repeat("a", 32), Status: workflows.RunStatusQueued}
	err = store.Admit(context.Background(), run, workflows.Snapshot{Metadata: map[string]any{"task_group": "inspection-group"}})
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("admit inspection fixture: %v, close: %v", err, closeErr)
	}
	var stdout, stderr bytes.Buffer
	if err := command(context.Background(), []string{"task-state", "--workspace", root, run.ID}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var inspection workflows.TaskStateInspection
	if err := json.Unmarshal(stdout.Bytes(), &inspection); err != nil || inspection.Group != "inspection-group" {
		t.Fatalf("incorrect task-state inspection: %s, %v", stdout.Bytes(), err)
	}
}

func TestTaskGroupFlagDoesNotReplaceResumeIdentity(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := command(context.Background(), []string{"resume", "--task-group", "replacement", "original-run"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "resume retains its admitted group") {
		t.Fatalf("resume accepted a replacement task group: %v", err)
	}
}

func TestTaskGroupFlagRejectsInvalidAdmission(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, group := range []string{"../escape", strings.Repeat("x", 129), "space group"} {
		err := command(context.Background(), []string{"run", "--task-group", group, "workflows/main.yml"}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "task group") {
			t.Fatalf("invalid group reached controller admission: %q: %v", group, err)
		}
	}
}
