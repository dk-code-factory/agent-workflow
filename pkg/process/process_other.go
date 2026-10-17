//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package process

import "os/exec"

func Configure(cmd *exec.Cmd) {}
