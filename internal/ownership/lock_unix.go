//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package ownership

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

type Lock struct{ file *os.File }

// Holds checks that the live ownership descriptor belongs to this workspace.
func (l *Lock) Holds(dir string) bool {
	if l == nil || l.file == nil {
		return false
	}
	owned, err := l.file.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(filepath.Join(dir, "owner.lock"))
	return err == nil && os.SameFile(owned, current)
}

func Own(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workspace already has an execution owner: %w", err)
	}
	return &Lock{file: f}, nil
}
func (l *Lock) Close() error { _ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN); return l.file.Close() }
