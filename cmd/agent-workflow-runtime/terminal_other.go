//go:build !linux

package main

import (
	"context"
	"os/exec"
)

func prepareTerminal(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd) (func() error, error) {
	return func() error { return nil }, nil
}
