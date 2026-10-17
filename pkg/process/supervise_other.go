//go:build !linux && !darwin

package process

import (
	"fmt"
	"os/exec"
)

func Supervise(*exec.Cmd) (func(), error) {
	return nil, fmt.Errorf("supervision requires Linux or macOS")
}

func SuperviseWithCleanup(cmd *exec.Cmd, _ []string) (func(), error) { return Supervise(cmd) }

func SuperviseWithOptions(cmd *exec.Cmd, _ SupervisionOptions) (func(), error) { return Supervise(cmd) }
