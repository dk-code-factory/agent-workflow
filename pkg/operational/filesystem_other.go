//go:build !linux && !darwin

package operational

import "fmt"

func StatFilesystem(path string) (Filesystem, error) {
	return Filesystem{Path: path}, fmt.Errorf("filesystem readiness requires Linux or macOS")
}
