package clirunner

import (
	"context"
	"encoding/json"
	"fmt"

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
