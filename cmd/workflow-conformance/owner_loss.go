package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dkropachev/agent-workflow/internal/ownership"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func ownerLossScenario(ctx context.Context, binary, root, configuration, role string) (*workflows.Run, error) {
	ref, err := writeWorkflow(root, role, "Use an approved shell tool to sleep for 60 seconds. Do not finish early.", false)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, binary, "run", "--foreground", "--parallel", "1", "--workspace", root, "--config", configuration, ref)
	if err = command.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	state := filepath.Join(root, ".agent-workflow")
	var id string
	for {
		view, openErr := workflows.OpenSQLiteInspection(state)
		if openErr == nil {
			runs, listErr := view.List(ctx)
			if listErr == nil && len(runs) == 1 {
				id = runs[0].ID
				records, recordErr := view.Invocations(ctx, id, true)
				if recordErr == nil {
					found := false
					for _, record := range records {
						if record.Container != nil && record.Container.ID != "" && record.State == "launching" {
							found = true
						}
					}
					if found {
						_ = view.Close()
						break
					}
				}
			}
			_ = view.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	// Kill only the process this scenario just launched and still owns. Cleanup
	// reconciliation uses durable labels/IDs, never this transient host PID.
	if err = command.Process.Kill(); err != nil {
		return nil, err
	}
	_ = command.Wait()
	owner, err := ownership.Own(state)
	if err != nil {
		return nil, err
	}
	defer owner.Close()
	store, err := workflows.OpenSQLite(state)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if err = store.Recover(ctx); err != nil {
		return nil, err
	}
	if err = store.ReconcileInvocations(ctx, id); err != nil {
		return nil, err
	}
	if err = store.CheckInvocationCleanup(ctx, id); err != nil {
		return nil, err
	}
	run, _, err := store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.Status != workflows.RunStatusNeedsAttention {
		return nil, fmt.Errorf("owner loss was not quarantined")
	}
	usage, err := store.Usage(ctx, id)
	if err != nil {
		return nil, err
	}
	run.Usage = &usage.Summary
	return run, nil
}
