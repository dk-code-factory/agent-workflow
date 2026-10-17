//go:build linux || darwin

package operational

import "golang.org/x/sys/unix"

func StatFilesystem(path string) (Filesystem, error) {
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	return Filesystem{Path: path, FreeBytes: stat.Bavail * uint64(stat.Bsize), FreeInodes: stat.Ffree}, err
}
