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

	"golang.org/x/sys/unix"
)

func TestNativeAgentDefinitionRejectsSubstitution(t *testing.T) {
	if replacement := os.Getenv("AGENT_DEFINITION_REPLACEMENT"); replacement != "" {
		path := filepath.Join(t.TempDir(), "review.toml")
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		admitted, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Retain the old inode so a replacement cannot reuse its identity.
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if replacement == "fifo" {
			err = unix.Mkfifo(path, 0600)
		} else {
			err = os.WriteFile(path, []byte("replaced"), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readAgentDefinition(path, admitted); err == nil || !strings.Contains(err.Error(), "changed before reading") {
			t.Fatalf("substituted %s accepted: %v", replacement, err)
		}
		return
	}
	for _, replacement := range []string{"fifo", "regular"} {
		t.Run(replacement, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeAgentDefinitionRejectsSubstitution$")
			child.Env = append(os.Environ(), "AGENT_DEFINITION_REPLACEMENT="+replacement)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("substitution reader did not reject promptly: %v (%v)\n%s", err, ctx.Err(), output)
			}
		})
	}
}
