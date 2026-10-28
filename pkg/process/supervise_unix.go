//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// A separate guardian owns the tool process group. Its lifetime pipe loses the
// writer when the controller exits, including SIGKILL. This avoids orphan cleanup
// based on a stale PID recorded by a previous controller.
func init() {
	if len(os.Args) < 4 || os.Args[1] != "__agent_workflow_guardian_v1" {
		return
	}
	fd, err := strconv.Atoi(os.Args[2])
	if err != nil {
		os.Exit(125)
	}
	lifetime := os.NewFile(uintptr(fd), "controller-lifetime")
	if lifetime == nil {
		os.Exit(125)
	}
	syscall.CloseOnExec(fd)
	var cleanupArgs []string
	var completionPath, completionToken string
	terminal := false
	target := 3
	for target < len(os.Args) {
		if os.Args[target] == "--terminal" {
			terminal = true
			target++
		} else if os.Args[target] == "--cleanup" {
			if target+1 >= len(os.Args) || json.Unmarshal([]byte(os.Args[target+1]), &cleanupArgs) != nil {
				os.Exit(125)
			}
			target += 2
		} else if os.Args[target] == "--completion" {
			if target+2 >= len(os.Args) {
				os.Exit(125)
			}
			completionPath, completionToken = os.Args[target+1], os.Args[target+2]
			target += 3
		} else {
			break
		}
	}
	if target >= len(os.Args) {
		os.Exit(125)
	}
	finish := func(code int, cleanupErr error) {
		if err := writeCompletion(completionPath, completionToken, cleanupErr); err != nil {
			fmt.Fprintln(os.Stderr, "guardian completion persistence failed")
			code = 125
		}
		os.Exit(code)
	}
	var cleanupError error
	var cleanupOnce sync.Once
	clean := func() {
		cleanupOnce.Do(func() {
			if len(cleanupArgs) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if cleanupArgs[0] == "__container_cleanup_v2" && len(cleanupArgs) == 2 {
					var owner containerapi.Owner
					if err := json.Unmarshal([]byte(cleanupArgs[1]), &owner); err != nil {
						cleanupError = err
					} else {
						cleanupError = containerapi.Remove(ctx, owner)
					}
					return
				}
				cleanup := exec.CommandContext(ctx, cleanupArgs[0], cleanupArgs[1:]...)
				cleanup.Stdout = io.Discard
				cleanup.Stderr = os.Stderr
				cleanupError = cleanup.Run()
			}
		})
	}
	if len(cleanupArgs) == 0 {
		if err = prepareDescendants(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			finish(125, err)
		}
	}
	cmd := exec.Command(os.Args[target], os.Args[target+1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if terminal {
		// The frontend supplied an invocation-owned PTY. Acquire only that
		// terminal in a fresh session; never steal the caller's terminal.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}
	if err = cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		clean()
		finish(127, cleanupError)
	}
	var groupMu sync.Mutex
	groupOwned := true
	stopOwnedGroup := func() {
		groupMu.Lock()
		defer groupMu.Unlock()
		if groupOwned {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	go func() {
		_, _ = io.Copy(io.Discard, lifetime)
		stopOwnedGroup()
		clean()
	}()
	waitErr := waitLeaderExit(cmd.Process.Pid)
	groupMu.Lock()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	groupOwned = false
	groupMu.Unlock()
	// Only reap after group termination and disabling future PID-based signals.
	var descendantErr error
	if len(cleanupArgs) == 0 {
		descendantErr = stopDescendants(cmd.Process.Pid)
	}
	err = cmd.Wait()
	if descendantErr != nil {
		fmt.Fprintln(os.Stderr, descendantErr)
		clean()
		finish(125, descendantErr)
	}
	if waitErr != nil {
		fmt.Fprintln(os.Stderr, "guardian wait failed:", waitErr)
		clean()
		finish(125, waitErr)
	}
	clean()
	if cleanupError != nil {
		finish(125, cleanupError)
	}
	code := 0
	if err != nil {
		code = 1
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
			if code < 0 {
				code = 128
				if terminal {
					if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
						code += int(status.Signal())
					}
				}
			}
		}
	}
	finish(code, nil)
}

// Supervise wraps a not-yet-started command. Call cleanup after Wait/Run, even
// when Start fails. The original command's streams, directory and environment
// are preserved; no shell interprets its arguments.
func Supervise(cmd *exec.Cmd) (func(), error) { return SuperviseWithCleanup(cmd, nil) }
func SuperviseWithCleanup(cmd *exec.Cmd, cleanupArgs []string) (func(), error) {
	return SuperviseWithOptions(cmd, SupervisionOptions{Cleanup: cleanupArgs})
}
func SuperviseWithOptions(cmd *exec.Cmd, options SupervisionOptions) (func(), error) {
	cleanupArgs := options.Cleanup
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	original := cmd.Path
	args := append([]string{}, cmd.Args[1:]...)
	fd := 3 + len(cmd.ExtraFiles)
	cmd.Path = executable
	prefix := []string{executable, "__agent_workflow_guardian_v1", strconv.Itoa(fd)}
	if len(cleanupArgs) > 0 {
		data, err := json.Marshal(cleanupArgs)
		if err != nil {
			_ = reader.Close()
			_ = writer.Close()
			return nil, err
		}
		prefix = append(prefix, "--cleanup", string(data))
	}
	if options.CompletionPath != "" {
		prefix = append(prefix, "--completion", options.CompletionPath, options.Token)
	}
	cmd.Args = append(append(prefix, original), args...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, reader)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var once sync.Once
	stop := func() { once.Do(func() { _ = writer.Close() }) }
	cmd.Cancel = func() error { stop(); return nil }
	cmd.WaitDelay = 5 * time.Second
	if len(cleanupArgs) > 0 {
		cmd.WaitDelay = 20 * time.Second
	}
	cleanup := func() { stop(); _ = reader.Close() }
	return cleanup, nil
}
