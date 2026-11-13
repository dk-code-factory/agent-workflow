package clirunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (r *Runner) Capabilities(id string) (workflows.ProviderCapabilities, error) {
	a, ok := r.Config.Agents[id]
	if !ok {
		return workflows.ProviderCapabilities{}, fmt.Errorf("unknown agent %s", id)
	}
	switch a.Provider {
	case "claude-sdk":
		if a.Execution.Mode != "container" {
			if err := verifySDKBridge(a); err != nil {
				return workflows.ProviderCapabilities{}, err
			}
		}
		return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, StructuredOutput: true, ManagedPolicy: true}, nil
	case "codex-app-server":
		return workflows.ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, StructuredOutput: true}, nil
	case "claude", "claude-cli", "codex", "codex-cli":
		return workflows.ProviderCapabilities{Interrupt: true, StructuredOutput: true}, nil
	default:
		return workflows.ProviderCapabilities{}, fmt.Errorf("unsupported provider %s", a.Provider)
	}
}

// ValidateConversationOutput rejects contracts the provider cannot transport.
func (r *Runner) ValidateConversationOutput(id string, schema map[string]any) error {
	agent, ok := r.Config.Agents[id]
	if !ok {
		return fmt.Errorf("unknown agent %s", id)
	}
	if agent.Provider != "claude-sdk" {
		return nil
	}
	var unsupported func(any) bool
	unsupported = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "__proto__" {
					return true
				}
				if unsupported(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if unsupported(child) {
					return true
				}
			}
		}
		return false
	}
	var requiresUnsupported func(map[string]any) bool
	requiresUnsupported = func(schema map[string]any) bool {
		if names, ok := schema["required"].([]any); ok {
			for _, name := range names {
				if name == "__proto__" {
					return true
				}
			}
		}
		if properties, ok := schema["properties"].(map[string]any); ok {
			for _, raw := range properties {
				if child, ok := raw.(map[string]any); ok && requiresUnsupported(child) {
					return true
				}
			}
		}
		for _, key := range []string{"items", "additionalProperties"} {
			if child, ok := schema[key].(map[string]any); ok && requiresUnsupported(child) {
				return true
			}
		}
		return false
	}
	if unsupported(schema) || requiresUnsupported(schema) {
		return fmt.Errorf("agent %s: Claude SDK conversation output cannot declare __proto__ properties", id)
	}
	return nil
}

func (r *Runner) RunTurn(ctx context.Context, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (result workflows.SessionResult, err error) {
	agent, ok := r.Config.Agents[req.AgentID]
	if !ok {
		return workflows.SessionResult{}, fmt.Errorf("unknown agent %s", req.AgentID)
	}
	if req.Definition != nil {
		if _, err := r.AdmitNativeTask(req.Definition); err != nil {
			return workflows.SessionResult{}, err
		}
		agent.InstructionText += "\n\n" + req.Definition.DeveloperInstructions
		if req.Definition.Model != "" {
			agent.Model = req.Definition.Model
		}
	}
	if req.Input != nil {
		req.Prompt, err = workflows.AgentTaskPrompt(req.Prompt, req.Input)
		if err != nil {
			return workflows.SessionResult{}, err
		}
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
	var observationErr error
	defer func() { err = errors.Join(err, observationErr) }()
	parentEmit := emit
	emit = func(event workflows.ProviderEvent) {
		if event.Type == "usage" && req.ObserveUsage != nil {
			usage, _ := event.Data["usage"].(map[string]any)
			key, _ := event.Data["observation_key"].(string)
			provider, model := r.UsageProvider(req.AgentID, req.Model)
			if recordErr := req.ObserveUsage(workflows.UsageUpdate{Key: key, Provider: provider, Model: model, SessionID: event.SessionID, Usage: usage, Incomplete: true}); recordErr != nil {
				observationErr = errors.Join(observationErr, recordErr)
				cancel()
			}
		}
		if parentEmit != nil {
			parentEmit(event)
		}
	}
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
	providerEnv, err := claudeSDKEnvironment(agent.Authentication)
	if err != nil {
		return workflows.SessionResult{}, err
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
	if req.Tools != nil && req.Tools.Binding != nil && !req.RepairOnly {
		catalog := []map[string]any{}
		for _, d := range req.Tools.Binding.Catalog() {
			catalog = append(catalog, map[string]any{"server": d.Server, "name": d.Name, "description": d.Description, "input_schema": d.InputSchema, "wire_name": workflows.ToolWireName(d), "controller_validation": d.ControllerValidation})
		}
		input["broker_tools"] = catalog
	}
	command := agent.Command
	if command == "" {
		command = "node"
	}
	cmd := exec.CommandContext(ctx, command, agent.Bridge)
	cmd.Dir = workspace
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workflows.SessionResult{}, err
	}
	defer stdin.Close()
	writer := &bridgeWriter{writer: stdin}
	// Authentication is explicit. The SDK receives an engine-owned config directory;
	// host user/project settings and native role/skill/plugin discovery are disabled.
	state, err := providerStateDir(r.Workspace, "claude", req)
	if err != nil {
		return workflows.SessionResult{}, err
	}
	cmd.Env = append(providerEnv, "CLAUDE_CONFIG_DIR="+state, "HOME="+filepath.Join(state, "home"), "MAX_STRUCTURED_OUTPUT_RETRIES=0", "ENABLE_CLAUDEAI_MCP_SERVERS=false")
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
	// Cancellation first requests SDK interruption. The guardian remains the
	// five-second fallback and still observes controller owner loss directly.
	var interruptTimer *time.Timer
	var interruptMu sync.Mutex
	cmd.Cancel = func() error {
		interruptMu.Lock()
		if interruptTimer == nil {
			interruptTimer = time.AfterFunc(5*time.Second, cleanup)
		}
		interruptMu.Unlock()
		go func() { _ = writer.send(map[string]any{"type": "interrupt"}) }()
		return nil
	}
	defer func() {
		interruptMu.Lock()
		if interruptTimer != nil {
			interruptTimer.Stop()
		}
		interruptMu.Unlock()
	}()
	if err = cmd.Start(); err != nil {
		return workflows.SessionResult{}, err
	}

	invocationTools := req.Tools
	if req.RepairOnly {
		invocationTools = nil
	}
	result, parseErr := bridgeTurn(ctx, stdout, writer, input, invocationTools, emit, cleanup)
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
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
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
	for id, agent := range r.Config.Agents {
		if agent.Provider == "codex-app-server" && agent.Authentication.Mode == "native" {
			if login, err := nativeCodexCredentials(agent); err == nil {
				for key, value := range login {
					if key != "type" {
						if value, ok := value.(string); ok {
							out["native_"+id+"_"+key] = value
						}
					}
				}
			}
		}
	}
	for name, server := range r.Config.MCPServers {
		for key, ref := range server.Environment {
			if value, ok := os.LookupEnv(ref); ok {
				out["mcp_"+name+"_"+key] = value
			}
		}
		for key, ref := range server.Headers {
			if value, ok := os.LookupEnv(ref); ok {
				out["mcp_"+name+"_"+key] = value
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

// Compatibility subprocesses receive only their own provider credential route.
func nativeProviderEnvironment(agent Agent) []string {
	out := providerEnvironment(agent.Authentication.CredentialEnv)
	keys := []string{"HOME", "USER", "LOGNAME", "SHELL", "TERM", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"}
	if agent.Provider == "claude" || agent.Provider == "claude-cli" {
		keys = append(keys, "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL")
	} else {
		keys = append(keys, "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL")
	}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
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
	access.Cleanup = "process-group"
	if access.Container || runtime.GOOS == "linux" {
		access.Cleanup = "strict"
	}
	if a.Provider == "codex-app-server" && (a.Sandbox == "" || a.Sandbox == "read-only") {
		access.Writable = false
	}
	if access.Container {
		access.Writable = a.Execution.Writable
	}
	return access, nil
}

func (r *Runner) WithWorkerWorkspace(role, workspace string) (workflows.SessionRunner, error) {
	access, err := r.WorkerAccess(role)
	if err != nil {
		return nil, err
	}
	target, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(access.Workspace, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("child workspace exceeds role workspace scope")
	}
	cfg := r.Config
	cfg.Agents = make(map[string]Agent, len(r.Config.Agents))
	for id, a := range r.Config.Agents {
		cfg.Agents[id] = a
	}
	a := cfg.Agents[role]
	a.Workspace = target
	cfg.Agents[role] = a
	return &Runner{Config: cfg, Workspace: r.Workspace}, nil
}

// AdmitNativeTask resolves behavior against the admitted codex runtime. It does
// not add managed tools, repair support, authentication or execution authority.
func (r *Runner) AdmitNativeTask(definition *workflows.AgentDefinition) (*workflows.AgentDefinition, error) {
	role, ok := r.Config.Agents["codex"]
	if !ok || role.Provider != "codex-app-server" {
		return nil, fmt.Errorf("agent/run requires codex runtime provider codex-app-server")
	}
	if role.Tools != nil || len(role.MCPGrants) > 0 {
		return nil, fmt.Errorf("Codex managed tool subsets are not certified")
	}
	result := *definition
	if result.Model == "" {
		result.Model = role.Model
	}
	return &result, nil
}
