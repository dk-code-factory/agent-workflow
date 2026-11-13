// Package testutil provides disposable fixtures for deterministic tests.
package testutil

import (
	"os"
	"testing"
)

// SocketDir avoids t.TempDir's test-name prefix, which can exceed the Unix socket
// path limit beneath macOS's long temporary directory. The directory is private
// and removed only after the test's later-registered server cleanups have run.
func SocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aw-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
