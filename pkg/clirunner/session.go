package clirunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func (r *Runner) Capabilities(id string) (workflows.ProviderCapabilities, error) {
	a, ok := r.Config.Agents[id]
	if !ok {
		return workflows.ProviderCapabilities{}, fmt.Errorf("unknown agent %s", id)
	}
	switch a.Provider {
	case "claude-sdk":
		return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, StructuredOutput: true, ManagedPolicy: true}, nil
	case "codex-app-server":
		return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, StructuredOutput: true}, nil
	case "claude", "claude-cli", "codex", "codex-cli":
		return workflows.ProviderCapabilities{Interrupt: true, StructuredOutput: true}, nil
	default:
		return workflows.ProviderCapabilities{}, fmt.Errorf("unsupported provider %s", a.Provider)
	}
}
func (r *Runner) RunTurn(ctx context.Context, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	agent, ok := r.Config.Agents[req.AgentID]
	if !ok {
		return workflows.SessionResult{}, fmt.Errorf("unknown agent %s", req.AgentID)
	}
	agent.Execution.runID = req.RunID
	agent.Execution.invocationID = req.InvocationID
	agent.Execution.ownerRoot = r.Workspace
	turn, cancel, contextErr := turnContext(ctx, agent)
	if contextErr != nil {
		return workflows.SessionResult{}, contextErr
	}
	defer cancel()
	ctx = turn
	if agent.Provider == "codex-app-server" {
		result, err := r.codexTurn(ctx, agent, req, emit)
		if err != nil {
			return result, err
		}
		return limitResult(result, agent)
	}
	if agent.Provider != "claude-sdk" {
		if req.SessionID != "" {
			return workflows.SessionResult{}, fmt.Errorf("CLI compatibility adapter cannot resume provider sessions")
		}
		outputs, err := r.RunAgent(ctx, req.AgentRequest)
		status := workflows.RunStatusSucceeded
		if err != nil {
			status = workflows.RunStatusFailed
		}
		return workflows.SessionResult{Status: status, Outputs: outputs}, err
	}
	workspace := r.Workspace
	if agent.Workspace != "" {
		workspace = agent.Workspace
	}
	if req.Workspace != "" {
		workspace = req.Workspace
	}
	credential, ok := os.LookupEnv(agent.Authentication.CredentialEnv)
	if !ok || credential == "" {
		return workflows.SessionResult{}, fmt.Errorf("gateway credential reference is unavailable")
	}
	model := agent.Model
	if req.Model != "" {
		model = req.Model
	}
	var schema any
	if req.Output != nil {
		schema = req.Output.Schema
	}
	input := map[string]any{"session_id": req.SessionID, "prompt": req.Prompt, "workspace": workspace, "model": model, "instructions": agent.InstructionText, "tools": agent.Tools, "repair_only": req.RepairOnly, "schema": schema}
	if agent.Execution.Mode == "container" {
		input["workspace"] = "/workspace"
	}
	data, err := json.Marshal(input)
	if err != nil {
		return workflows.SessionResult{}, err
	}
	command := agent.Command
	if command == "" {
		command = "node"
	}
	cmd := exec.CommandContext(ctx, command, agent.Bridge)
	cmd.Dir = workspace
	cmd.Stdin = bytes.NewReader(data)
	// Gateway auth is explicit. The SDK receives an engine-owned config directory;
	// host user/project settings and native role/skill/plugin discovery are disabled.
	state := filepath.Join(r.Workspace, ".agent-workflow", "providers", "claude", req.AgentID)
	if err = os.MkdirAll(state, 0700); err != nil {
		return workflows.SessionResult{}, err
	}
	cmd.Env = providerEnvironment(agent.Authentication.CredentialEnv)
	cmd.Env = append(cmd.Env, "ANTHROPIC_BASE_URL="+agent.Authentication.Endpoint, "ANTHROPIC_API_KEY="+credential, "CLAUDE_CONFIG_DIR="+state, "MAX_STRUCTURED_OUTPUT_RETRIES=0", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return workflows.SessionResult{}, err
	}
	cleanup, err := superviseContainer(ctx, cmd, agent, workspace, state, true)
	if err != nil {
		return workflows.SessionResult{}, err
	}
	defer cleanup()
	if err = cmd.Start(); err != nil {
		return workflows.SessionResult{}, err
	}
	result, parseErr := parseBridge(stdout, emit)
	if parseErr == nil && req.SessionID != "" && result.SessionID != req.SessionID {
		parseErr = fmt.Errorf("provider returned an unexpected resumed session identity")
	}
	if parseErr != nil {
		cleanup()
	}
	waitErr := cmd.Wait()
	if cmd.ProcessState != nil && (cmd.ProcessState.ExitCode() == 125 || cmd.ProcessState.ExitCode() < 0) {
		return result, errors.Join(ctx.Err(), fmt.Errorf("%w: %v", process.ErrCleanup, waitErr))
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if parseErr != nil {
		return result, parseErr
	}
	if waitErr != nil {
		return result, fmt.Errorf("Claude SDK bridge: %w", waitErr)
	}
	if stderr.overflow {
		return result, fmt.Errorf("bridge diagnostics exceed limit")
	}
	if result.Status != workflows.RunStatusSucceeded {
		return result, fmt.Errorf("provider terminal status %s", result.Status)
	}
	if result.Outputs != nil {
		result.Outputs["provider"] = agent.Provider
		result.Outputs["provider_status"] = result.ProviderStatus
		result.Outputs["provider_version"] = result.ProviderVersion
		result.Outputs["session_id"] = result.SessionID
	}
	return limitResult(result, agent)
}
func parseBridge(reader io.Reader, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxOutput)
	var result workflows.SessionResult
	terminal := false
	total := 0
	for scanner.Scan() {
		total += len(scanner.Bytes())
		if total > maxOutput {
			return result, fmt.Errorf("bridge output exceeds limit")
		}
		var frame struct {
			Version         int            `json:"version"`
			Type            string         `json:"type"`
			SessionID       string         `json:"session_id"`
			Status          string         `json:"status"`
			ProviderStatus  string         `json:"provider_status"`
			ProviderVersion string         `json:"provider_version"`
			Outputs         map[string]any `json:"outputs"`
			Error           string         `json:"error"`
			Data            map[string]any `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return result, fmt.Errorf("invalid bridge frame: %w", err)
		}
		if frame.Version > 1 {
			return result, fmt.Errorf("unsupported bridge protocol version %d", frame.Version)
		}
		if frame.SessionID != "" {
			if result.SessionID != "" && result.SessionID != frame.SessionID {
				return result, fmt.Errorf("bridge changed session identity within a turn")
			}
			result.SessionID = frame.SessionID
		}
		if terminal {
			return result, fmt.Errorf("bridge emitted data after terminal status")
		}
		if frame.Type == "result" {
			terminal = true
			result.Status = frame.Status
			result.ProviderStatus = frame.ProviderStatus
			result.ProviderVersion = frame.ProviderVersion
			result.Outputs = frame.Outputs
			if frame.Status != workflows.RunStatusSucceeded {
				return result, fmt.Errorf("provider failed: %s", frame.Error)
			}
		} else if emit != nil {
			emit(workflows.ProviderEvent{Type: frame.Type, SessionID: frame.SessionID, Data: frame.Data})
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if !terminal {
		return result, fmt.Errorf("bridge returned no terminal status")
	}
	return result, nil
}

// Only explicit provider authentication is added by each session adapter.
// In particular GH_TOKEN, SSH_AUTH_SOCK and host tool configuration are excluded.
func providerEnvironment(extra string) []string {
	keys := []string{"PATH", "LANG", "LC_ALL", "TMPDIR", "SYSTEMROOT"}
	if extra != "" {
		keys = append(keys, extra)
	}
	out := []string{}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func stripJSONText(s string) string { return strings.TrimSpace(s) }

func (r *Runner) SecretValues() map[string]string {
	out := map[string]string{}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			out[key] = value
		}
	}
	for id, agent := range r.Config.Agents {
		if ref := agent.Authentication.CredentialEnv; ref != "" {
			if v, ok := os.LookupEnv(ref); ok {
				out["provider_"+id] = v
			}
		}
	}
	return out
}

func (r *Runner) WorkerLimits(id string) (workflows.WorkerLimits, error) {
	a, ok := r.Config.Agents[id]
	if !ok {
		return workflows.WorkerLimits{}, fmt.Errorf("unknown role %s", id)
	}
	limit := a.Limits
	var duration time.Duration
	var err error
	if limit.TurnTimeout != "" {
		duration, err = time.ParseDuration(limit.TurnTimeout)
		if err != nil {
			return workflows.WorkerLimits{}, err
		}
	}
	return workflows.WorkerLimits{MaxWorkers: limit.MaxWorkers, MaxDepth: limit.MaxDepth, MaxQueued: limit.MaxQueued, MaxTurns: limit.MaxTurns, TurnDuration: duration, MaxOutput: limit.MaxOutput}, nil
}

func nativeEnvironment() []string {
	blocked := map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true, "SSH_AUTH_SOCK": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true, "GIT_SSH_COMMAND": true}
	out := []string{}
	for _, assignment := range os.Environ() {
		key, _, _ := strings.Cut(assignment, "=")
		if !blocked[key] {
			out = append(out, assignment)
		}
	}
	return out
}

func (r *Runner) WorkerAccess(id string) (workflows.WorkerAccess, error) {
	a, ok := r.Config.Agents[id]
	if !ok {
		return workflows.WorkerAccess{}, fmt.Errorf("unknown role")
	}
	workspace := r.Workspace
	if a.Workspace != "" {
		workspace = a.Workspace
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return workflows.WorkerAccess{}, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return workflows.WorkerAccess{}, err
	}
	access := workflows.WorkerAccess{Container: a.Execution.Mode == "container", Writable: true, Workspace: real}
	if access.Container {
		access.Writable = a.Execution.Writable
	}
	return access, nil
}
