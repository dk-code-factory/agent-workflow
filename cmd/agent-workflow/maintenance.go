package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dkropachev/agent-workflow/internal/ownership"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func databaseMaintenance(ctx context.Context, dir, operation, path string) (map[string]any, error) {
	if path == "" {
		return nil, fmt.Errorf("%s requires a database file path", operation)
	}
	owner, err := ownership.Own(dir)
	if err != nil {
		return nil, fmt.Errorf("%s requires a stopped controller and exclusive workspace ownership: %w", operation, err)
	}
	defer owner.Close()
	if operation == "restore" {
		if _, statErr := os.Stat(filepath.Join(dir, "state.db")); statErr == nil {
			current, openErr := workflows.OpenSQLite(dir)
			if openErr != nil {
				return nil, openErr
			}
			if err = current.Migrate(ctx, owner); err == nil {
				err = current.Recover(ctx)
			}
			if err == nil {
				err = current.ReconcileInvocations(ctx, "")
			}
			if err == nil {
				err = current.CheckInvocationCleanup(ctx, "")
			}
			_ = current.Close()
			if err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		retained, err := workflows.RestoreDatabase(ctx, dir, path, owner)
		if err != nil {
			return nil, err
		}
		return map[string]any{"restored": path, "retained_database": retained, "recovery": "restored runnable work requires explicit uncertain-retry acknowledgement"}, nil
	}
	if _, err = os.Stat(filepath.Join(dir, "state.db")); err != nil {
		return nil, err
	}
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if operation == "bundle" {
		if err = store.Migrate(ctx, owner); err != nil {
			return nil, err
		}
		if err = store.RecoveryBundle(ctx, path, owner); err != nil {
			return nil, err
		}
		return map[string]any{"bundle": path, "scope": "engine-owned recovery data; native credential stores excluded"}, nil
	}
	if err = store.Backup(ctx, path, owner); err != nil {
		return nil, err
	}
	return map[string]any{"database": path, "scope": "database-only; managed workspaces and provider state are separate"}, nil
}
