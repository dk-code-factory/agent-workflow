package workflows

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolverResolveLocalCanonicalWorkflowRef(t *testing.T) {
	workspace := t.TempDir()
	createWorkflowFile(t, workspace, "summarize-text.yml")

	got, err := (Resolver{WorkspaceDir: workspace}).ResolveLocal("workflows/summarize-text.yml")
	if err != nil {
		t.Fatalf("ResolveLocal failed: %v", err)
	}
	if got.Canonical != "workflows/summarize-text.yml" {
		t.Fatalf("Canonical = %q, want workflows/summarize-text.yml", got.Canonical)
	}
	wantPath := filepath.Join(workspace, "workflows", "summarize-text.yml")
	if got.Path != wantPath {
		t.Fatalf("Path = %q, want %q", got.Path, wantPath)
	}
}

func TestResolverAcceptsDotSlashButCanonicalizes(t *testing.T) {
	workspace := t.TempDir()
	createWorkflowFile(t, workspace, "summarize-text.yml")

	got, err := (Resolver{WorkspaceDir: workspace}).ResolveLocal("./workflows/summarize-text.yml")
	if err != nil {
		t.Fatalf("ResolveLocal failed: %v", err)
	}
	if got.Canonical != "workflows/summarize-text.yml" {
		t.Fatalf("Canonical = %q, want workflows/summarize-text.yml", got.Canonical)
	}
}

func TestResolverRejectsUnsafeWorkflowRefs(t *testing.T) {
	workspace := t.TempDir()
	tests := []string{
		"",
		"/tmp/workflows/a.yml",
		"../workflows/a.yml",
		"workflows/../secret.yml",
		"not-workflows/a.yml",
		"workflows/a.txt",
	}
	for _, ref := range tests {
		t.Run(ref, func(t *testing.T) {
			if _, err := (Resolver{WorkspaceDir: workspace}).ResolveLocal(ref); err == nil {
				t.Fatalf("ResolveLocal(%q) succeeded, want error", ref)
			}
		})
	}
}

func TestIsLocalWorkflowRef(t *testing.T) {
	if !IsLocalWorkflowRef("workflows/a.yml") {
		t.Fatal("expected workflows/a.yml to be a local workflow ref")
	}
	if IsLocalWorkflowRef("tool/message") {
		t.Fatal("expected tool/message not to be a local workflow ref")
	}
}

func TestResolverRejectsSymlinkEscape(t *testing.T) {
	if testing.Short() {
		t.Skip("symlink test skipped in short mode")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "target.yml")
	if err := os.WriteFile(target, []byte("name: outside\njobs: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "workflows", "escape.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := (Resolver{WorkspaceDir: workspace}).ResolveLocal("workflows/escape.yml")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("ResolveLocal symlink escape error = %v, want escape error", err)
	}
}

func TestResolverRejectsSymlinkedDirectoryEscape(t *testing.T) {
	if testing.Short() {
		t.Skip("symlink test skipped in short mode")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(outside, "outside.yml"),
		[]byte("name: outside\njobs: {}\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "workflows", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := (Resolver{WorkspaceDir: workspace}).ResolveLocal("workflows/link/outside.yml")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("ResolveLocal symlinked directory escape error = %v, want escape error", err)
	}
}

func TestResolverRejectsDefinitionsRootSymlinkEscape(t *testing.T) {
	if testing.Short() {
		t.Skip("symlink test skipped in short mode")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "automation")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := (Resolver{
		WorkspaceDir:   workspace,
		DefinitionsDir: "automation",
	}).ResolveLocal("workflows/outside.yml")
	if err == nil || !strings.Contains(err.Error(), "root escapes workspace") {
		t.Fatalf(
			"ResolveLocal definitions-root symlink error = %v, want workspace escape",
			err,
		)
	}
}

func createWorkflowFile(t *testing.T, workspace, name string) {
	t.Helper()
	dir := filepath.Join(workspace, "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, name),
		[]byte("name: test\njobs:\n  noop:\n    runs-on: picoclaw\n    steps:\n      - uses: tool/message\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
}
