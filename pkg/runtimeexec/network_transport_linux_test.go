//go:build linux

package runtimeexec

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
	"golang.org/x/sys/unix"
)

func TestNetworkTransportClosedWhenGuardianPreparationFails(t *testing.T) {
	if os.Getenv("AW_TRANSPORT_FD_FAILURE") == "1" {
		// Restrict only this disposable subprocess. Leave one free descriptor for
		// Wrap's memfd, forcing the guardian's subsequent lifetime pipe to fail.
		// Initialize crypto/runtimer polling before applying the private fd limit.
		_ = NewID()
		<-time.After(time.Nanosecond)
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		highest := 0
		for _, entry := range entries {
			n, e := strconv.Atoi(entry.Name())
			if e == nil && n > highest {
				highest = n
			}
		}
		var original unix.Rlimit
		if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Fatal(err)
		}
		limited := original
		limited.Cur = uint64(highest + 1)
		cmd := exec.Command("/bin/true")
		cmd.Dir = "/"
		ref := network.Reference{Name: "test", Identity: strings.Repeat("a", 32), Generation: 1}
		ref.Endpoint = network.Endpoint(ref.Name, ref.Identity)
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
			t.Fatal(err)
		}
		stop, setupErr := superviseNetwork(context.Background(), cmd, ref)
		restoreErr := unix.Setrlimit(unix.RLIMIT_NOFILE, &original)
		if restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if stop != nil {
			stop()
			t.Fatal("guardian setup unexpectedly succeeded")
		}
		if setupErr == nil || len(cmd.ExtraFiles) != 1 || cmd.Process != nil {
			t.Fatalf("fixture did not fail after Wrap: %v files=%d", setupErr, len(cmd.ExtraFiles))
		}
		if _, err := cmd.ExtraFiles[0].Stat(); err == nil {
			t.Fatal("transport descriptor retained after Supervise failure")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNetworkTransportClosedWhenGuardianPreparationFails$")
	child.WaitDelay = 2 * time.Second
	child.Env = append(os.Environ(), "AW_TRANSPORT_FD_FAILURE=1")
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("isolated descriptor failure: %s %v", out, err)
	}
}
