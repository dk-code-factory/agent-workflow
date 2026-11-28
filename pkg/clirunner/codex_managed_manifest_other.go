//go:build !linux

package clirunner

import (
	"fmt"
	"os"
)

func openManagedManifest(string) (*os.File, error) {
	return nil, fmt.Errorf("managed Codex manifests require the verified Linux execution profile")
}
