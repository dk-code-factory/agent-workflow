//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package plugins

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCollectorRejectsSubstitutedSource(t *testing.T) {
	for _, kind := range []string{"regular file", "contained symlink", "ancestor symlink", "FIFO"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "declared/file", "declared")
			put(t, root, "other/file", "undeclared")
			name := filepath.Join(root, "declared/file")
			collector := newCollector(root)
			collector.open = func(source *os.Root, relative string) (*os.File, error) {
				// Substitute only after all pathname admission checks, immediately
				// before the real protected open. Every fixture is disposable.
				if kind == "ancestor symlink" {
					if err := os.Rename(filepath.Dir(name), filepath.Join(root, "original")); err != nil {
						return nil, err
					}
					if err := os.Symlink("other", filepath.Dir(name)); err != nil {
						return nil, err
					}
				} else {
					if err := os.Remove(name); err != nil {
						return nil, err
					}
					var err error
					switch kind {
					case "regular file":
						err = os.WriteFile(name, []byte("undeclared"), 0644)
					case "contained symlink":
						err = os.Symlink("../other/file", name)
					case "FIFO":
						err = unix.Mkfifo(name, 0600)
					}
					if err != nil {
						return nil, err
					}
				}
				return openSourceFile(source, relative)
			}
			result := make(chan error, 1)
			go func() { _, err := collector.read("declared/file"); result <- err }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("accepted substituted source")
				}
			case <-time.After(2 * time.Second):
				// Unblock a regressed blocking FIFO open before reporting failure.
				// Closing the disposable writer lets the goroutine finish safely.
				if kind == "FIFO" {
					fd, err := unix.Open(name, unix.O_RDWR|unix.O_NONBLOCK, 0)
					if err != nil {
						t.Fatalf("unblock substituted FIFO: %v", err)
					}
					defer unix.Close(fd)
				}
				select {
				case <-result:
				case <-time.After(2 * time.Second):
					t.Fatal("source read did not stop after cleanup")
				}
				t.Fatal("source read blocked on substitution")
			}
			if len(collector.files) != 0 {
				t.Fatal("cached rejected source bytes")
			}
		})
	}
}
