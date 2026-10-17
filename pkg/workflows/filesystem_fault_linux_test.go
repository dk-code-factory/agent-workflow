//go:build linux

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

// All exhaustion is confined to a new tmpfs in a private mount namespace. The
// fixture refuses to write unless it independently verifies that tmpfs mount.
func TestDisposableFilesystemExhaustion(t *testing.T) {
	if root := os.Getenv("AW_EXHAUSTION_FIXTURE"); root != "" {
		var stat unix.Statfs_t
		if err := unix.Statfs(root, &stat); err != nil || stat.Type != unix.TMPFS_MAGIC || stat.Blocks*uint64(stat.Bsize) > 9<<20 {
			t.Fatal("refusing exhaustion outside bounded private tmpfs", err)
		}
		store, err := OpenSQLite(filepath.Join(root, "state"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if _, err = store.db.Exec("CREATE TABLE exhaustion_fixture(data BLOB)"); err != nil {
			t.Fatal(err)
		}
		if _, err = store.db.Exec("INSERT INTO exhaustion_fixture VALUES(zeroblob(16777216))"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "full") {
			t.Fatal("SQLite-full boundary was not reached", err)
		}
		var integrity string
		if err = store.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatal("SQLite-full rollback damaged storage", integrity, err)
		}
		if _, err = store.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 256; i++ {
			file, err := os.CreateTemp(root, "inode-*")
			if err != nil {
				break
			}
			_ = file.Close()
		}
		if err = unix.Statfs(root, &stat); err != nil || stat.Ffree != 0 {
			t.Fatal("inode exhaustion was not reached", stat.Ffree, err)
		}
		if file, err := os.CreateTemp(root, "must-fail-*"); err == nil {
			_ = file.Close()
			t.Fatal("temporary creation succeeded with no inodes")
		}
		return
	}
	if os.Getenv("AGENT_WORKFLOW_FILESYSTEM_TEST") != "1" {
		t.Skip("isolated mount-namespace fault suite; enable explicitly or in Linux conformance CI")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"--user", "--map-root-user", "--mount", "--propagation", "private"}
	if os.Geteuid() == 0 {
		args = []string{"--mount", "--propagation", "private"}
	}
	args = append(args, "sh", "-eu", "-c", `mount -t tmpfs -o size=8m,nr_inodes=64 tmpfs "$1"; exec "$2" -test.run=^TestDisposableFilesystemExhaustion$`, "fixture", root, self)
	cmd := exec.CommandContext(ctx, "unshare", args...)
	cmd.Env = append(os.Environ(), "AW_EXHAUSTION_FIXTURE="+root, "GORACE=atexit_sleep_ms=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private filesystem conformance unavailable or failed: %v\n%s", err, output)
	}
}
