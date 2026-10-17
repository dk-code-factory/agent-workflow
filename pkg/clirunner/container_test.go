//go:build linux

package clirunner

import (
	"bytes"
	"context"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerRejectsSharedGitAdministration(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /host/shared/admin"), 0600)
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "true")
	if _, err := containerCommand(cmd, Agent{Execution: Execution{Mode: "container", Image: "image"}}, root, t.TempDir(), false); err == nil {
		t.Fatal("host worktree mounted in a container")
	}
}
func TestContainerMountsAndCredentialAllowlist(t *testing.T) {
	runtime, err := exec.LookPath("podman")
	if err != nil {
		t.Skip("podman not installed")
	}
	_ = runtime
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0700)
	state := t.TempDir()
	bridge := filepath.Join(t.TempDir(), "bridge.mjs")
	_ = os.WriteFile(bridge, []byte(""), 0600)
	cmd := exec.CommandContext(context.Background(), "node", bridge)
	cmd.Env = []string{"ANTHROPIC_API_KEY=private-key", "ANTHROPIC_BASE_URL=https://gateway.invalid", "GH_TOKEN=must-not-forward", "SSH_AUTH_SOCK=/host/socket"}
	a := Agent{Bridge: bridge, Execution: Execution{Mode: "container", Image: "image", Runtime: "podman"}, Authentication: Authentication{Mode: "gateway"}}
	cleanup, err := containerCommand(cmd, a, root, state, true)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, forbidden := range []string{"private-key", "must-not-forward", "--env GH_TOKEN", "--env SSH_AUTH_SOCK", "docker.sock", "src=" + os.Getenv("HOME") + ","} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("credential or host mount in args: %s", forbidden)
		}
	}
	for _, required := range []string{"--env ANTHROPIC_API_KEY", "--read-only", "--cap-drop=ALL", "dst=/workspace,ro=true", runtimeexec.ContainerBridge, "dst=/engine-lifetime,ro=true"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing restriction %s", required)
		}
	}
	if len(cleanup) != 2 || cleanup[0] != "__container_cleanup_v2" {
		t.Fatal("container has no lifetime cleanup")
	}
}
func TestContainerExecutionAndCleanup(t *testing.T) {
	image := os.Getenv("AGENT_WORKFLOW_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set AGENT_WORKFLOW_CONTAINER_IMAGE to an existing pinned image with guardian protocol 1 and sh")
	}
	runtime := os.Getenv("AGENT_WORKFLOW_CONTAINER_RUNTIME")
	if runtime == "" {
		runtime = "podman"
	}
	if _, err := exec.LookPath(runtime); err != nil {
		t.Skip("selected container runtime unavailable")
	}
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0700)
	t.Setenv("GH_TOKEN", "controller-only")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := workflows.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: strings.Repeat("1", 32), Status: workflows.RunStatusRunning}
	if err = store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	ctx = runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: run.ID, Generation: run.Generation, LogicalID: "container-conformance"})
	cmd := exec.CommandContext(ctx, "sh", "-c", `test -z "$GH_TOKEN" && test ! -S /var/run/docker.sock && test "$PWD" = /workspace && printf isolated`)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	a := Agent{Execution: Execution{Mode: "container", Image: image, Runtime: runtime}}
	a.Execution.ownerRoot = root
	cleanup, err := superviseContainer(ctx, cmd, a, root, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err = cmd.Run(); err != nil {
		t.Fatalf("container failed: %v: %s", err, output.String())
	}
	if output.String() != "isolated" {
		t.Fatal(output.String())
	}
}

// Removing only this test's durable container simulates a runtime-side loss.
// The attached execution must fail, and cleanup must reconcile the missing ID.
func TestContainerRuntimeRemoval(t *testing.T) {
	image := os.Getenv("AGENT_WORKFLOW_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set AGENT_WORKFLOW_CONTAINER_IMAGE for real runtime conformance")
	}
	runtime := os.Getenv("AGENT_WORKFLOW_CONTAINER_RUNTIME")
	if runtime == "" {
		runtime = "podman"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: runtimeexec.NewID(), Status: workflows.RunStatusRunning}
	if err := store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	ctx = runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: run.ID, Generation: run.Generation, LogicalID: "runtime-removal"})
	command := exec.CommandContext(ctx, "sh", "-c", "printf started; exec sleep 120")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	agent := Agent{Execution: Execution{Mode: "container", Image: image, Runtime: runtime}}
	agent.Execution.ownerRoot = root
	cleanup, err := superviseContainer(ctx, command, agent, root, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	records, err := store.Invocations(ctx, run.ID, false)
	if err != nil || len(records) != 1 || records[0].Container == nil || records[0].Container.ID == "" {
		t.Fatalf("missing durable container identity: %v, %v", records, err)
	}
	id := records[0].Container.ID
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	for {
		data, err := exec.CommandContext(ctx, runtime, "inspect", "--format", "{{.State.Running}}", id).Output()
		if err != nil {
			t.Fatalf("inspect test-owned container: %v", err)
		}
		if strings.TrimSpace(string(data)) == "true" {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("execution stopped before runtime removal: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if data, err := exec.CommandContext(ctx, runtime, "rm", "--force", id).CombinedOutput(); err != nil {
		t.Fatalf("remove test-owned container: %v: %s", err, data)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runtime removal unexpectedly reported success")
		}
	case <-ctx.Done():
		t.Fatal("attached execution did not stop after runtime removal")
	}
	cleanup()
	if err := store.ReconcileInvocations(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	records, err = store.Invocations(ctx, run.ID, false)
	if err != nil || len(records) != 1 || records[0].State != "cleaned" {
		t.Fatalf("missing container was not reconciled: %v, %v", records, err)
	}
}
