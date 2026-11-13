// Package engine connects the production provider, worker and broker adapters.
package engine

import (
	"encoding/json"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func Executor(cfg clirunner.Config, store *workflows.SQLiteStore, root string, parallel int) (*workflows.Executor, *workflows.WorkerManager) {
	roles, _ := json.Marshal(cfg)
	executor := &workflows.Executor{WorkspaceDir: root, Store: store, Agents: &clirunner.Runner{Config: cfg, Workspace: root}, Roles: roles, MaxParallel: parallel, CommandExecution: cfg.CommandExecutionPolicy()}
	executor.RoleRunner = func(raw json.RawMessage) (workflows.AgentRunner, error) {
		var pinned clirunner.Config
		if err := json.Unmarshal(raw, &pinned); err != nil {
			return nil, err
		}
		return &clirunner.Runner{Config: pinned, Workspace: root}, nil
	}
	workers := WorkerManager(store, root)
	binder := ToolBinder(store, workers, root)
	executor.Workers, executor.BindTools, workers.BindTools = workers, binder, binder
	return executor, workers
}
