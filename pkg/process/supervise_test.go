//go:build linux

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGuardianStopsDescendantsAfterParentDeath(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_GUARDIAN_PARENT_TEST") == "1" {
		cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 300 & echo $! > "$1"; wait`, "sh", os.Getenv("AGENT_WORKFLOW_GUARDIAN_PID_FILE"))
		cleanup, err := SuperviseWithOptions(cmd, SupervisionOptions{CompletionPath: os.Getenv("AGENT_WORKFLOW_GUARDIAN_COMPLETION"), Token: "fixture-completion-token"})
		if err != nil {
			os.Exit(2)
		}
		defer cleanup()
		_ = cmd.Run()
		os.Exit(0)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	self, _ := os.Executable()
	parent := exec.Command(self, "-test.run=^TestGuardianStopsDescendantsAfterParentDeath$")
	parent.Env = append(os.Environ(), "AGENT_WORKFLOW_GUARDIAN_PARENT_TEST=1", "AGENT_WORKFLOW_GUARDIAN_PID_FILE="+pidFile, "AGENT_WORKFLOW_GUARDIAN_COMPLETION="+pidFile+".complete")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Process.Kill(); _ = parent.Wait() }()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant not launched")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err == nil {
			fields := strings.Fields(string(b))
			if len(fields) > 2 && fields[2] == "Z" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant survived controller SIGKILL")
		}
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := ReadCompletion(pidFile+".complete", "fixture-completion-token"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner-loss cleanup did not leave a durable acknowledgement")
		}
		time.Sleep(5 * time.Millisecond)
	}

}

func TestGuardianStopsDetachedDescendantOnNormalCompletion(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "detached.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `setsid sleep 300 >/dev/null 2>&1 & echo $! > "$1"; exit 0`, "sh", pidFile)
	cleanup, err := Supervise(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err = syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatal("detached descendant survived normal completion")
	}
}
