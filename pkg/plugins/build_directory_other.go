//go:build !linux && !darwin

package plugins

import "io/fs"

func permitBuildDirectory(name string, expected fs.FileInfo) error {
	return setDirectoryMode(createdPath{name, expected}, 0700)
}
