package plugins

import (
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func permitBuildDirectory(name string, expected fs.FileInfo) error {
	// Event-only opens do not require the read bits that umask may remove.
	f, err := os.OpenFile(name, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
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
	return f.Chmod(0700)
}
