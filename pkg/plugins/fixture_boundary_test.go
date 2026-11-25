package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func boundaryExecutor(t *testing.T, source string) (*workflows.Executor, *workflows.SQLiteStore, *fixturePorts) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows/main.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ports := newFixturePorts(nil)
	e := &workflows.Executor{WorkspaceDir: root, Store: store, Boundary: ports, Actions: ports, Agents: &fixtureNative{ports: ports, config: fixtureConfig("")}}
	return e, store, ports
}
func TestPluginFixtureBoundaryRejectsPolicyBeforeDiscovery(t *testing.T) {
	for _, policy := range []runtimeexec.Policy{
		{Mode: "container", Image: "must-not-resolve"},
		{Mode: "local", NetworkRuntime: "/must-not-discover-a-runtime"},
	} {
		t.Run(policy.Mode+policy.NetworkRuntime, func(t *testing.T) {
			e, _, ports := boundaryExecutor(t, "jobs: {main: {steps: [{argv: [must-not-execute]}]}}\n")
			e.CommandExecution = policy
			if _, err := e.Prepare(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"}); err == nil {
				t.Fatal("unsupported policy passed admission")
			}
			if ports.count() != 0 {
				t.Fatal("unsupported policy invoked an effect")
			}
		})
	}
}
func TestPluginFixtureBoundaryChecksCompletedResumeBeforeContainerRecovery(t *testing.T) {
	e, store, ports := boundaryExecutor(t, "jobs: {main: {steps: [{argv: [must-not-execute]}]}}\n")
	ports.operations = []FixtureOperation{{Uses: "command/run", Result: map[string]any{"stdout": "done"}}}
	ports.used = make([]bool, 1)
	run, err := e.Run(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	// Model a valid older admitted container job. The engine must reject this
	// fixture capability before resolving the image, guardian or completed effect.
	_, snap, err := store.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := snap.Definitions[run.WorkflowRef].Jobs["main"]
	job.Container = &workflows.JobContainer{Image: "must-not-resolve"}
	snap.Definitions[run.WorkflowRef].Jobs["main"] = job
	// Admit a historical completed checkpoint, retaining its old definition
	// hash and results. Resume must reject unsupported container effects even
	// before a completed-job shortcut or container receipt validation.
	raw, _ := json.Marshal(snap.Definitions[run.WorkflowRef])
	hash := sha256.Sum256(raw)
	snap.Hashes[run.WorkflowRef] = hex.EncodeToString(hash[:])
	run.ID = "0123456789abcdef0123456789abcdef"
	run.OriginID = run.ID
	run.Generation = 0
	run.Owner = ""
	snap.Metadata["origin_run_id"] = run.ID
	if err = store.Admit(t.Context(), run, snap); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil || !strings.Contains(err.Error(), "fixtures do not support job containers") {
		t.Fatal(err)
	}
	if ports.count() != 1 {
		t.Fatal("completed command replayed")
	}
}
func TestPluginFixtureBoundaryRejectsNestedContainerBeforeAnyCommand(t *testing.T) {
	e, _, ports := boundaryExecutor(t, `jobs:
  main:
    steps:
      - id: first
        argv: [must-not-execute]
      - id: nested
        sequence:
          - uses: docker://alpine:latest
            with: {args: echo unsafe}
`)
	if _, err := e.Run(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "fixtures do not support") {
		t.Fatal(err)
	}
	if ports.count() != 0 {
		t.Fatal("command ran before closure rejection")
	}
}
func TestPluginFixtureBoundaryMissingCommandCannotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unsafe")
	e, _, ports := boundaryExecutor(t, "jobs: {main: {steps: [{argv: [touch, '"+marker+"']}]}}\n")
	if _, err := e.Run(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"}); err == nil {
		t.Fatal("unconfigured command passed")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unconfigured command executed", err)
	}
	if ports.count() != 0 || ports.finish() == nil {
		t.Fatal("missing response did not fail closed")
	}
}
func TestPluginFixtureBoundaryCancellationIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e, _, ports := boundaryExecutor(t, "jobs: {main: {steps: [{argv: [must-not-execute]}]}}\n")
	if _, err := e.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if ports.count() != 0 {
		t.Fatal("canceled workflow dispatched")
	}
}

func TestPluginFixtureBoundaryChecksReusableChildBeforeAnyCommand(t *testing.T) {
	e, _, ports := boundaryExecutor(t, `jobs:
  first: {steps: [{argv: [must-not-execute]}]}
  child: {uses: ./workflows/child.yml}
`)
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/child.yml"), []byte(`on: {workflow_call: {}}
jobs:
  child:
    steps: [{uses: 'docker://alpine:latest', with: {args: echo unsafe}}]
`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "fixtures do not support") {
		t.Fatal(err)
	}
	if ports.count() != 0 {
		t.Fatal("command ran before child rejection")
	}
}
