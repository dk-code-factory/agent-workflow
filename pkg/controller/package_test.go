package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func writePackage(t *testing.T, root, name, value, workflow string) {
	t.Helper()
	files := map[string]string{
		"plugin.yaml":          fmt.Sprintf("schema-version: 1\nname: %s\nversion: 1.0.0\nengine-version: 0.2.2\nhosts: [codex, claude]\nskills: [{name: task, path: skills/task/SKILL.md, workflow: workflows/main.yml}]\nresources: [resources]\n", name),
		"skills/task/SKILL.md": "---\nname: task\ndescription: Run the fixture\n---\nUse the workflow.\n",
		"workflows/main.yml":   workflow,
		"resources/value":      value,
	}
	for name, contents := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contents), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "resources/empty"), 0755); err != nil {
		t.Fatal(err)
	}
}

func packageController(t *testing.T, workspace string, roles json.RawMessage) (*Server, *Client, func()) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(workspace, ".agent-workflow/assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	store, err := workflows.OpenSQLite(filepath.Join(workspace, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Dir: store.Dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: workspace, Store: store, Roles: roles, MaxParallel: 2}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("controller stop deadline")
			}
			store.Close()
		})
	}
	t.Cleanup(stop)
	c := NewClient(store.Dir)
	deadline := time.Now().Add(5 * time.Second)
	for c.Call(ctx, "GET", "/health", nil, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("controller start deadline")
		}
		time.Sleep(time.Millisecond)
	}
	return s, c, stop
}

func TestPackageReadOnlyRelocatedSourceRunsInTargetWorkspace(t *testing.T) {
	workspace := t.TempDir()
	_, client, _ := packageController(t, workspace, nil)
	parent := t.TempDir()
	installed := filepath.Join(parent, "installed")
	writePackage(t, installed, "relocated", "retained resource", `jobs:
  task:
    steps:
      - argv: [sh, -c, 'cat "$1" > result; pwd > working-directory', fixture, '${{ package.root }}/resources/value']
`)
	root := filepath.Join(parent, "moved")
	if err := os.Rename(installed, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, 0500)
		}
		return os.Chmod(name, 0400)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := plugins.Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	var run workflows.Run
	if err := client.Call(t.Context(), "POST", "/runs", StartRequest{RequestID: "relocated", Package: &PackageRequest{Root: root, Skill: "task"}}, &run); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for run.Status == workflows.RunStatusQueued || run.Status == workflows.RunStatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("completion deadline", run)
		}
		if err := client.Call(t.Context(), "GET", "/runs/"+run.ID, nil, &run); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if run.Status != workflows.RunStatusSucceeded {
		t.Fatal(run)
	}
	for name, want := range map[string]string{"result": "retained resource", "working-directory": workspace + "\n"} {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(data) != want {
			t.Fatalf("target %s: %q %v", name, data, err)
		}
	}
	after, err := plugins.Capture(root)
	if err != nil || before.Digest() != after.Digest() {
		t.Fatal("execution changed immutable installation", err)
	}
}

func TestPackageAdmissionIdentityAndConcurrentSources(t *testing.T) {
	s, c, _ := packageController(t, t.TempDir(), nil)
	const workflow = `jobs:
  task:
    steps:
      - id: gate
        uses: input/wait
        with: {prompt: approve, evidence-id: e}
      - argv: [sh, -c, 'cat "$1" > "$2"', fixture, '${{ package.root }}/resources/value', '${{ package.name }}.txt']
`
	roots := []string{t.TempDir(), t.TempDir()}
	requests := make([]StartRequest, 2)
	runs := make([]workflows.Run, 2)
	for i, root := range roots {
		writePackage(t, root, fmt.Sprintf("package-%d", i), fmt.Sprint(i), workflow)
		requests[i] = StartRequest{RequestID: fmt.Sprintf("package-request-%d", i), Package: &PackageRequest{Root: root, Skill: "task"}}
	}
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := c.Call(t.Context(), "POST", "/runs", requests[i], &runs[i]); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i, run := range runs {
		awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
		var duplicate workflows.Run
		if err := c.Call(t.Context(), "POST", "/runs", requests[i], &duplicate); err != nil || duplicate.ID != run.ID {
			t.Fatalf("same-package retry: %+v, %v", duplicate, err)
		}
		_, snap, err := s.Store.Load(t.Context(), run.ID)
		if err != nil || snap.Package == nil || snap.Package.Name != fmt.Sprintf("package-%d", i) {
			t.Fatalf("package snapshot: %+v, %v", snap.Package, err)
		}
		captured, err := plugins.Capture(roots[i])
		if err != nil || snap.Package.Digest != captured.Digest() {
			t.Fatalf("admitted digest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(roots[i], "resources/value"), []byte("replacement"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := c.Call(t.Context(), "POST", "/runs", requests[i], nil); err == nil {
			t.Fatal("changed package silently reused request")
		}
		if err := os.RemoveAll(roots[i]); err != nil {
			t.Fatal(err)
		}
		gate := controllerGates(t, c, run.ID)[0]
		if err := c.Call(t.Context(), "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", workflows.ExternalInputAnswer{AnswerID: fmt.Sprintf("answer-%d", i), EvidenceID: "e", Value: json.RawMessage(`true`)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, run := range runs {
		awaitStatus(t, c, run.ID, workflows.RunStatusSucceeded)
		got, err := os.ReadFile(filepath.Join(s.Executor.WorkspaceDir, fmt.Sprintf("package-%d.txt", i)))
		if err != nil || string(got) != fmt.Sprint(i) {
			t.Fatalf("concurrent source redirected: %q, %v", got, err)
		}
	}
	if s.Executor.DefinitionRoot != "" {
		t.Fatal("shared executor source root mutated")
	}
}

func TestPackageRecoveryAfterInstallationRemoval(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	const workflow = `jobs:
  task:
    steps:
      - {id: before, shell: sh, run: 'echo once >> effects'}
      - {id: gate, uses: input/wait, with: {prompt: approve, evidence-id: original}}
      - {argv: [sh, -c, 'cat "$1" > result; pwd > cwd', fixture, '${{ package.root }}/resources/value']}
  child:
    needs: task
    uses: workflows/child.yml
`
	writePackage(t, root, "portable", "admitted", workflow)
	if err := os.WriteFile(filepath.Join(root, "workflows/child.yml"), []byte("on: {workflow_call: {}}\njobs: {task: {steps: [{argv: [sh, -c, 'cat \"$1\" > child-result', fixture, '${{ package.root }}/resources/value']}]}}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	oldRoles := json.RawMessage(`{"selected":"original"}`)
	s, c, stop := packageController(t, workspace, oldRoles)
	var run workflows.Run
	if err := c.Call(t.Context(), "POST", "/runs", StartRequest{RequestID: "portable", Package: &PackageRequest{Root: root, Skill: "task"}}, &run); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, run.ID, workflows.RunStatusWaiting)
	gate := controllerGates(t, c, run.ID)[0]
	_, original, err := s.Store.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	s, c, _ = packageController(t, workspace, json.RawMessage(`{"selected":"changed"}`))
	if err := c.Call(t.Context(), "POST", "/runs/"+run.ID+"/inputs/"+gate.ID+"/answer", workflows.ExternalInputAnswer{AnswerID: "accepted", EvidenceID: "original", Value: json.RawMessage(`"approved"`)}, nil); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, c, run.ID, workflows.RunStatusSucceeded)
	for name, want := range map[string]string{"effects": "once\n", "result": "admitted", "child-result": "admitted", "cwd": workspace + "\n"} {
		got, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(got) != want {
			t.Fatalf("recovery %s: %q, %v", name, got, err)
		}
	}
	_, snap, err := s.Store.Load(t.Context(), run.ID)
	if err != nil || string(snap.Roles) != string(oldRoles) || !workflows.SamePackage(snap.Package, original.Package) {
		t.Fatalf("recovery changed binding: %+v, %s, %v", snap.Package, snap.Roles, err)
	}
	binding, retainedRoot, err := s.Store.PackageAssets(t.Context(), run.ID)
	if err != nil || binding.Digest != original.Package.Digest {
		t.Fatalf("retained resource reference: %+v, %v", binding, err)
	}
	if _, err := os.Stat(filepath.Join(retainedRoot, "resources/empty")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(retainedRoot, "resources/value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retainedRoot, "resources/value"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Store.PackageAssets(t.Context(), run.ID); err == nil {
		t.Fatal("corrupt package resources accepted")
	}
	if _, err := s.Executor.Resume(t.Context(), run.ID, nil, false); err == nil {
		t.Fatal("completed resume ignored package corruption")
	}
}
