//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os/exec"
	"syscall"
)

func detachController(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
