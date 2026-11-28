package workflows

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentStateBinaryTargetsRetainRepresentation(t *testing.T) {
	for _, target := range []string{"/cf-state", "/cf-état", "/cf-\xff", "/cf-\xfe"} {
		t.Run(base64.StdEncoding.EncodeToString([]byte(target)), func(t *testing.T) {
			definition := "persistent-state: {ledger: {mount-target: !!binary " + base64.StdEncoding.EncodeToString([]byte(target)) + "}}\njobs: {task: {steps: [{argv: [true]}]}}"
			workflow, err := Parse([]byte(definition))
			if err != nil {
				t.Fatal(err)
			}
			if got := workflow.PersistentState["ledger"].MountTarget; got != target {
				t.Fatalf("binary target decoded as %q, want %q", got, target)
			}
			err = Validate(workflow)
			if target == "/cf-\xff" || target == "/cf-\xfe" {
				if err == nil || !strings.Contains(err.Error(), "UTF-8") {
					t.Fatalf("nonrepresentable target was not rejected: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(workflow)
			if err != nil {
				t.Fatal(err)
			}
			var restored Workflow
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if err := Validate(&restored); err != nil {
				t.Fatal(err)
			}
			if got := restored.PersistentState["ledger"].MountTarget; got != target {
				t.Fatalf("retained target changed: %q != %q", got, target)
			}
		})
	}
	// These byte-distinct targets would both become /cf-\uFFFD in JSON.
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ui-compat", "persistent-state-binary-invalid-utf8.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(workflow); err == nil {
		t.Fatal("accepted targets that collapse during JSON serialization")
	}
}

const persistentWorkflow = `persistent-state:
  ledger: {mount-target: /cf-workflows-state}
jobs:
  write:
    container: ubuntu:24.04
    steps:
      - id: save
        shell: sh
        run: 'echo receipt >> "$CF_WORKFLOWS_STATE/ledger"'
        env: {CF_WORKFLOWS_STATE: /cf-workflows-state}
      - id: read
        argv: [cat, /cf-workflows-state/ledger]
  approval:
    needs: write
    steps:
      - {id: gate, uses: input/wait, with: {prompt: Continue, evidence-id: original}}
  verify:
    needs: approval
    steps:
      - id: read
        uses: docker://ubuntu:24.04
        with: {entrypoint: cat, args: /cf-workflows-state/ledger}
`

func TestPackagePersistentStateSurvivesStepsFreshRunsAndResume(t *testing.T) {
	e, store, d := containerExecutor(t)
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(store.Dir, "assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	stateHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	source := packageSourceFixture([]AssetInput{{Path: "workflows/main.yml", Mode: 0644, Data: []byte(persistentWorkflow)}})
	var firstSource string
	for n := 1; n <= 2; n++ {
		run, err := e.Run(t.Context(), RunRequest{Ref: source.Binding.Workflow, Package: source})
		if err != nil || run.Status != RunStatusWaiting {
			t.Fatalf("run %d: %+v %v", n, run, err)
		}
		want := strings.Repeat("receipt\n", n)
		if got := run.Jobs["write"].Steps["read"].Outputs["stdout"]; got != want {
			t.Fatalf("step did not retain previous state: %q", got)
		}
		_, snap, err := store.Load(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(snap)
		if strings.Contains(string(raw), stateHome) {
			t.Fatal("persistent host path leaked into snapshot")
		}
		d.mu.Lock()
		for _, mounts := range d.persistentMounts {
			if p := mounts["/cf-workflows-state"]; p != "" {
				if firstSource == "" {
					firstSource = p
				}
				if p != firstSource {
					t.Errorf("persistent source changed between helpers: %q != %q", p, firstSource)
				}
			}
		}
		d.mu.Unlock()
		if firstSource == "" {
			t.Fatal("fake runtime received no persistent mount")
		}
		answerGate(t, store, inputGate(t, store, run.ID))
		// Reopen storage as a replacement controller would, then recover only
		// from retained package bytes and the engine-selected namespace.
		directory := store.Dir
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = OpenSQLite(directory)
		if err != nil {
			t.Fatal(err)
		}
		replacementStore := store
		t.Cleanup(func() { replacementStore.Close() })
		e.Store = store
		replacement := *e
		resumed, err := replacement.Resume(t.Context(), run.ID, nil, false)
		if err != nil || resumed.Status != RunStatusSucceeded {
			t.Fatalf("resume: %+v %v", resumed, err)
		}
		if got := resumed.Jobs["verify"].Steps["read"].Outputs["stdout"]; got != want {
			t.Fatalf("resumed container lost durable state: %q", got)
		}
	}
	if !strings.HasPrefix(firstSource, stateHome+string(filepath.Separator)) || strings.HasPrefix(firstSource, e.WorkspaceDir+string(filepath.Separator)) || strings.HasPrefix(firstSource, store.Dir+string(filepath.Separator)) {
		t.Fatal("persistent directory not isolated", firstSource)
	}
	data, err := os.ReadFile(filepath.Join(firstSource, "ledger"))
	if err != nil || string(data) != "receipt\nreceipt\n" {
		t.Fatal("durable ledger missing", string(data), err)
	}
}

func TestPersistentStateDeclarationsFailClosed(t *testing.T) {
	for _, declaration := range []string{
		"ledger: {mount-target: /workspace/private}",
		"../escape: {mount-target: /private-state}",
		"ledger: {mount-target: /private-state, source: /host}",
		"a: {mount-target: /private-state}, b: {mount-target: /private-state/child}",
	} {
		w, err := Parse([]byte("persistent-state: {" + declaration + "}\njobs: {task: {steps: [{argv: [true]}]}}"))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Errorf("accepted %s", declaration)
		}
	}
	e, _, d := containerExecutor(t)
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/test.yml"), []byte(persistentWorkflow), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml"}); err == nil || !strings.Contains(err.Error(), "admitted package") {
		t.Fatal("unpackaged directory declaration accepted", err)
	}
	if d.creates != 0 {
		t.Fatal("container launched before rejection")
	}
}

func TestPersistentStateNamespaceAndBindingValidation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "resources"))
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{WorkspaceDir: workspace, packageBinding: &PackageBinding{Name: "fixture"}}
	bindings, err := e.bindPersistentState(map[string]PersistentStateDirectory{"ledger": {MountTarget: "/cf-workflows-state"}})
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine"))
	if err != nil {
		t.Fatal(err)
	}
	original := mounts[0].Source
	e.packageBinding.PackageVersion = "2.0.0"
	same, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine"))
	if err != nil || same[0].Source != original {
		t.Fatal("package upgrade changed durable namespace", err)
	}
	e.packageBinding.Name = "foreign"
	if _, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine")); err == nil {
		t.Fatal("cross-package binding accepted")
	}
	bindings[0].Package = "foreign"
	foreign, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine"))
	if err != nil || foreign[0].Source == original {
		t.Fatal("packages share state", err)
	}
	e.WorkspaceDir = filepath.Join(root, "other-workspace")
	if err := os.Mkdir(e.WorkspaceDir, 0700); err != nil {
		t.Fatal(err)
	}
	other, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine"))
	if err != nil || other[0].Source == foreign[0].Source {
		t.Fatal("target repositories share state", err)
	}
	bindings[0].Name = "../escape"
	if _, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine")); err == nil {
		t.Fatal("escaping resource name accepted")
	}
	bindings[0].Name = "ledger"
	if err := os.RemoveAll(other[0].Source); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspace, other[0].Source); err != nil {
		t.Fatal(err)
	}
	if _, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine")); err == nil {
		t.Fatal("symlink resource accepted")
	}
	t.Setenv("XDG_STATE_HOME", e.WorkspaceDir)
	if _, err := e.persistentStateMounts(bindings, filepath.Join(root, "engine")); err == nil {
		t.Fatal("resource inside workspace accepted")
	}
	if _, err := os.Stat(filepath.Join(e.WorkspaceDir, "agent-workflow")); !os.IsNotExist(err) {
		t.Fatal("unsafe resource created before validation", err)
	}
}

func TestPersistentStateActionIdentityUsesSymbolsWithoutHostPaths(t *testing.T) {
	e, _, _ := containerExecutor(t)
	e.packageBinding = &PackageBinding{Name: "fixture"}
	w, err := Parse([]byte("persistent-state: {ledger: {mount-target: /cf-workflows-state}}\n" + structuredWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]*Workflow{"workflows/main.yml": w}
	if err := e.pinContainers(t.Context(), all); err != nil {
		t.Fatal(err)
	}
	e.actionContracts = map[string]ActionContract{}
	if err := e.configureContainerActions(all); err != nil {
		t.Fatal(err)
	}
	name := containerActionName("workflows/main.yml", "capture", "current")
	original := e.actionContracts[name]
	if original.Implementation == "" {
		t.Fatal("missing action implementation")
	}
	firstRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{firstRoot, secondRoot} {
		t.Setenv("XDG_STATE_HOME", root)
		if _, err := e.persistentStateMounts(w.Jobs["capture"].Container.Implementation.PersistentState, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.configureContainerActions(all); err != nil {
			t.Fatal(err)
		}
		if e.actionContracts[name].Fingerprint != original.Fingerprint {
			t.Fatal("host storage location changed action identity")
		}
		raw, _ := json.Marshal(all)
		if strings.Contains(string(raw), root) {
			t.Fatal("host storage location retained in definition snapshot")
		}
	}
	job := w.Jobs["capture"]
	job.Container.Implementation.PersistentState[0].Name = "other-ledger"
	if err := e.configureContainerActions(all); err != nil {
		t.Fatal(err)
	}
	if e.actionContracts[name].Implementation == original.Implementation {
		t.Fatal("symbolic mount identity ignored")
	}
	job.Container.Implementation.PersistentState = nil
	if err := e.pinContainers(t.Context(), all); err == nil || !strings.Contains(err.Error(), "binding changed") {
		t.Fatal("changed retained mount binding accepted", err)
	}
}
