package plugins

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// O_PATH opens even a directory whose owner bits were removed by umask.
// AT_EMPTY_PATH changes that opened inode, never a replacement pathname.
func permitBuildDirectory(name string, expected fs.FileInfo) error {
	f, err := os.OpenFile(name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(expected, info) {
		return fmt.Errorf("%s changed identity; directory permissions retained", name)
	}
	err = unix.Fchmodat(int(f.Fd()), "", 0700, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EOPNOTSUPP) {
		// Older Linux kernels lack fchmodat2. The proc link names this held
		// descriptor's inode, not the caller-controlled distribution pathname.
		return os.Chmod(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), 0700)
	}
	return err
}
