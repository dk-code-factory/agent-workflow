package workflows

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func putHelper(t *testing.T, base, name, data string, mode os.FileMode) {
	t.Helper()
	file := filepath.Join(base, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}
func TestHelperBundleCaptureBoundsPathsAndIntegrity(t *testing.T) {
	_, store, _ := containerExecutor(t)
	base := t.TempDir()
	origin := randomID()
	putHelper(t, base, "helpers/main.py", "import common\n", 0755)
	putHelper(t, base, "helpers/common/lib.py", "VALUE = 1\n", 0644)
	b, err := store.CaptureHelperBundle(t.Context(), origin, base, []string{"helpers/", "./helpers/main.py"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != 2 || b.Files[0].Mode != 0444 || b.Files[1].Mode != 0555 {
		t.Fatal(b)
	}
	path, err := store.HelperBundlePath(b)
	if err != nil {
		t.Fatal(err)
	}
	putHelper(t, base, "helpers/main.py", "changed source\n", 0755)
	if err = store.VerifyHelperBundle(b); err != nil {
		t.Fatal("source edits changed retained bytes", err)
	}
	for _, name := range []string{"../escape", "/absolute", "helpers/../../escape", "helpers\\main.py", "${{ inputs.path }}"} {
		if _, err = store.CaptureHelperBundle(t.Context(), origin, base, []string{name}); err == nil {
			t.Fatal("accepted", name)
		}
	}
	if err = os.Symlink("common", filepath.Join(base, "helpers/link")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CaptureHelperBundle(t.Context(), origin, base, []string{"helpers/"}); err == nil {
		t.Fatal("accepted symlink")
	}
	if err = os.Remove(filepath.Join(base, "helpers/link")); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(base, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(MaxHelperBytes + 1); err != nil {
		t.Fatal(err)
	}
	large.Close()
	if _, err = store.CaptureHelperBundle(t.Context(), origin, base, []string{"large"}); err == nil {
		t.Fatal("accepted oversized file")
	}
	file := filepath.Join(path, "helpers/main.py")
	if err = os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if err = store.VerifyHelperBundle(b); err == nil {
		t.Fatal("changed retained mode accepted")
	}
	if err = os.WriteFile(file, bytes.Repeat([]byte("x"), len("import common\n")), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(file, 0555); err != nil {
		t.Fatal(err)
	}
	if err = store.VerifyHelperBundle(b); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatal("changed retained content did not reach checksum verification", err)
	}

}
func TestHelperBundleFileCountAndStorageSymlinks(t *testing.T) {
	_, store, _ := containerExecutor(t)
	base := t.TempDir()
	for i := 0; i <= MaxHelperFiles; i++ {
		putHelper(t, base, strings.Repeat("x", i/1000)+newID(), "", 0644)
	}
	if _, err := store.CaptureHelperBundle(t.Context(), randomID(), base, []string{"."}); err == nil {
		t.Fatal("accepted too many files")
	}
	other := t.TempDir()
	if err := os.RemoveAll(filepath.Join(store.Dir, "assets")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(store.Dir, "assets")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CaptureHelperBundle(t.Context(), randomID(), base, []string{}); err == nil {
		t.Fatal("accepted asset storage symlink")
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatal("wrote through storage symlink")
	}
}
func helperBundleWorkflow() string {
	return strings.Replace(structuredWorkflow, "container: ubuntu:24.04", "container: {image: 'ubuntu:24.04', files: [helpers/]}", 1)
}
func TestRetainedHelpersSurviveSourceDeletionAndRejectCorruption(t *testing.T) {
	for _, condition := range []string{"retained", "dangling-source", "outside-source", "corrupted", "missing"} {
		t.Run(condition, func(t *testing.T) {
			e, store, d := containerExecutor(t)
			f := &protocolFixture{binding: "stable"}
			putHelper(t, e.WorkspaceDir, "workflows/helpers/main.py", "admitted helper", 0755)
			installProtocolWorkflow(t, e, d, f, helperBundleWorkflow())
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err != nil {
				t.Fatal(err)
			}
			_, snap, err := store.Load(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			b := snap.Definitions[run.WorkflowRef].Jobs["capture"].Container.Implementation.Bundle
			if b == nil || b.OriginID != run.ID {
				t.Fatal(b)
			}
			// Deleting both YAML and imported helpers must not affect retained admission.
			if err = os.RemoveAll(filepath.Join(e.WorkspaceDir, "workflows")); err != nil {
				t.Fatal(err)
			}
			if condition == "dangling-source" || condition == "outside-source" {
				target := filepath.Join(t.TempDir(), "changed.yml")
				if condition == "outside-source" {
					putHelper(t, filepath.Dir(target), filepath.Base(target), "not the admitted workflow", 0644)
				}
				if err = os.MkdirAll(filepath.Join(e.WorkspaceDir, "workflows"), 0755); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, filepath.Join(e.WorkspaceDir, "workflows/test.yml")); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "corrupted" {
				p, err := store.HelperBundlePath(b)
				if err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(p, "helpers/main.py")
				if err = os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(file, []byte("tampered helper"), 0644); err != nil {
					t.Fatal(err)
				}
				if err = os.Chmod(file, 0555); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "missing" {
				path, _ := store.bundleDirectory(b)
				if err = os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = e.Resume(t.Context(), run.ID, nil, false)
			if condition == "corrupted" || condition == "missing" {
				if err == nil {
					t.Fatal("reused corrupted completed checkpoint")
				}
				if f.validations != 0 {
					t.Fatal("started validation before asset verification")
				}
				if condition == "corrupted" && !strings.Contains(err.Error(), "checksum mismatch") {
					t.Fatal("corrupted resume did not reach checksum verification", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if f.execute != 1 {
				t.Fatal("reexecuted completed helper")
			}
		})
	}
}
func TestHelperBundlesSiblingInheritanceAndImmutableReferences(t *testing.T) {
	e, store, d := containerExecutor(t)
	putHelper(t, e.WorkspaceDir, "workflows/helpers/x", "x", 0644)
	putHelper(t, e.WorkspaceDir, "workflows/alternate/y", "y", 0644)
	source := `jobs:
  build:
    container: {image: ubuntu, files: [helpers/]}
    steps:
      - uses: docker://ubuntu
      - uses: docker://ubuntu
        files: [alternate/]
      - uses: docker://ubuntu
        files: []
`
	installProtocolWorkflow(t, e, d, &protocolFixture{}, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := store.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	j := snap.Definitions[run.WorkflowRef].Jobs["build"]
	b := j.Container.Implementation.Bundle
	if j.Steps[0].ContainerImplementation.Bundle.ID != b.ID || j.Steps[1].ContainerImplementation.Bundle.ID == b.ID || len(j.Steps[2].ContainerImplementation.Bundle.Files) != 0 {
		t.Fatal("replacement/inheritance failed", j)
	}
	var n int
	store.db.QueryRow("SELECT count(*) FROM run_helper_assets WHERE run_id=?", run.ID).Scan(&n)
	if n != 3 {
		t.Fatal(n)
	}
	if _, err = store.db.Exec("UPDATE helper_assets SET manifest='{}' WHERE origin_id=?", run.ID); err == nil {
		t.Fatal("manifest update accepted")
	}
	// Another originating run keeps its own copy despite identical contents.
	second, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, secondSnap, _ := store.Load(t.Context(), second.ID)
	secondBundle := secondSnap.Definitions[run.WorkflowRef].Jobs["build"].Container.Implementation.Bundle
	if secondBundle.OriginID == b.OriginID || secondBundle.ID != b.ID {
		t.Fatal("cross-run ownership was deduplicated")
	}
}
func TestHelperAssetRecoveryRemovesOnlyMarkedAbandonedStorage(t *testing.T) {
	for _, missing := range []string{"", "input_controls_continuations", "input_gates_run", "input_controls_continuations input_gates_run"} {
		t.Run("missing-indexes="+missing, func(t *testing.T) {
			e, store, d := containerExecutor(t)
			putHelper(t, e.WorkspaceDir, "workflows/helpers/x", "x", 0644)
			installProtocolWorkflow(t, e, d, &protocolFixture{}, helperBundleWorkflow())
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err != nil {
				t.Fatal(err)
			}
			_, snap, _ := store.Load(t.Context(), run.ID)
			committed := snap.Definitions[run.WorkflowRef].Jobs["capture"].Container.Implementation.Bundle
			orphan, err := store.CaptureHelperBundle(t.Context(), randomID(), filepath.Join(e.WorkspaceDir, "workflows"), []string{"helpers"})
			if err != nil {
				t.Fatal(err)
			}
			orphanDir, _ := store.bundleDirectory(orphan)
			stageID := randomID()
			stage := filepath.Join(store.Dir, "assets/staging", stageID)
			marker, _ := json.Marshal(assetStageOwner{OriginID: randomID(), StageID: stageID})
			putHelper(t, stage, "owner.json", string(marker), 0400)
			unknown := filepath.Join(store.Dir, "assets/staging/unknown")
			putHelper(t, unknown, "keep", "unowned", 0600)
			for _, index := range strings.Fields(missing) {
				if _, err = store.db.Exec("DROP INDEX " + index); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := ownership.Own(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = store.Migrate(t.Context(), lock); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{orphanDir, stage} {
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("abandoned stage retained", path, err)
				}
			}
			if err = store.VerifyHelperBundle(committed); err != nil {
				t.Fatal("committed assets removed", err)
			}
			if _, err = os.Stat(filepath.Join(unknown, "keep")); err != nil {
				t.Fatal("unowned files removed", err)
			}
		})
	}
}
func TestHelperAssetsPortableRecoveryBundle(t *testing.T) {
	e, store, d := containerExecutor(t)
	putHelper(t, e.WorkspaceDir, "workflows/helpers/x", "portable", 0755)
	installProtocolWorkflow(t, e, d, &protocolFixture{}, helperBundleWorkflow())
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, _ := store.Load(t.Context(), run.ID)
	b := snap.Definitions[run.WorkflowRef].Jobs["capture"].Container.Implementation.Bundle
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	bundle := filepath.Join(t.TempDir(), "recovery.tar.gz")
	if err = store.RecoveryBundle(t.Context(), bundle, lock); err != nil {
		t.Fatal(err)
	}
	restoredDir := t.TempDir()
	file, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "staging") {
			t.Fatal("staging exported")
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		putHelper(t, restoredDir, h.Name, string(data), os.FileMode(h.Mode))
	}
	owner, err := ownership.Own(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// RestoreDatabase supplies quarantine and schema validation, just like the CLI.
	saved := filepath.Join(restoredDir, "state.db")
	database := filepath.Join(t.TempDir(), "state.db")
	raw, _ := os.ReadFile(saved)
	os.WriteFile(database, raw, 0600)
	os.Remove(saved)
	if _, err = RestoreDatabase(t.Context(), restoredDir, database, owner); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenSQLite(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.verifyRunHelperAssets(t.Context(), run.ID, snap); err != nil {
		t.Fatal(err)
	}
	p, err := restored.HelperBundlePath(b)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(p, "helpers/x"))
	if string(got) != "portable" {
		t.Fatal(string(got))
	}
	// Database-only backups deliberately cannot supply portable helper content.
	missing := *restored
	missing.Dir = t.TempDir()
	if err = missing.VerifyHelperBundle(b); err == nil {
		t.Fatal("database-only backup pretended to contain assets")
	}
}
func TestHelperAssetMigrationKeepsPreUpgradeBackup(t *testing.T) {
	_, store, _ := containerExecutor(t)
	if _, err := store.db.Exec(dropTaskStateFixtureSchema + "DROP TABLE run_helper_assets; DROP TABLE helper_assets; PRAGMA user_version=10"); err != nil {
		t.Fatal(err)
	}
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = store.Migrate(t.Context(), lock); err != nil {
		t.Fatal(err)
	}
	backup, err := openBackupSource(t.Context(), filepath.Join(store.Dir, fmt.Sprintf("state.pre-v%d.db", schemaVersion)))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var version int
	backup.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 10 {
		t.Fatal(version)
	}
}
func TestBundledNestedCaptureExampleUsesRetainedPythonImports(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("GIT_TEMPLATE_DIR", t.TempDir())
	e, store, d := containerExecutor(t)
	sourceDir := filepath.Join("..", "..", "examples", "container-workflows", "workflows")
	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, _ := filepath.Rel(sourceDir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		putHelper(t, e.WorkspaceDir, filepath.Join("workflows", relative), string(data), 0644)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-qm", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.WorkspaceDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	main := filepath.Join(e.WorkspaceDir, "workflows/main.yml")
	data, _ := os.ReadFile(main)
	os.WriteFile(main, bytes.ReplaceAll(data, []byte("repository: /workspace"), []byte("repository: "+e.WorkspaceDir)), 0600)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Jobs["inspect"].Outputs["head"].(string)) != 40 {
		t.Fatal(run)
	}
	if err = os.RemoveAll(filepath.Join(e.WorkspaceDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	_, snap, _ := store.Load(t.Context(), run.ID)
	if len(helperBundles(snap.Definitions)) != 1 {
		t.Fatal("nested helper references lost")
	}
	var child string
	if err = store.db.QueryRowContext(t.Context(), `SELECT id FROM runs WHERE origin_id=? AND json_extract(CAST(record AS TEXT), '$.workflow_ref')='workflows/capture.yml'`, run.ID).Scan(&child); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.db.ExecContext(t.Context(), "DELETE FROM run_helper_assets WHERE run_id=?", child)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := deleted.RowsAffected(); err != nil || n != 1 {
		t.Fatal("did not remove the deepest child's asset reference", n, err)
	}
	var rootReferences int
	if err = store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM run_helper_assets WHERE run_id=?", run.ID).Scan(&rootReferences); err != nil || rootReferences != 1 {
		t.Fatal("origin reference changed", rootReferences, err)
	}
	d.mu.Lock()
	executions := d.executions
	d.mu.Unlock()
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil || !strings.Contains(err.Error(), "reference missing for run "+child) {
		t.Fatal("missing child reference did not block completed checkpoint reuse", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.executions != executions {
		t.Fatal("started validation before checking child references")
	}
}

func TestHelperGuardianRelocationAndTagChangesKeepAdmittedIdentity(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "stable"}
	putHelper(t, e.WorkspaceDir, "workflows/helpers/x", "retained", 0644)
	installProtocolWorkflow(t, e, d, f, helperBundleWorkflow())
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, _ := store.Load(t.Context(), run.ID)
	replacement := filepath.Join(t.TempDir(), "guardians")
	if err = os.Rename(e.ContainerGuardianDirectory, replacement); err != nil {
		t.Fatal(err)
	}
	e.ContainerGuardianDirectory = replacement
	installProtocolWorkflow(t, e, d, f, strings.ReplaceAll(helperBundleWorkflow(), "ubuntu:24.04", "different-tag:now"))
	d.mu.Lock()
	d.imageReferences = nil
	d.mu.Unlock()
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ref := range d.imageReferences {
		if !strings.Contains(ref, "sha256:") {
			t.Fatal("re-resolved an authoring tag", ref)
		}
	}
	if len(d.imageReferences) == 0 {
		t.Fatal("retained image was not checked")
	}
	if err = store.verifyRunHelperAssets(t.Context(), run.ID, snap); err != nil {
		t.Fatal(err)
	}
}
func TestBundledMutationExampleCheckpointEvidence(t *testing.T) {
	e, _, d := containerExecutor(t)
	e.CommandExecution = runtimeexec.Policy{Mode: "container", Runtime: "docker", Image: "python-git:local", Writable: true}
	root := filepath.Join("..", "..", "examples", "container-workflows", "workflows")
	for _, name := range []string{"helpers/simulate_mutation.py", "helpers/common/protocol.py"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		putHelper(t, e.WorkspaceDir, filepath.Join("workflows", name), string(raw), 0644)
	}
	receipts := filepath.Join(e.WorkspaceDir, "receipts")
	if err := os.Mkdir(receipts, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(root, "mutate.yml"))
	if err != nil {
		t.Fatal(err)
	}
	installProtocolWorkflow(t, e, d, &protocolFixture{}, strings.ReplaceAll(string(source), "/workspace/receipts", receipts))
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(receipts)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing durable receipt", entries, err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(receipts, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil {
		t.Fatal("missing application evidence was reused")
	}
	entries, err = os.ReadDir(receipts)
	if err != nil || len(entries) != 0 {
		t.Fatal("checkpoint validation mutated application state")
	}
}
func TestSharedContainerTransportFailureBlocksFollowingSibling(t *testing.T) {
	e, _, d := containerExecutor(t)
	d.execAttachError = true
	source := `jobs: {build: {container: ubuntu, steps: [{argv: [echo, first], continue-on-error: true}, {uses: 'docker://ubuntu'}]}}`
	installProtocolWorkflow(t, e, d, &protocolFixture{}, source)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"}); err == nil || !strings.Contains(err.Error(), "received 502") || !strings.Contains(err.Error(), "shared container command lifetime is unconfirmed") {
		t.Fatal("attachment failure did not poison the shared scope", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != 1 || d.executions != 1 || len(d.containers) != 0 {
		t.Fatal("sibling ran or parent leaked after attachment failure", d.creates, d.executions, len(d.containers))
	}
}

func TestSharedContainerCompletedHighExitAllowsFollowingSibling(t *testing.T) {
	e, _, d := containerExecutor(t)
	source := `jobs: {build: {container: ubuntu, steps: [{argv: [sh, -c, 'exit 200'], continue-on-error: true}, {uses: 'docker://ubuntu'}]}}`
	installProtocolWorkflow(t, e, d, &protocolFixture{}, source)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"}); err != nil {
		t.Fatal("completed workload exit blocked continuation", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != 2 || d.executions != 1 || len(d.containers) != 0 {
		t.Fatal("unexpected continued container lifecycle", d.creates, d.executions, len(d.containers))
	}
}

func TestBundledStructuredLifecycleChildrenRejectedBeforeExecution(t *testing.T) {
	e, _, d := containerExecutor(t)
	f := &protocolFixture{binding: "stable"}
	putHelper(t, e.WorkspaceDir, "workflows/helpers/helper", "fixture", 0755)
	source := `jobs:
  build:
    container: {image: ubuntu, files: [helpers/]}
    steps:
      - id: task
        argv: [echo, ordinary]
        finally:
          - id: capture
            argv: [helper]
            protocol: action-v1
            effect: read_only
            input-schema: {type: object}
            output-schema: {type: object, properties: {head: {type: string}}, required: [head]}
`
	installProtocolWorkflow(t, e, d, f, source)
	if _, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"}); err == nil || !strings.Contains(err.Error(), "structured helpers cannot appear inside ordinary lifecycle handlers") {
		t.Fatal("invalid bundled handler was not rejected", err)
	}
	if f.execute != 0 {
		t.Fatal("invalid bundled handler executed", f.execute)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creates != 0 {
		t.Fatal("invalid bundled handler started a container", d.creates)
	}
}
func TestContainerWorkingDirectoryAppliesToEveryStructuredPhase(t *testing.T) {
	e, store, d := containerExecutor(t)
	if err := os.Mkdir(filepath.Join(e.WorkspaceDir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	source := `jobs:
  work:
    container: ubuntu
    steps:
      - argv:
          - python3
          - -c
          - |
            import json, os, pathlib, sys
            request = json.load(sys.stdin)
            phase = request["phase"]
            if os.path.basename(os.getcwd()) != "subdir":
                sys.exit("wrong working directory in " + phase)
            response = {"protocol": "action-v1", "phase": phase}
            result = {"output": {"cwd": "subdir"}, "evidence": {"cwd": "subdir"}}
            receipt = pathlib.Path("receipt")
            if phase == "bindings":
                response.update(capabilities=["bindings", "execute", "validate_checkpoint", "reconcile"],
                    read_only_phases=["bindings", "validate_checkpoint", "reconcile"], bindings={"cwd": "subdir"})
            elif phase == "execute":
                with receipt.open("x") as file:
                    file.write("completed")
                sys.exit("lost response after simulated mutation")
            elif phase == "reconcile":
                if receipt.read_text() != "completed":
                    sys.exit("missing mutation receipt")
                response.update(outcome="completed", result=result,
                    proof={"operation_key": request["context"]["operation_key"]})
            elif phase == "validate_checkpoint":
                response["valid"] = receipt.read_text() == "completed"
            else:
                sys.exit("unsupported phase")
            print(json.dumps(response))
        working-directory: subdir
        protocol: action-v1
        effect: external_mutation
        input-schema: {type: object}
        output-schema: {type: object, properties: {cwd: {type: string, enum: [subdir]}}, required: [cwd]}
`
	installProtocolWorkflow(t, e, d, &protocolFixture{}, source)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
	if err == nil || run == nil {
		t.Fatalf("expected uncertain mutation after lost response: run=%+v err=%v", run, err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	records, err := store.ActionAttempts(t.Context(), run.ID)
	if err != nil || len(records) != 1 || records[0].State != "completed" || len(records[0].Reconciliations) != 1 {
		t.Fatalf("mutation was not reconciled: records=%+v err=%v", records, err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal("completed checkpoint working directory", err)
	}
}

func TestStructuredContainerAdmissionPinsEffectiveIdentity(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[explicit], func(t *testing.T) {
			e, store, d := containerExecutor(t)
			uid, gid := os.Getuid(), os.Getgid()
			if uid == 0 {
				uid = 65532
			}
			if gid == 0 {
				gid = 65532
			}
			if explicit {
				uid, gid = 12345, 23456
				e.CommandExecution = runtimeexec.Policy{Mode: "container", Image: "ubuntu:24.04", UID: &uid, GID: &gid}
			}
			installProtocolWorkflow(t, e, d, &protocolFixture{binding: "stable"}, structuredWorkflow)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"})
			if err != nil {
				t.Fatal(err)
			}
			_, snap, err := store.Load(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			policy := snap.Definitions[run.WorkflowRef].Jobs["capture"].Container.Implementation.Policy
			if policy.UID == nil || policy.GID == nil || *policy.UID != uid || *policy.GID != gid {
				t.Fatalf("effective identity was not retained: %+v; want %d:%d", policy, uid, gid)
			}
			otherUID, otherGID := uid+1, gid+1
			e.CommandExecution = runtimeexec.Policy{Mode: "container", Image: "ubuntu:24.04", UID: &otherUID, GID: &otherGID}
			if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
				t.Fatal("current identity changed the admitted contract", err)
			}
		})
	}
}
