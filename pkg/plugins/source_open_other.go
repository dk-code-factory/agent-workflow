//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package plugins

import "os"

func openSourceFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}
