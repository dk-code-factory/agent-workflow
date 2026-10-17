package process

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

func init() {
	if len(os.Args) < 6 || os.Args[1] != "__container_workload_guardian_v1" {
		return
	}
	if runtime.GOOS != "linux" || os.Getpid() != 1 || os.Getuid() == 0 || os.Args[4] != "--" {
		fmt.Fprintln(os.Stderr, "workload guardian requires a private Linux PID namespace")
		os.Exit(125)
	}
	writable, err := strconv.ParseBool(os.Getenv("AGENT_WORKFLOW_WORKSPACE_WRITABLE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "container workspace policy missing")
		os.Exit(125)
	}
	if err = CheckContainerMounts("/workspace", "/state", writable); err != nil {
		fmt.Fprintln(os.Stderr, "container mount prerequisites are unavailable")
		os.Exit(125)
	}
	if file := os.Getenv("AGENT_WORKFLOW_AUTH_FILE"); file != "" {
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "container credential file is not readable")
			os.Exit(125)
		}
		_ = f.Close()
	}
	code, err := RunContainerWorkload(context.Background(), os.Args[2], os.Args[3], os.Args[5:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "container lifetime or cleanup could not be confirmed")
		os.Exit(125)
	}
	os.Exit(code)
}

func CheckContainerMounts(workspace, state string, writable bool) error {
	if _, err := os.ReadDir(workspace); err != nil {
		return fmt.Errorf("workspace is not readable")
	}
	if info, err := os.Lstat(workspace + string(os.PathSeparator) + ".git"); err == nil && !info.IsDir() {
		return fmt.Errorf("shared Git administration is unsupported")
	}
	probe, err := os.CreateTemp(state, ".guardian-permission-*")
	if err != nil {
		return fmt.Errorf("provider state is not writable")
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	probe, err = os.CreateTemp(workspace, ".guardian-permission-*")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		if !writable {
			return fmt.Errorf("read-only workspace policy is not enforced")
		}
	} else if writable {
		return fmt.Errorf("workspace is not writable under selected UID mapping")
	}
	return nil
}

type lifetimeObservation struct {
	Token    string `json:"token"`
	Sequence uint64 `json:"sequence"`
}

func readLifetime(path, token string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(data) > 4096 {
		return 0, fmt.Errorf("invalid lifetime record")
	}
	var value lifetimeObservation
	if json.Unmarshal(data, &value) != nil || value.Token != token || value.Sequence == 0 {
		return 0, fmt.Errorf("invalid lifetime identity")
	}
	return value.Sequence, nil
}

// RunContainerWorkload requires a changing controller lifetime record before
// effects. A frozen record cannot start work after VM/runtime recovery. While
// running, the monotonic local deadline avoids host/VM wall-clock assumptions.
func RunContainerWorkload(ctx context.Context, path, token string, argv []string) (int, error) {
	return runContainerWorkload(ctx, path, token, argv, 3*time.Second, 100*time.Millisecond)
}
func runContainerWorkload(ctx context.Context, path, token string, argv []string, window, interval time.Duration) (int, error) {
	if len(argv) == 0 {
		return -1, fmt.Errorf("missing container workload")
	}
	initial, err := readLifetime(path, token)
	if err != nil {
		return -1, err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	last := initial
	for last == initial {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-deadline.C:
			return -1, fmt.Errorf("controller lifetime did not advance")
		case <-ticker.C:
			last, err = readLifetime(path, token)
			if err != nil {
				return -1, err
			}
		}
	}
	turn, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(turn, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	stop, err := Supervise(cmd)
	if err != nil {
		return -1, err
	}
	defer stop()
	if err = cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	lastChange := time.Now()
	for {
		select {
		case err = <-done:
			code := cmd.ProcessState.ExitCode()
			if code == 125 {
				return code, ErrCleanup
			}
			if code < 0 {
				return code, ErrCleanup
			}
			return code, nil
		case <-ctx.Done():
			cancel()
			stop()
			<-done
			return -1, ctx.Err()
		case <-ticker.C:
			sequence, readErr := readLifetime(path, token)
			if readErr == nil && sequence != last {
				last = sequence
				lastChange = time.Now()
			}
			if readErr != nil || time.Since(lastChange) > window {
				cancel()
				stop()
				<-done
				return -1, fmt.Errorf("controller lifetime ended")
			}
		}
	}
}
