package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const RunStatusQueued = "queued"

// Prepare validates and persists a queued run before acknowledging admission.
// Controller admission resolves credentials separately from the snapshot.
func (e *Executor) Prepare(ctx context.Context, req RunRequest) (*Run, error) {
	copy := *e
	e = &copy
	req.Execution = e.CommandExecution
	if source, ok := e.Agents.(interface {
		AdmittedRoles(context.Context) (json.RawMessage, error)
	}); ok {
		roles, err := source.AdmittedRoles(ctx)
		if err != nil {
			return nil, err
		}
		e.Roles = roles
	}
	journal, ok := e.Store.(Journal)
	if !ok {
		return nil, fmt.Errorf("durable store required")
	}
	all, err := e.Load(req.Ref)
	if err != nil {
		return nil, err
	}
	if err = e.preflight(ctx, all); err != nil {
		return nil, err
	}
	if e.Actions != nil {
		req.Metadata, err = e.Actions.Admission(ctx, all)
		if err != nil {
			return nil, err
		}
	}
	req.actionContracts = e.actionContracts
	ref, _ := CanonicalLocalRef(req.Ref)
	if _, err = resolveInputs(all[ref].On.WorkflowCall, req.Inputs, req.Secrets); err != nil {
		return nil, err
	}
	runID := randomID()
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["origin_run_id"] = runID
	snap, err := snapshotFor(all, req, e.Roles, e.MaxParallel)
	if err != nil {
		return nil, err
	}
	run := &Run{OriginID: runID, Version: 1, ID: runID, WorkflowRef: ref, Status: RunStatusQueued, StartedAt: time.Now().UTC(), Jobs: map[string]JobExecution{}}
	return run, (redactedStore{Journal: journal, secrets: e.redactions(req.Secrets)}).Admit(ctx, run, snap)
}
