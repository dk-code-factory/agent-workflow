package main

import (
	"context"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func ownerLossScenario(ctx context.Context, binary, root, configuration, role, runtime, control string, report *evidence) (*workflows.Run, error) {
	ref, err := writeManagedWorkflow(root, role, "Use an approved shell tool to sleep for 60 seconds. Do not finish early.", false)
	if err != nil {
		return nil, err
	}
	command, err := startFixture(ctx, root, configuration)
	if err != nil {
		return nil, err
	}
	defer stopFixture(command)
	client := controller.NewClient(filepath.Join(root, ".agent-workflow"))
	var admitted workflows.Run
	if err = client.Call(ctx, "POST", "/runs", controller.StartRequest{Ref: ref}, &admitted); err != nil {
		return nil, err
	}
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
					usage, usageErr := view.Usage(ctx, id)
					if found && usageErr == nil && usage.Summary.InputTokens != nil && *usage.Summary.InputTokens > 0 {
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
	var original runtimeexec.Record
	records, err := store.Invocations(ctx, id, true)
	if err != nil || len(records) == 0 {
		return nil, fmt.Errorf("missing active invocation")
	}
	original = records[0]
	if control != "" {
		if err = exec.CommandContext(ctx, control, "stop").Run(); err != nil {
			return nil, err
		}
		if _, err = runtimeVersion(ctx, runtime); err == nil {
			return nil, fmt.Errorf("disposable runtime did not become unavailable")
		}
	}
	if err = store.Recover(ctx); err != nil {
		return nil, err
	}
	if control != "" {
		if err = store.ReconcileInvocations(ctx, id); err == nil {
			return nil, fmt.Errorf("runtime outage incorrectly confirmed cleanup")
		}
		if store.CheckInvocationCleanup(ctx, id) == nil {
			return nil, fmt.Errorf("runtime outage lost cleanup obligation")
		}
		if err = exec.CommandContext(ctx, control, "start").Run(); err != nil {
			return nil, err
		}
		if _, err = runtimeVersion(ctx, runtime); err != nil {
			return nil, err
		}
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
	if err = verifyStaleOwnership(ctx, store, original); err != nil {
		return nil, err
	}
	if control != "" {
		report.RuntimeRecovery = map[string]bool{"owner_loss": true, "runtime_unavailable": true, "runtime_restart": true, "cleanup_recovery": true, "stale_execution_ownership": true}
	}
	if err = collectObservations(root, report); err != nil {
		return nil, err
	}
	return run, nil
}

// Claim a new generation without dispatching a turn. A current intent is a
// positive control; old generation and old owner must each be refused.
func verifyStaleOwnership(ctx context.Context, store *workflows.SQLiteStore, original runtimeexec.Record) error {
	current, err := store.QueueResume(ctx, original.RunID, "conformance-stale-owner-check", true, 1)
	if err != nil {
		return err
	}
	current.Status = workflows.RunStatusRunning
	if err = store.Claim(ctx, current, workflows.RunStatusQueued); err != nil {
		return err
	}
	record := runtimeexec.Record{ID: runtimeexec.NewID(), RunID: current.ID, Owner: current.Owner, Generation: current.Generation, State: "intent", LogicalID: "stale-control"}
	stale := record
	stale.Generation = original.Generation
	if err = store.AdmitInvocation(ctx, stale); err == nil {
		return fmt.Errorf("stale generation admitted")
	}
	stale = record
	stale.Owner = "stale-owner"
	if err = store.AdmitInvocation(ctx, stale); err == nil {
		return fmt.Errorf("stale owner admitted")
	}
	if err = store.AdmitInvocation(ctx, record); err != nil {
		return fmt.Errorf("current owner positive control rejected: %w", err)
	}
	record.State = "cleaned"
	if err = store.UpdateInvocation(ctx, record); err != nil {
		return err
	}
	current.Status = workflows.RunStatusNeedsAttention
	current.FailureClass = "owner_loss"
	return store.Save(ctx, current)
}
