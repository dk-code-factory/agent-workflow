package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// SkillTaskPreflight checks effective native controls before workflow admission
// or recovery claims. It sends no credentials, login, thread or model request.
// The application-supplied executable remains trusted process authority.
func (r *Runner) SkillTaskPreflight(ctx context.Context, plan *workflows.SkillInvocation, workspace string) (returnErr error) {
	role, err := r.nativeTaskRuntime()
	if err != nil {
		return err
	}
	if role.Provider != "codex-app-server" {
		return nil
	}
	if plan == nil || plan.Profile != codexSkillProfile || len(role.Args) > 0 || role.Execution.Mode == "container" {
		return fmt.Errorf("unsupported selected-skill native control preflight")
	}
	if !filepath.IsAbs(workspace) || plan.Workspace != "" && workspace != plan.Workspace {
		return fmt.Errorf("selected-skill preflight requires pinned absolute workspace")
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state, err := os.MkdirTemp("", "agent-workflow-skill-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	home := filepath.Join(state, "home")
	if err = os.Mkdir(home, 0700); err != nil {
		return err
	}
	command := role.Command
	if command == "" {
		command = "codex"
	}
	cmd := exec.CommandContext(probe, command, codexSessionArgs(true)...)
	cmd.Dir = workspace
	cmd.Env = append(providerEnvironment(""), "CODEX_HOME="+state, "HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var diagnostics boundedBuffer
	cmd.Stderr = &diagnostics
	cleanup, err := superviseContainer(probe, cmd, role, workspace, state, false)
	if err != nil {
		return err
	}
	defer cleanup()
	if err = cmd.Start(); err != nil {
		return err
	}
	rpc := newAppRPC(probe, stdout, stdin, func(context.Context, rpcFrame) (any, error) {
		cancel()
		return nil, fmt.Errorf("control preflight denies native interactive request")
	}, nil)
	defer func() {
		cleanup()
		rpc.Close()
		waitErr := cmd.Wait()
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 125 {
			returnErr = errors.Join(returnErr, process.ErrCleanup, waitErr)
		}
		if probe.Err() != nil {
			returnErr = errors.Join(returnErr, probe.Err())
		}
	}()
	raw, err := rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "agent-workflow-skill-preflight", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err != nil {
		return err
	}
	var info struct {
		UserAgent string `json:"userAgent"`
	}
	if json.Unmarshal(raw, &info) != nil {
		return fmt.Errorf("selected-skill control preflight omitted native identity")
	}
	if err = verifyCodexHandshake(info.UserAgent); err != nil {
		return err
	}
	if err = rpc.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	raw, err = rpc.call("config/read", map[string]any{"cwd": workspace, "includeLayers": true})
	if err != nil {
		return fmt.Errorf("selected-skill preclaim control readback: %w", err)
	}
	return verifyCodexSkillRetryConfig(raw, role.Command != "")
}
