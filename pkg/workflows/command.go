package workflows

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

const MaxCommandOutput = 16 << 20

type cappedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := MaxCommandOutput - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (e *Executor) executeCommand(ctx context.Context, step Step, ec expressionContext, slots chan struct{}) (map[string]any, error) {
	argv := make([]string, 0, len(step.Argv))
	for _, a := range step.Argv {
		v, err := renderString(a, ec)
		if err != nil {
			return nil, err
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("argv values must render to strings")
		}
		argv = append(argv, s)
	}
	if step.Run != "" {
		v, err := renderString(step.Run, ec)
		if err != nil {
			return nil, err
		}
		script, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("run must render to a string")
		}
		argv = []string{step.Shell, "-c", script}
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("command requires executable")
	}
	if step.Timeout != "" {
		d, err := time.ParseDuration(step.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("command timeout must be a positive duration")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	queued := time.Now()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-slots }()
	queueTime := time.Since(queued).Milliseconds()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	dir := step.WorkingDirectory
	if dir == "" {
		dir = e.WorkspaceDir
	} else {
		v, err := renderString(dir, ec)
		if err != nil {
			return nil, err
		}
		var ok bool
		dir, ok = v.(string)
		if !ok {
			return nil, fmt.Errorf("working-directory must render to a string")
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(e.WorkspaceDir, dir)
		}
	}
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	var cleanup func()
	var superviseErr error
	if e.CommandExecution.Mode == "container" {
		binding, ok := runtimeexec.BindingFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("container commands require durable invocation ownership")
		}
		state := filepath.Join(binding.Dir, "providers", "commands", binding.RunID, binding.LogicalID)
		if err := os.MkdirAll(filepath.Join(state, "home"), 0700); err != nil {
			return nil, err
		}
		cleanup, superviseErr = runtimeexec.PrepareContainer(ctx, cmd, e.CommandExecution, runtimeexec.ContainerOptions{Workspace: dir, State: state, Root: e.WorkspaceDir})
	} else {
		cleanup, superviseErr = runtimeexec.Supervise(ctx, cmd)
	}
	if superviseErr != nil {
		return nil, superviseErr
	}
	defer cleanup()
	var stdout, stderr cappedOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if ec.Ordered {
		if err := e.iterationEvidence(ctx, RunRequest{runID: ec.RunID, Metadata: ec.Metadata}); err != nil {
			return nil, err
		}
	}
	err := cmd.Run()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	out := map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": code, "queue_time_ms": queueTime}
	if code == 125 || code < 0 && cmd.ProcessState != nil {
		return out, fmt.Errorf("%w: %v", process.ErrCleanup, err)
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return out, fmt.Errorf("command output exceeds 16 MiB")
	}
	if err != nil {
		return out, fmt.Errorf("command failed: %w", err)
	}
	return out, nil
}
