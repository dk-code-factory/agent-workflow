package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func rootHelperCaptureFixture(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	put(t, source, "main.yml", "jobs: {task: {container: {image: ubuntu:24.04, files: [.]}, steps: [{run: echo ready}]}}\n")
	put(t, source, "helpers/run.sh", "#!/bin/sh\necho retained\n")
	if err := os.Chmod(source, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "helpers/run.sh"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "helpers/empty"), 0710); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "installed")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "main.yml")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0700) })
	return root, source
}

func TestCaptureImportedRootHelpersPreserveMaterializedDirectory(t *testing.T) {
	root, source := rootHelperCaptureFixture(t)
	captured, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]PackageEntry{}
	for _, entry := range captured.Manifest().Entries {
		if entry.Path == "." {
			t.Fatal("structural root entered payload manifest")
		}
		entries[entry.Path] = entry
	}
	for _, name := range []string{DescriptorFile, "skills/demo/SKILL.md", "imports/demo/workflows", "imports/demo/workflows/main.yml", "imports/demo/workflows/helpers", "imports/demo/workflows/helpers/run.sh", "imports/demo/workflows/helpers/empty"} {
		entry, exists := entries[name]
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if !exists || err != nil || entry.Mode != uint32(info.Mode().Perm()) {
			t.Fatalf("child metadata missing: %s %+v %v", name, entry, err)
		}
	}
	if entries["imports/demo/workflows/helpers/empty"].Kind != "directory" || entries["imports/demo/workflows/helpers/empty"].Size != 0 {
		t.Fatal("empty descendant lost")
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	readonly, err := Capture(root)
	if err != nil || readonly.Digest() != captured.Digest() {
		t.Fatal("transport root permissions changed payload identity", err)
	}
	// Import uses the same collector, but its rebased source root is now a
	// materialized directory whose original mode must not be discarded.
	_, imported, err := importWorkflow("rebased", filepath.Join(source, "main.yml"))
	if err != nil {
		t.Fatal(err)
	}
	name := "imports/rebased/workflows"
	info, err := os.Stat(source)
	entry, exists := imported[name]
	if err != nil || !exists || !entry.mode.IsDir() || entry.mode.Perm() != info.Mode().Perm() {
		t.Fatalf("rebased root lost its directory metadata: %s %+v %v", name, entry, err)
	}
	if _, exists := imported["."]; exists {
		t.Fatal("import did not rebase its root into a materialized directory")
	}
}
