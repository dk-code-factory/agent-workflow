package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (c Config) PinRuntimes(ctx context.Context) (Config, error) {
	cfg := c
	cfg.Agents = make(map[string]Agent, len(c.Agents))
	global, err := runtimeexec.PinPolicy(ctx, c.Execution.policy())
	if err != nil {
		return cfg, err
	}
	cfg.Execution.Network = global.Network
	for id, a := range c.Agents {
		if global.NetworkRuntime != "" {
			if a.Execution.NetworkRuntime != "" && a.Execution.NetworkRuntime != global.NetworkRuntime {
				return cfg, fmt.Errorf("agent %s cannot override global network-runtime", id)
			}
			a.Execution.NetworkRuntime = global.NetworkRuntime
			a.Execution.Network = global.Network
		}
		p, err := runtimeexec.PinPolicy(ctx, a.Execution.policy())
		if err != nil {
			return cfg, err
		}
		a.Execution.Network = p.Network
		cfg.Agents[id] = a
	}
	return cfg, nil
}
func (r *Runner) RunnerForRoles(raw json.RawMessage) (workflows.AgentRunner, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return &Runner{Config: cfg, Workspace: r.Workspace}, nil
}

// Codex's explicit shell environment overrides retain the selected endpoint.
// These are execution settings, never credentials or additional
// approval/sandbox authority.
func networkShellArgs(e Execution) []string {
	if e.Network == nil || e.Mode == "container" {
		return nil
	}
	// Codex applies CLI overrides in order. Override individual keys after the
	// caller's args so a configured set table retains its unrelated values.
	return []string{
		"--config", "shell_environment_policy.set.DOCKER_HOST=" + strconv.Quote(e.Network.Endpoint),
		"--config", "shell_environment_policy.set.CONTAINER_HOST=" + strconv.Quote(e.Network.Endpoint),
		"--config", "shell_environment_policy.set.AGENT_WORKFLOW_NETWORK_RUNTIME=" + strconv.Quote(e.Network.Name),
	}
}

// providerArgs retains explicit provider configuration before enforcing the
// admitted endpoint keys. Other settings, including authority, remain explicit.
func providerArgs(agent Agent) []string {
	args := append([]string(nil), agent.Args...)
	if agent.Provider == "codex" || agent.Provider == "codex-cli" || agent.Provider == "codex-app-server" {
		args = append(args, networkShellArgs(agent.Execution)...)
	}
	return args
}
