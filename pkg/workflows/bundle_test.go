package workflows

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkropachev/agent-workflow/internal/ownership"
)

func TestRecoveryBundleExcludesNativeCredentialsAndOtherOwnership(t *testing.T) {
	store, run := executionStore(t)
	owner, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	provider := filepath.Join(store.Dir, "providers", "codex", run.ID, "session")
	if err = os.MkdirAll(provider, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(provider, "auth.json"), []byte("PRIVATE_NATIVE_CREDENTIAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(provider, "session.jsonl"), []byte("owned session data"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "private-auth")
	if err = os.WriteFile(foreign, []byte("FOREIGN_SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(foreign, filepath.Join(provider, "native-reference")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "recovery.tar.gz")
	if err = store.RecoveryBundle(context.Background(), destination, owner); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	entries := map[string]bool{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = true
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "PRIVATE_NATIVE_CREDENTIAL") || strings.Contains(string(data), "FOREIGN_SECRET") || strings.HasSuffix(header.Name, "auth.json") || strings.HasSuffix(header.Name, "native-reference") {
			t.Fatal("native credential data/reference exported", header.Name)
		}
	}
	if !entries["manifest.json"] || !entries["state.db"] || !entries[filepath.ToSlash(filepath.Join("providers", "codex", run.ID, "session", "session.jsonl"))] {
		t.Fatal("engine-owned recovery state omitted", entries)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("bundle is not private", err)
	}
}
