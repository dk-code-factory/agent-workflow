package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dkropachev/agent-workflow/pkg/clirunner"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func toolBinder(store *workflows.SQLiteStore, workers *workflows.WorkerManager, root string) workflows.ToolBinder {
	broker := &workflows.Broker{Store: store, Workers: workers}
	return func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		_, snapshot, err := store.Load(ctx, in.RunID)
		if err != nil {
			return nil, err
		}
		var cfg clirunner.Config
		if err = json.Unmarshal(snapshot.Roles, &cfg); err != nil {
			return nil, err
		}
		role, ok := cfg.Agents[in.Role]
		if !ok {
			return nil, fmt.Errorf("tool invocation role is outside admitted snapshot")
		}
		if in.Workspace != "" {
			role.Workspace = in.Workspace
			cfg.Agents[in.Role] = role
		}
		runner := &clirunner.Runner{Config: cfg, Workspace: root}
		grants, catalog := runner.ToolPolicy(in.Role)
		if in.Grants == nil {
			in.Grants = grants
		} else {
			for _, grant := range in.Grants {
				allowed := false
				for _, roleGrant := range grants {
					if roleGrant == grant {
						allowed = true
						break
					}
				}
				if !allowed {
					return nil, fmt.Errorf("worker grants exceed admitted role")
				}
			}
		}
		in.Catalog = catalog
		in.NativeTools = role.Tools
		in.Access, err = runner.WorkerAccess(in.Role)
		if err != nil {
			return nil, err
		}
		in.Limits, err = runner.WorkerLimits(in.Role)
		if err != nil {
			return nil, err
		}
		in.Dispatch = runner.ExternalTool
		if in.Secrets == nil {
			in.Secrets = map[string]string{}
		}
		for key, value := range runner.SecretValues() {
			in.Secrets[key] = value
		}
		return broker.Bind(ctx, in)
	}
}
