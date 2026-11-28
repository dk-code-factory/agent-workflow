//go:build linux

package clirunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"golang.org/x/sys/unix"
)

func managedFixtureExecutable(t *testing.T) *ManagedCodexBuildIdentity {
	t.Helper()
	raw, err := os.ReadFile("/bin/cat")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture-cat")
	if err = os.WriteFile(path, raw, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return &ManagedCodexBuildIdentity{BinaryPath: path, BinaryBytes: int64(len(raw)), BinarySHA256: hex.EncodeToString(sum[:])}
}

func TestManagedCodexActualGuardianChildUsesHeldImage(t *testing.T) {
	identity := managedFixtureExecutable(t)
	// Coreutils may reject descriptor paths as argv[0]; the owned Go fixture
	// exercises image identity without depending on that utility-specific rule.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identity.BinaryPath, raw, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	identity.BinarySHA256, identity.BinaryBytes = hex.EncodeToString(sum[:]), int64(len(raw))
	image, err := openManagedExecutable(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer image.file.Close()
	// Replacing the pathname after opening cannot choose another image.
	if err = os.Rename(identity.BinaryPath, identity.BinaryPath+".original"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(identity.BinaryPath, []byte("not the retained executable"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := image.command(ctx, []string{"-test.run=^TestManagedCodexELFFixture$"})
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var diagnostics bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &diagnostics
	stop, err := runtimeexec.SuperviseLocalDescriptors(ctx, cmd, runtimeexec.Policy{}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Cancel()
			_ = cmd.Wait()
		}
	}()
	pid, err := image.verifyGuardianChild(ctx, cmd.Process)
	if err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		waited = true
		t.Fatalf("%v; guardian=%d path=%s argv=%q stderr=%s exit=%v", err, cmd.Process.Pid, cmd.Path, cmd.Args, diagnostics.String(), cmd.ProcessState)
	}
	if pid == cmd.Process.Pid {
		t.Fatal("verified guardian instead of actual child")
	}
	if _, err = input.Write([]byte("input after image verification")); err != nil {
		t.Fatal(err)
	}
	input.Close()
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "input after image verification") {
		t.Fatal(output.String())
	}
	if _, err = openManagedExecutable(identity); err == nil {
		t.Fatal("recovery recaptured replaced binary")
	}
}

func TestManagedCodexELFFixture(t *testing.T) {
	if len(os.Args) != 2 || os.Args[1] != "-test.run=^TestManagedCodexELFFixture$" {
		return
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCodexExecutableRejectsChangedMissingAndSymlink(t *testing.T) {
	for _, mode := range []string{"changed", "missing", "symlink", "wrong-hash"} {
		t.Run(mode, func(t *testing.T) {
			identity := managedFixtureExecutable(t)
			switch mode {
			case "changed":
				if err := os.WriteFile(identity.BinaryPath, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(identity.BinaryPath); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(identity.BinaryPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/bin/cat", identity.BinaryPath); err != nil {
					t.Fatal(err)
				}
			case "wrong-hash":
				identity.BinarySHA256 = string(bytes.Repeat([]byte{'a'}, 64))
			}
			if image, err := openManagedExecutable(identity); err == nil {
				image.file.Close()
				t.Fatal("changed executable accepted")
			}
		})
	}
}

func TestManagedCodexManifestFIFORejectedWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest-fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd, output, err := managedManifestFIFOProbe(ctx, "capture", path)
	assertManagedManifestProbeReaped(t, cmd)
	if err != nil {
		t.Fatalf("FIFO rejection probe failed: %v; context=%v; output=%s", err, ctx.Err(), output)
	}
}

func TestManagedCodexManifestFIFOBlockedProbeIsReaped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-manifest-fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Include startup headroom for the race-instrumented Go child. The same
	// bounded probe must reap a regression that blocks before any writer opens.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd, output, err := managedManifestFIFOProbe(ctx, "blocked", path)
	assertManagedManifestProbeReaped(t, cmd)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || err == nil {
		t.Fatalf("blocked probe escaped cancellation: %v; context=%v; output=%s", err, ctx.Err(), output)
	}
	t.Logf("blocked FIFO probe pid=%d context=%v state=%s", cmd.Process.Pid, ctx.Err(), cmd.ProcessState.String())
	marker, err := os.ReadFile(path + ".entered")
	if err != nil || string(marker) != "blocking FIFO open" {
		t.Fatal("child did not reach the synthetic blocking probe", string(marker), err)
	}
}

func managedManifestFIFOProbe(ctx context.Context, mode, path string) (*exec.Cmd, []byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestManagedCodexManifestFIFOProbe$", "--", mode, path)
	// This helper spawns no children. CombinedOutput waits and reaps this exact
	// process, including after CommandContext kills a blocked probe.
	output, err := cmd.CombinedOutput()
	return cmd, output, err
}

func assertManagedManifestProbeReaped(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil || cmd.ProcessState == nil {
		t.Fatal("manifest probe did not start and finish its owned wait", cmd)
	}
	var status unix.WaitStatus
	if pid, err := unix.Wait4(cmd.Process.Pid, &status, unix.WNOHANG, nil); !errors.Is(err, unix.ECHILD) {
		t.Fatalf("manifest probe was not already reaped: pid=%d wait=%d error=%v", cmd.Process.Pid, pid, err)
	}
	t.Logf("manifest probe pid=%d actual Wait complete; wait4=ECHILD", cmd.Process.Pid)
}

func TestManagedCodexManifestFIFOProbe(t *testing.T) {
	// Normal package invocation is a no-op; only the exact owned probe argv
	// enters either mode. Neither mode starts another process.
	if len(os.Args) != 5 || os.Args[1] != "-test.run=^TestManagedCodexManifestFIFOProbe$" || os.Args[2] != "--" {
		return
	}
	mode, path := os.Args[3], os.Args[4]
	switch mode {
	case "capture":
		if _, err := captureManagedBuild(path, managedCodexDescriptor{}); err == nil {
			t.Fatal("FIFO became a build manifest")
		}
	case "blocked":
		if err := os.WriteFile(path+".entered", []byte("blocking FIFO open"), 0600); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err == nil {
			unix.Close(fd)
		}
		t.Fatal("synthetic no-writer FIFO open unexpectedly returned", err)
	default:
		t.Fatal("unknown owned manifest probe mode", mode)
	}
}
