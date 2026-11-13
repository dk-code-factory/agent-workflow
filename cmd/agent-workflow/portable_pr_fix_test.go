package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStockCLIRunsPortableWorkflowsWithoutPRFixRegistration(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "old.yml"), []byte("jobs: {capture: {steps: [{uses: action/prfix_cycle, with: {plan: old}}]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := command(context.Background(), []string{"pr-fix", "prepare"}, &out, &stderr); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("legacy PR-specific CLI unexpectedly remains available: %v", err)
	}
	err := command(context.Background(), []string{"run", "--foreground", "--workspace", root, "workflows/old.yml"}, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "missing registered Go action") {
		t.Fatalf("stock engine unexpectedly registers PR-specific actions: %v", err)
	}
}
