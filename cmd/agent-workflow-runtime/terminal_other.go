//go:build !linux

package main

import "os/exec"

func prepareTerminal(cmd *exec.Cmd) (func(), error) { return func() {}, nil }
