package workflows

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func packageSourceFixture(inputs []AssetInput) *PackageSource {
	entries := make([]AssetEntry, 0, len(inputs))
	for _, input := range inputs {
		entry := AssetEntry{Path: input.Path, Kind: "directory", Mode: uint32(input.Mode.Perm())}
		if !input.Mode.IsDir() {
			digest := sha256.Sum256(input.Data)
			entry.Kind, entry.Size, entry.SHA256 = "file", int64(len(input.Data)), hex.EncodeToString(digest[:])
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return &PackageSource{Binding: PackageBinding{Version: 1, Name: "fixture", PackageVersion: "1.0.0", EngineVersion: "0.2.2", Skill: "task", Workflow: "workflows/main.yml", Digest: assetBundleID(entries)}, Entries: inputs}
}

func TestPackageNativeLostCompletionRetainsDefinitionAndAllowance(t *testing.T) {
	workflow := `jobs:
  task:
    steps:
      - id: task
        uses: agent/run
        with: {agent: agents, prompt: work, turns: 2}
`
	e, store, fixture := agentTaskFixture(t, "jobs: {}\n", func(SessionRequest) (map[string]any, error) {
		return map[string]any{"text": "original"}, nil
	})
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(store.Dir, "assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	definition := []byte("name: packaged\ndescription: packaged\ninstructions: Exact packaged instructions.\n")
	source := packageSourceFixture([]AssetInput{
		{Path: "workflows/main.yml", Mode: 0644, Data: []byte(workflow)},
		{Path: "workflows/agents", Mode: fs.ModeDir | 0755},
		{Path: "workflows/agents/reviewer.yaml", Mode: 0644, Data: definition},
	})
	if _, err := store.db.Exec(`CREATE TRIGGER lose_package_completion BEFORE UPDATE OF status ON attempts WHEN OLD.step_id='task' AND NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'lost package completion'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: source.Binding.Workflow, Package: source})
	if err == nil || run == nil {
		t.Fatal("completion fault not exercised", run, err)
	}
	// No package installation exists to reread, and the target has a different
	// definition. Mutating caller-owned captured input must not change admission.
	definition[0] = '!'
	if _, err := store.db.Exec("DROP TRIGGER lose_package_completion"); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded {
		t.Fatal(resumed, err)
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil || len(workers) != 1 || workers[0].TaskTurns != 2 || workers[0].Remaining != 1 {
		t.Fatal("recovery changed original task allowance", workers, err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.requests) != 1 || fixture.requests[0].Definition.DeveloperInstructions != "Exact packaged instructions." || fixture.requests[0].Definition.Source != "package:workflows/agents/reviewer.yaml" {
		t.Fatal("recovery changed captured task or replayed provider work", fixture.requests)
	}
	if e.DefinitionRoot != "" || e.packageBinding != nil || e.packageFiles != nil {
		t.Fatal("package context escaped private executor")
	}
}

func retainedPackageSnapshot(t *testing.T, store *SQLiteStore) (*Run, Snapshot) {
	t.Helper()
	run := &Run{Version: 1, ID: randomID(), WorkflowRef: "workflows/main.yml", Status: RunStatusQueued}
	run.OriginID = run.ID
	bundle, err := store.CaptureAssetBundle(t.Context(), run.ID, retainedAssetInputs())
	if err != nil {
		t.Fatal(err)
	}
	binding := &PackageBinding{Version: 1, Name: "fixture", PackageVersion: "1.0.0", EngineVersion: "0.2.2", Skill: "task", Workflow: run.WorkflowRef, Digest: bundle.ID, Assets: bundle}
	snap := Snapshot{Version: 1, Package: binding, Metadata: map[string]any{"origin_run_id": run.ID}, RequestID: "package-fixture"}
	if err := store.Admit(t.Context(), run, snap); err != nil {
		t.Fatal(err)
	}
	return run, snap
}

func TestPackageAssetsRetainChildReferencesAndCleanupProtection(t *testing.T) {
	store := retainedAssetStore(t)
	run, snap := retainedPackageSnapshot(t, store)
	child := &Run{Version: 1, ID: randomID(), OriginID: run.ID, WorkflowRef: run.WorkflowRef, Status: RunStatusQueued}
	snap.RequestID = ""
	if err := store.Admit(t.Context(), child, snap); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PackageAssets(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := store.CleanupHelperStaging(t.Context(), lock); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PackageAssets(t.Context(), run.ID); err != nil {
		t.Fatal("cleanup removed committed package", err)
	}
	if _, err := store.db.Exec("DELETE FROM run_helper_assets WHERE run_id=?", child.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PackageAssets(t.Context(), run.ID); err == nil || !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("missing child reference ignored: %v", err)
	}
}

func TestPackageDatabaseBackupRequiresExternalAssetsAndRecoveryBundleIncludesThem(t *testing.T) {
	store := retainedAssetStore(t)
	run, snap := retainedPackageSnapshot(t, store)
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := store.Backup(t.Context(), backup, lock); err != nil {
		t.Fatal(err)
	}
	restored := retainedAssetStore(t)
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored.Dir, "state.db"), data, 0600); err != nil {
		t.Fatal(err)
	}
	inspection, err := OpenSQLiteInspection(restored.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	if _, _, err := inspection.PackageAssets(t.Context(), run.ID); err == nil {
		t.Fatal("database-only backup claimed to contain package bytes")
	}
	bundlePath := filepath.Join(t.TempDir(), "recovery.tar.gz")
	if err := store.RecoveryBundle(t.Context(), bundlePath, lock); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	dirs := map[string]fs.FileMode{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(header.Name, "assets/") {
			continue
		}
		name := filepath.Join(restored.Dir, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(name, 0755); err != nil {
				t.Fatal(err)
			}
			dirs[name] = fs.FileMode(header.Mode)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, fs.FileMode(header.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	for name, mode := range dirs {
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	got, root, err := inspection.PackageAssets(t.Context(), run.ID)
	if err != nil || !SamePackage(got, snap.Package) {
		t.Fatalf("recovery package: %+v, %v", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "files")); err != nil || string(data) != "a source file named files" {
		t.Fatalf("restored bytes: %q, %v", data, err)
	}
}

func TestPackageAdmissionRejectsForgedDigestAndEscapingAgent(t *testing.T) {
	store := retainedAssetStore(t)
	source := &PackageSource{Binding: PackageBinding{Version: 1, Name: "fixture", PackageVersion: "1.0.0", EngineVersion: "0.2.2", Skill: "task", Workflow: "workflows/main.yml", Digest: strings.Repeat("a", 64)}, Entries: []AssetInput{{Path: "workflows/main.yml", Mode: 0644, Data: []byte("jobs: {task: {steps: [{argv: [echo, hello]}]}}\n")}}}
	e := &Executor{WorkspaceDir: t.TempDir(), Store: store}
	if _, err := e.Prepare(t.Context(), RunRequest{Ref: source.Binding.Workflow, Package: source}); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("accepted forged source: %v", err)
	}
	copy := *e
	copy.DefinitionRoot = t.TempDir()
	copy.packageFiles = map[string]AssetInput{"agents/a.yaml": {Path: "agents/a.yaml", Mode: 0644, Data: []byte("name: a\ndescription: a\ninstructions: a\n")}}
	if _, err := copy.capturedAgent(filepath.Join(copy.DefinitionRoot, "../outside.yaml")); err == nil {
		t.Fatal("escaped captured agent root")
	}
	definition, err := copy.capturedAgent(filepath.Join(copy.DefinitionRoot, "agents/a.yaml"))
	if err != nil || definition.Source != "package:agents/a.yaml" {
		t.Fatalf("logical agent source: %+v, %v", definition, err)
	}
	encoded, _ := json.Marshal(source.Binding)
	if strings.Contains(string(encoded), store.Dir) {
		t.Fatal("package identity contains installation/storage path")
	}
}
