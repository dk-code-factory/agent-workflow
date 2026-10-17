//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package clirunner

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
