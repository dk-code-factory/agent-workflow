package workflows

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

func reusableChildID(parent, job string, iteration int) string {
	hash := sha256.Sum256([]byte(parent + "/" + job + fmt.Sprint(iteration)))
	return hex.EncodeToString(hash[:16])
}

// inputSuspension proves a cancellation-only suspension from the persisted
// physical child closure. Reusable calls do not carry iteration_ancestors, so
// follow their admitted job/iteration identity as well as ordered item children.
// This is execution evidence, not permission to replay uncertain effects.
func (e *Executor) inputSuspension(ctx context.Context, child *Run, runErr error, req RunRequest) (bool, error) {
	if child == nil || child.Status == RunStatusSucceeded || (runErr != nil && !cancellationOnly(runErr)) {
		return false, nil
	}
	journal := externalInputJournal(e.Store)
	if journal == nil {
		return false, nil
	}
	origin := nativeString(req.Metadata, "origin_run_id")
	if origin == "" {
		origin = req.runID
	}
	gates, err := journal.Inputs(ctx, origin)
	if err != nil {
		return false, err
	}
	control, err := journal.InputControl(ctx, origin)
	if err != nil {
		return false, err
	}
	type gateKey struct {
		run, job, step string
		iteration      int
	}
	correlated := map[gateKey]ExternalInputGate{}
	byRun := map[string][]ExternalInputGate{}
	for _, gate := range gates {
		if gate.OriginID != origin || gate.Fingerprint != inputFingerprint(gate) {
			return false, fmt.Errorf("%w: retained input request changed", ErrActionIntegrity)
		}
		correlated[gateKey{gate.RunID, gate.JobID, gate.StepID, gate.Iteration}] = gate
		byRun[gate.RunID] = append(byRun[gate.RunID], gate)
	}
	found := false
	seen := map[string]bool{}
	var visit func(string) (bool, error)
	visit = func(id string) (bool, error) {
		if seen[id] {
			return false, fmt.Errorf("%w: recursive suspended child", ErrActionIntegrity)
		}
		seen[id] = true
		run, snapshot, err := journal.Load(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch run.Status {
		case RunStatusSucceeded, RunStatusWaiting, RunStatusInterrupted:
		case RunStatusCanceled:
			if run.FailureClass != "canceled" && run.FailureClass != "" {
				return false, nil
			}
		case RunStatusNeedsAttention:
			// Older ordered wrappers escalated a safe descendant cancellation. Only
			// structural proof below can recover those observations; leaf errors cannot.
			if run.FailureClass != "owner_loss" && run.FailureClass != "iteration_blocked" {
				return false, nil
			}
		default:
			return false, nil
		}
		workflow := snapshot.Definitions[run.WorkflowRef]
		if workflow == nil {
			return false, fmt.Errorf("%w: suspended child definition missing", ErrActionIntegrity)
		}
		// Index each declared step once. The same index validates committed
		// gates and retained observations, including implicit step IDs.
		steps := map[string]map[string]Step{}
		for jobID, definition := range workflow.Jobs {
			steps[jobID] = map[string]Step{}
			for index, step := range definition.Steps {
				steps[jobID][stepID(step, index)] = step
			}
		}
		// A process may stop after the gate transaction and before publishing
		// any job result. The committed request is evidence of that input,
		// never evidence that preceding physical effects completed.
		for _, gate := range byRun[run.ID] {
			definition, ok := workflow.Jobs[gate.JobID]
			step, input := steps[gate.JobID][gate.StepID]
			limit := 1
			if definition.Repeat != nil {
				limit = definition.Repeat.MaxAttempts
			}
			validState := gate.State == "pending" || gate.State == "accepted" || gate.State == "consumed" || (gate.State == "canceled" && control.Canceled)
			if !ok || !input || step.Uses != "input/wait" || gate.Iteration < 1 || gate.Iteration > limit || gate.ID != actionHash([]any{run.ID, gate.JobID, gate.StepID, gate.Iteration}) || !validState {
				return false, nil
			}
			if _, observed := run.Jobs[gate.JobID]; !observed {
				inputs, err := resolveInputs(workflow.On.WorkflowCall, snapshot.Inputs, req.Secrets)
				if err != nil {
					return false, err
				}
				needs := map[string]JobExecution{}
				for _, dep := range definition.Needs {
					needs[dep] = run.Jobs[dep]
				}
				args, err := renderMap(step.With, expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: snapshot.Event, Needs: needs, Metadata: snapshot.Metadata, Iteration: map[string]any{"number": gate.Iteration}})
				if err != nil {
					return false, err
				}
				raw, err := e.actionIdentityJSON(mustInputJSON(args))
				if err != nil {
					return false, err
				}
				if err := decodeJSONWithNumbers(raw, &args); err != nil {
					return false, err
				}
				expected := gate
				expected.Prompt, _ = args["prompt"].(string)
				expected.EvidenceID, _ = args["evidence-id"].(string)
				expected.Payload = nil
				if payload, ok := args["payload"]; ok {
					expected.Payload, err = actionJSON(payload)
					if err != nil {
						return false, err
					}
				}
				if expected.Prompt == "" || expected.EvidenceID == "" || inputFingerprint(expected) != gate.Fingerprint {
					return false, nil
				}
				found = true
			}
		}
		// Descendant admission is durable before the wrapper job result. Follow
		// only children that were actually admitted; never create a replacement.
		for jobID, definition := range workflow.Jobs {
			if _, observed := run.Jobs[jobID]; observed || definition.Uses == "" {
				continue
			}
			if definition.ForEach != nil {
				iterations := iterationJournal(e.Store)
				if iterations == nil {
					return false, nil
				}
				checkpoint, err := iterations.IterationRun(ctx, run.ID, nativeString(snapshot.Metadata, "iteration_logical_run_id"), jobID)
				if err != nil {
					return false, err
				}
				records, err := iterations.IterationAttempts(ctx, checkpoint, jobID)
				if err != nil {
					return false, err
				}
				latest := map[string]IterationItemResult{}
				for _, record := range records {
					latest[record.Key] = record
				}
				for _, record := range latest {
					safe, err := visit(record.ChildID)
					if err != nil || !safe {
						return false, err
					}
				}
				continue
			}
			limit := 1
			if definition.Repeat != nil {
				limit = definition.Repeat.MaxAttempts
			}
			for number := 1; number <= limit; number++ {
				id := reusableChildID(run.ID, jobID, number)
				_, _, err := journal.Load(ctx, id)
				if errors.Is(err, sql.ErrNoRows) {
					break
				}
				if err != nil {
					return false, err
				}
				safe, err := visit(id)
				if err != nil || !safe {
					return false, err
				}
			}
		}
		for jobID, result := range run.Jobs {
			definition, ok := workflow.Jobs[jobID]
			if !ok {
				return false, fmt.Errorf("%w: suspended child job changed", ErrActionIntegrity)
			}
			switch result.Status {
			case RunStatusSucceeded, RunStatusSkipped, RunStatusWaiting, RunStatusInterrupted, RunStatusCanceled, RunStatusNeedsAttention:
			default:
				return false, nil
			}
			switch result.FailureClass {
			case "", "canceled", "iteration_blocked", "owner_loss", "needs_attention":
			default:
				return false, nil
			}
			if definition.ForEach != nil {
				if len(result.Items) == 0 && result.Status != RunStatusSucceeded && result.Status != RunStatusSkipped {
					return false, nil
				}
				for _, item := range result.Items {
					if item.Attempt == 0 && item.Status == "pending" {
						continue
					}
					if item.Status != "completed" && item.Status != "waiting" && item.Status != "interrupted" {
						return false, nil
					}
					safe, err := visit(item.ChildID)
					if err != nil || !safe {
						return false, err
					}
				}
				continue
			}
			if definition.Uses != "" {
				iterations := result.Iterations
				if len(iterations) == 0 {
					iterations = []JobIteration{{Number: 1, Status: result.Status}}
				}
				if result.ChildRunID != "" && result.ChildRunID != reusableChildID(run.ID, jobID, iterations[len(iterations)-1].Number) {
					return false, fmt.Errorf("%w: suspended reusable child changed", ErrActionIntegrity)
				}
				for _, iteration := range iterations {
					if iteration.Status == RunStatusSkipped {
						continue
					}
					safe, err := visit(reusableChildID(run.ID, jobID, iteration.Number))
					if err != nil || !safe {
						return false, err
					}
				}
				continue
			}
			if result.Status == RunStatusNeedsAttention || result.Status == RunStatusInterrupted || (result.Status == RunStatusCanceled && len(result.Steps) == 0) {
				return false, nil
			}
			iterations := result.Iterations
			if len(iterations) == 0 {
				iterations = []JobIteration{{Number: 1, Steps: result.Steps}}
			}
			for _, iteration := range iterations {
				for sid, step := range iteration.Steps {
					if step.Status == RunStatusSkipped {
						continue
					}
					definition, declared := steps[jobID][sid]
					input := declared && definition.Uses == "input/wait"
					if !declared {
						return false, nil
					}
					if step.Status == RunStatusSucceeded && !input {
						continue
					}
					if !input || (step.Status != RunStatusSucceeded && step.Status != RunStatusWaiting && (step.Status != RunStatusCanceled || step.FailureClass != "canceled")) {
						return false, nil
					}
					gate, ok := correlated[gateKey{run.ID, jobID, sid, iteration.Number}]
					validState := gate.State == "pending" || gate.State == "accepted" || gate.State == "consumed" || (gate.State == "canceled" && control.Canceled)
					if !ok || gate.ID != actionHash([]any{run.ID, jobID, sid, iteration.Number}) || !validState {
						return false, nil
					}
					// Consumption can commit before cancellation prevents the next
					// step from starting. Its completed input checkpoint remains
					// durable continuation evidence, with no ordinary effect replay.
					if step.Status == RunStatusSucceeded && gate.State != "consumed" {
						return false, nil
					}
					found = true
				}
			}
		}
		return true, nil
	}
	safe, err := visit(child.ID)
	if err != nil || !safe || !found {
		return false, err
	}

	return true, nil
}
