package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestReadExternalInputAnswer(t *testing.T) {
	for _, raw := range []string{`null`, ``, `true false`, `{"a":`, strings.Repeat(" ", (1<<20)+1)} {
		if _, err := readAnswer("-", strings.NewReader(raw)); err == nil {
			t.Fatal("invalid answer accepted")
		}
	}
	for _, raw := range []string{`false`, `0`, `""`, `[]`, `{"policy":"approve"}`} {
		if out, err := readAnswer("-", strings.NewReader(raw)); err != nil || string(out) != raw {
			t.Fatal(string(out), err)
		}
	}
	path := filepath.Join(t.TempDir(), "answer.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := readAnswer(path, nil); err != nil || string(out) != `{"ok":true}` {
		t.Fatal(string(out), err)
	}
}
func TestForegroundInputWaitReturnsHandleAndDistinctStatus(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source := `jobs: {decide: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: evidence}}]}}`
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	err := command(context.Background(), []string{"run", "--foreground", "--workspace", root, "workflows/main.yml"}, &out, &stderr)
	if !errors.Is(err, errInputWaiting) || !strings.Contains(out.String(), `"status": "waiting"`) || !strings.Contains(out.String(), `"gate_id"`) {
		t.Fatal(out.String(), stderr.String(), err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := store.List(context.Background())
	store.Close()
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	out.Reset()
	if err = command(context.Background(), []string{"inputs", "--workspace", root, runs[0].ID}, &out, &stderr); err != nil || !strings.Contains(out.String(), `"prompt": "q"`) {
		t.Fatal(out.String(), err)
	}
}
