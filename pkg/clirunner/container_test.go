//go:build linux

package clirunner

import (
	"bytes"
	"context"
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
	for _, required := range []string{"--env ANTHROPIC_API_KEY", "--read-only", "--cap-drop=ALL", "dst=/workspace,ro=true", "dst=/bridge,ro=true"} {
		if !strings.Contains(args, required) {
			t.Fatalf("missing restriction %s", required)
		}
	}
	if len(cleanup) != 3 || cleanup[0] != "__container_cleanup_v1" {
		t.Fatal("container has no lifetime cleanup")
	}
}
func TestContainerExecutionAndCleanup(t *testing.T) {
	image := os.Getenv("AGENT_WORKFLOW_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set AGENT_WORKFLOW_CONTAINER_IMAGE to an existing image with sh")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman unavailable")
	}
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0700)
	t.Setenv("GH_TOKEN", "controller-only")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `test -z "$GH_TOKEN" && test ! -S /var/run/docker.sock && test "$PWD" = /workspace && printf isolated`)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	a := Agent{Execution: Execution{Mode: "container", Image: image, Runtime: "podman"}}
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
