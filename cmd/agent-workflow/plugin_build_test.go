package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func TestPluginBuildCLI(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := plugins.Init(source, "example", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	for _, args := range [][]string{
		{"plugin", "build"},
		{"plugin", "build", "--target", "codex", source},
		{"plugin", "build", "--output", filepath.Join(root, "missing-target"), source},
		{"plugin", "build", "--target", "codex", "--output", filepath.Join(root, "flag-order"), source, "--target", "claude"},
	} {
		if err := command(t.Context(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted invalid build: %v", args)
		}
	}
	var out bytes.Buffer
	if err := command(t.Context(), []string{"plugin", "build", "--target", "codex,claude", "--output", filepath.Join(root, "output"), source}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result plugins.BuildResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Name != "example" || result.PackageDigest == "" {
		t.Fatalf("invalid completion report %s: %v", out.Bytes(), err)
	}
}
