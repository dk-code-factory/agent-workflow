//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package workflows

import (
	"os"

	"golang.org/x/sys/unix"
)

func openAssetFile(root *os.Root, name string) (*os.File, error) {
	// Check the opened descriptor without blocking on a substituted FIFO.
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
