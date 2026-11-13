//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package workflows

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"golang.org/x/sys/unix"
)

func TestHelperBundleSpecialFilesDoNotBlockVerification(t *testing.T) {
	const scenarioEnv = "AGENT_WORKFLOW_HELPER_FIFO_TEST"
	if scenario := os.Getenv(scenarioEnv); scenario != "" {
		store, err := OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		base := t.TempDir()
		putHelper(t, base, "helper.py", "print('retained')\n", 0644)
		bundle, err := store.CaptureHelperBundle(t.Context(), randomID(), base, []string{"helper.py"})
		if err != nil {
			t.Fatal(err)
		}
		directory, err := store.bundleDirectory(bundle)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(directory, scenario)
		if err = os.Remove(name); err != nil {
			t.Fatal(err)
		}
		if err = unix.Mkfifo(name, 0600); err != nil {
			t.Fatal(err)
		}
		if scenario == "owner.json" {
			lock, err := ownership.Own(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = store.CleanupHelperStaging(t.Context(), lock); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(directory); err != nil {
				t.Fatal("removed storage without a valid ownership marker", err)
			}
		} else if err = store.VerifyHelperBundle(bundle); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatal("special retained file was not rejected", err)
		}
		return
	}
	for _, scenario := range []string{"bundle.json", "files/helper.py", "owner.json"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperBundleSpecialFilesDoNotBlockVerification$")
			command.Env = append(os.Environ(), scenarioEnv+"="+scenario, "TMPDIR="+t.TempDir())
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("special-file verification exceeded its deadline: %v\n%s", ctx.Err(), output)
			}
			if err != nil {
				t.Fatalf("special-file verification failed: %v\n%s", err, output)
			}
		})
	}
}
