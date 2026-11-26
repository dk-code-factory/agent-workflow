//go:build linux

package clirunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
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
				os.WriteFile(identity.BinaryPath, []byte("changed"), 0700)
			case "missing":
				os.Remove(identity.BinaryPath)
			case "symlink":
				os.Remove(identity.BinaryPath)
				os.Symlink("/bin/cat", identity.BinaryPath)
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
	done := make(chan error, 1)
	go func() { _, err := captureManagedBuild(path, managedCodexDescriptor{}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO became a build manifest")
		}
	case <-time.After(time.Second):
		t.Fatal("manifest admission blocked on FIFO without writer")
	}
}
