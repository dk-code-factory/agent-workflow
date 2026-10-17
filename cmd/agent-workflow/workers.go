package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func workerManager(store *workflows.SQLiteStore, root string) *workflows.WorkerManager {
	return &workflows.WorkerManager{Store: store, Factory: func(ctx context.Context, run, role string, requested []string) (workflows.SessionRunner, []string, map[string]string, error) {
		_, snap, err := store.Load(ctx, run)
		if err != nil {
			return nil, nil, nil, err
		}
		var cfg clirunner.Config
		if err = json.Unmarshal(snap.Roles, &cfg); err != nil {
			return nil, nil, nil, err
		}
		agent, ok := cfg.Agents[role]
		if !ok {
			return nil, nil, nil, fmt.Errorf("role is outside admitted configuration")
		}
		if requested != nil {
			allowed := map[string]bool{}
			for _, tool := range agent.Tools {
				allowed[tool] = true
			}
			for _, tool := range requested {
				if !allowed[tool] {
					return nil, nil, nil, fmt.Errorf("tool %s exceeds role policy", tool)
				}
			}
			agent.Tools = requested
			cfg.Agents[role] = agent
		}
		secrets, err := controller.ResolveSecrets(snap.SecretRefs)
		if err != nil {
			return nil, nil, nil, err
		}
		runner := &clirunner.Runner{Config: cfg, Workspace: root}
		if err = runner.Preflight(ctx, []string{role}); err != nil {
			return nil, nil, nil, err
		}
		for key, value := range runner.SecretValues() {
			secrets[key] = value
		}
		return runner, agent.Tools, secrets, nil
	}}
}
