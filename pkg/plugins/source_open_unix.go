//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package plugins

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSourceFile(root *os.Root, name string) (*os.File, error) {
	// Reject substituted links and inspect FIFOs without waiting for a writer.
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
