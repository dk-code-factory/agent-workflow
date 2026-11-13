package main

import (
	"github.com/dk-code-factory/agent-workflow/internal/engine"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func workerManager(store *workflows.SQLiteStore, root string) *workflows.WorkerManager {
	return engine.WorkerManager(store, root)
}
