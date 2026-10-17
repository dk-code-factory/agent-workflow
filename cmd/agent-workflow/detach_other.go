//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import "os/exec"

func detachController(cmd *exec.Cmd) {}
