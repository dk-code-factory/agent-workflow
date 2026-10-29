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
		var fillers []int
		defer func() {
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
				t.Error(err)
			}
			for _, fd := range fillers {
				_ = unix.Close(fd)
			}
		}()
		// RLIMIT bounds descriptor numbers, so occupy holes below the limit
		// before releasing exactly one owned slot for Wrap's payload.
		for {
			fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			if err == unix.EMFILE {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			fillers = append(fillers, fd)
		}
		if len(fillers) == 0 {
			t.Fatal("fixture has no owned descriptor slot to release")
		}
		if err := unix.Close(fillers[len(fillers)-1]); err != nil {
			t.Fatal(err)
		}
		fillers = fillers[:len(fillers)-1]
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
	for _, sparse := range []bool{false, true} {
		name := "normal"
		if sparse {
			name = "inherited-fd64"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNetworkTransportClosedWhenGuardianPreparationFails$")
			child.WaitDelay = 2 * time.Second
			child.Env = append(os.Environ(), "AW_TRANSPORT_FD_FAILURE=1")
			if sparse {
				owned, err := os.Open("/dev/null")
				if err != nil {
					t.Fatal(err)
				}
				defer owned.Close()
				child.ExtraFiles = make([]*os.File, 64-3+1)
				child.ExtraFiles[len(child.ExtraFiles)-1] = owned
			}
			if out, err := child.CombinedOutput(); err != nil {
				t.Fatalf("isolated descriptor failure: %s %v", out, err)
			}
		})
	}
}
