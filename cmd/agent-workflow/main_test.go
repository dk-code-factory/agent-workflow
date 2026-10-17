package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateCommandChecksWorkflowAndAgentBindings(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents.yml"), []byte("agents:\n  claude:\n    provider: claude-cli\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workflows", "main.yml")
	if err := os.WriteFile(path, []byte("jobs:\n  review:\n    steps:\n      - uses: agent/claude\n        with: {prompt: hello}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/main.yml"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Valid: workflows/main.yml\n" {
		t.Fatal(stdout.String())
	}
	if err := os.WriteFile(path, []byte("jobs:\n  review:\n    steps:\n      - uses: agent/missing\n        with: {prompt: hello}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/main.yml"}, &stdout, &stderr); err == nil {
		t.Fatal("accepted missing agent binding")
	}
}

func TestInitScaffoldsBoundDefinitionsAndPreservesExistingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "workflows", "classify.yml")
	if err := os.WriteFile(existing, []byte("user definition\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := command(context.Background(), []string{"init", "--workspace", root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "user definition\n" {
		t.Fatal("existing definition overwritten")
	}
	stdout.Reset()
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/develop.yml"}, &stdout, &stderr); err != nil {
		t.Fatal("scaffold has invalid agent bindings", err)
	}
	config := filepath.Join(root, "agents.yml")
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = command(context.Background(), []string{"init", "--workspace", root}, &stdout, &stderr); err == nil {
		t.Fatal("existing configuration overwritten")
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("configuration changed on duplicate init")
	}
}
