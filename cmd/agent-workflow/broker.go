package main

import (
	"github.com/dk-code-factory/agent-workflow/internal/engine"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func toolBinder(store *workflows.SQLiteStore, workers *workflows.WorkerManager, root string) workflows.ToolBinder {
	return engine.ToolBinder(store, workers, root)
}
