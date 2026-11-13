//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package workflows

import (
	"fmt"
	"os"
)

func openAssetFile(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("helper assets require a supported Unix host")
}
