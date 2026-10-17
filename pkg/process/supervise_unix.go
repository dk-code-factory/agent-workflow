//go:build linux || darwin

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	target := 3
	if len(os.Args) > 4 && os.Args[3] == "--cleanup" {
		if len(os.Args) < 7 || json.Unmarshal([]byte(os.Args[4]), &cleanupArgs) != nil {
			os.Exit(125)
		}
		target = 5
	}
	var cleanupError error
	var cleanupOnce sync.Once
	clean := func() {
		cleanupOnce.Do(func() {
			if len(cleanupArgs) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if cleanupArgs[0] == "__container_cleanup_v1" && len(cleanupArgs) == 3 {
					runtime, name := cleanupArgs[1], cleanupArgs[2]
					remove := exec.CommandContext(ctx, runtime, "rm", "--force", name)
					diagnostics, err := remove.CombinedOutput()
					if err != nil {
						listing, listErr := exec.CommandContext(ctx, runtime, "ps", "-a", "--filter", "name=^"+name+"$", "--format", "{{.Names}}").Output()
						if listErr != nil || strings.TrimSpace(string(listing)) != "" {
							cleanupError = fmt.Errorf("container cleanup failed: %w", err)
							_, _ = os.Stderr.Write(diagnostics)
						}
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
			os.Exit(125)
		}
	}
	cmd := exec.Command(os.Args[target], os.Args[target+1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(127)
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
		os.Exit(125)
	}
	if waitErr != nil {
		fmt.Fprintln(os.Stderr, "guardian wait failed:", waitErr)
		clean()
		os.Exit(125)
	}
	clean()
	if cleanupError != nil {
		os.Exit(125)
	}
	code := 0
	if err != nil {
		code = 1
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
			if code < 0 {
				code = 128
			}
		}
	}
	os.Exit(code)
}

// Supervise wraps a not-yet-started command. Call cleanup after Wait/Run, even
// when Start fails. The original command's streams, directory and environment
// are preserved; no shell interprets its arguments.
func Supervise(cmd *exec.Cmd) (func(), error) { return SuperviseWithCleanup(cmd, nil) }
func SuperviseWithCleanup(cmd *exec.Cmd, cleanupArgs []string) (func(), error) {
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
