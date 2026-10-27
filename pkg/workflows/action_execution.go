package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

var ErrActionIntegrity = errors.New("registered action recovery requires attention")

func actionFailure(ac ActionContext, err error) error {
	return fmt.Errorf("%w: run %s job %s step %s iteration %d: %v; start a new run for contract/binding/evidence drift", ErrActionIntegrity, ac.RunID, ac.JobID, ac.StepID, ac.Iteration, err)
}

func actionJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err = decodeJSONWithNumbers(b, &normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func validateActionJSON(raw json.RawMessage, schema *jsonschema.Resolved) error {
	var value any
	if err := decodeJSONWithNumbers(raw, &value); err != nil {
		return err
	}
	v, err := toolValidationValue(value)
	if err != nil {
		return err
	}
	return schema.Validate(v)
}

func (e *Executor) safeActionJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := decodeJSONWithNumbers(raw, &v); err != nil {
		return nil, err
	}
	return actionJSON(redactValue(v, e.redactionValues))
}

// Input and bindings must round-trip exactly. Persisting redacted replacements
// here would make later validators/reconciliation act on different identities.
func (e *Executor) actionIdentityJSON(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := actionJSON(raw)
	if err != nil {
		return nil, err
	}
	safe, err := e.safeActionJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(safe) == 0 || bytes.Equal(safe, []byte("null")) {
		return nil, fmt.Errorf("input/bindings must be non-null JSON")
	}
	if !bytes.Equal(canonical, safe) {
		return nil, fmt.Errorf("action input/bindings contain credential literals; use application-owned credential references")
	}
	return safe, nil
}

func (e *Executor) safeActionResult(v ActionValue) (ActionValue, error) {
	var err error
	v.Output, err = e.safeActionJSON(v.Output)
	if err != nil {
		return ActionValue{}, err
	}
	v.Evidence, err = e.safeActionJSON(v.Evidence)
	if err != nil {
		return ActionValue{}, err
	}
	v.Receipt, err = e.safeActionJSON(v.Receipt)
	return v, err
}

func (a *registeredAction) validateResult(ctx context.Context, r ActionAttempt) error {
	if err := validateActionJSON(r.Result.Output, a.output); err != nil {
		return fmt.Errorf("output schema: %w", err)
	}
	return a.validate(ctx, r.Context, r.Input, r.Bindings, r.Result)
}

func (e *Executor) validateRetainedAction(ctx context.Context, r ActionAttempt) error {
	a := e.applicationActions[r.Contract.Name]
	if a == nil || actionHash(a.contract) != actionHash(r.Contract) {
		return fmt.Errorf("implementation/schema/effect contract changed for action/%s", r.Contract.Name)
	}
	if actionHash(r.Input) != r.InputFingerprint || actionHash(r.Bindings) != r.BindingsFingerprint {
		return fmt.Errorf("retained input/bindings fingerprint mismatch")
	}
	if err := validateActionJSON(r.Input, a.input); err != nil {
		return fmt.Errorf("input schema: %w", err)
	}
	bindings, err := a.bindings(ctx, r.Context, r.Input)
	if err != nil {
		return fmt.Errorf("capture current bindings: %w", err)
	}
	bindings, err = e.actionIdentityJSON(bindings)
	if err != nil {
		return err
	}
	if actionHash(bindings) != r.BindingsFingerprint {
		return fmt.Errorf("application bindings changed")
	}
	if r.State == "completed" {
		return a.validateResult(ctx, r)
	}
	return nil
}

// This runs before successful job/child shortcuts. It covers actions in all
// retained repeat iterations and descendants, even when their job is skipped.
func (e *Executor) validateActionRecovery(ctx context.Context, run string, snap Snapshot) error {
	if actionHash(e.actionContracts) != actionHash(snap.ActionContracts) && !(len(e.actionContracts) == 0 && len(snap.ActionContracts) == 0) {
		for _, w := range snap.Definitions {
			for job, j := range w.Jobs {
				for i, step := range j.Steps {
					name := strings.TrimPrefix(step.Uses, "action/")
					if isApplicationAction(step.Uses) && actionHash(e.actionContracts[name]) != actionHash(snap.ActionContracts[name]) {
						return actionFailure(ActionContext{RunID: run, JobID: job, StepID: stepID(step, i)}, fmt.Errorf("action/%s admitted implementation/schema/effect manifest changed", name))
					}
				}
			}
		}
		return fmt.Errorf("%w: admitted application action manifest differs from current implementation/schema/effect; start a new run", ErrActionIntegrity)
	}
	journal := actionJournal(e.Store)
	if journal == nil {
		return nil
	} // Legacy workflows need no extension.
	records, err := journal.ActionAttempts(ctx, run)
	if err != nil {
		return err
	}
	latest := map[string]ActionAttempt{}
	for _, r := range records {
		latest[r.Context.OperationKey] = r
	}
	for _, r := range latest {
		if err := e.validateRetainedAction(ctx, r); err != nil {
			return actionFailure(r.Context, err)
		}
	}
	return nil
}

func (e *Executor) checkpointAction(ctx context.Context, j ActionJournal, r *ActionAttempt) error {
	r.Revision++
	return j.CheckpointAction(context.WithoutCancel(ctx), *r)
}

func completeAction(r *ActionAttempt) (StepExecution, error) {
	var outputs map[string]any
	if err := decodeJSONWithNumbers(r.Result.Output, &outputs); err != nil {
		return StepExecution{}, err
	}
	result := StepExecution{ID: r.Context.StepID, Status: RunStatusSucceeded, AttemptID: r.Attempt.ID, StartedAt: r.Attempt.StartedAt, FinishedAt: time.Now().UTC(), Outputs: outputs}
	r.State = "completed"
	r.Attempt.Status = RunStatusSucceeded
	r.Attempt.Result = result
	r.Attempt.FinishedAt = result.FinishedAt
	return result, nil
}

func (e *Executor) executeApplicationAction(ctx context.Context, step Step, ec expressionContext) (result StepExecution, err error) {
	ac := ActionContext{RunID: ec.RunID, JobID: ec.JobID, StepID: step.ID, Iteration: ec.IterationNumber, Generation: 1}
	if f, ok := ctx.Value(fenceKey{}).(executionFence); ok {
		ac.Generation = f.generation
	}
	operationRun := nativeString(ec.Metadata, "iteration_logical_run_id")
	if operationRun == "" {
		operationRun = ac.RunID
	}
	ac.OperationKey = actionHash([]any{operationRun, ac.JobID, ac.StepID, ac.Iteration})
	result = StepExecution{ID: step.ID, Status: RunStatusFailed}
	defer func() {
		if err != nil && errors.Is(err, ErrActionIntegrity) {
			result.Status = RunStatusNeedsAttention
			result.Error = err.Error()
		}
	}()
	fail := func(err error) (StepExecution, error) { return result, actionFailure(ac, err) }
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if e.Ready != nil {
		if err = e.Ready(ctx); err != nil {
			return fail(err)
		}
	}
	a := e.applicationActions[strings.TrimPrefix(step.Uses, "action/")]
	j := actionJournal(e.Store)
	if a == nil || j == nil {
		return fail(fmt.Errorf("registered action or durable ActionJournal is unavailable"))
	}
	args, err := renderMap(step.With, ec)
	if err != nil {
		return result, err
	}
	if args == nil {
		args = map[string]any{}
	}
	input, err := actionJSON(args)
	if err != nil {
		return fail(err)
	}
	if err = validateActionJSON(input, a.input); err != nil {
		return result, fmt.Errorf("action/%s input schema: %w", a.contract.Name, err)
	}
	input, err = e.actionIdentityJSON(input)
	if err != nil {
		return fail(err)
	}
	recordRun := ec.RunID
	if nativeString(ec.Metadata, "iteration_logical_run_id") != "" {
		recordRun = nativeString(ec.Metadata, "origin_run_id")
	}
	records, err := j.ActionAttempts(ctx, recordRun)
	if err != nil {
		return fail(err)
	}
	var last *ActionAttempt
	for i := range records {
		if records[i].Context.OperationKey == ac.OperationKey {
			last = &records[i]
		}
	}
	if last != nil {
		if err = e.validateRetainedAction(ctx, *last); err != nil {
			return fail(err)
		}
		sameInput := actionHash(input) == last.InputFingerprint
		if !sameInput && a.contract.Effect == ActionExternalMutation {
			return fail(fmt.Errorf("rendered action input changed"))
		}
		if last.State == "completed" && sameInput {
			return last.Attempt.Result, nil
		}
		if a.contract.Effect == ActionExternalMutation && last.State != "no_effect" {
			observation := ActionRecoveryObservation{Generation: ac.Generation, Time: time.Now().UTC(), Outcome: "unknown"}
			var recovered actionReconciliation
			if a.reconcile == nil {
				err = fmt.Errorf("uncertain mutation has no reconciliation hook")
			} else {
				recovered, err = a.reconcile(ctx, last.Context, last.Input, last.Bindings, last.Result)
			}
			if err == nil {
				recovered.Proof, err = e.safeActionJSON(recovered.Proof)
				if err == nil && (recovered.Outcome == "completed" || recovered.Outcome == "no_effect") && (len(recovered.Proof) == 0 || bytes.Equal(recovered.Proof, []byte("null"))) {
					err = fmt.Errorf("reconciliation requires non-null proof")
				}
				if err == nil && recovered.Outcome != "completed" && recovered.Outcome != "no_effect" {
					err = fmt.Errorf("mutation outcome remains unknown")
				}
			}
			if err == nil && recovered.Outcome == "completed" {
				var safe ActionValue
				safe, err = e.safeActionResult(recovered.Result)
				if err == nil {
					candidate := *last
					candidate.Result = safe
					err = a.validateResult(ctx, candidate)
				}
				if err == nil {
					last.Result = safe
				}
			}
			observation.Proof = recovered.Proof
			if err != nil {
				observation.Error = Redact(err.Error(), e.redactionValues)
			} else {
				observation.Outcome = recovered.Outcome
			}
			last.Reconciliations = append(last.Reconciliations, observation)
			if err == nil {
				last.State = recovered.Outcome
			}
			if err == nil && last.State == "completed" {
				result, err = completeAction(last)
			}
			persistErr := e.checkpointAction(ctx, j, last)
			if err != nil || persistErr != nil {
				return fail(errors.Join(err, persistErr))
			}
			if last.State == "completed" {
				return result, nil
			}
		}
		ac.Attempt = last.Attempt.Number
	}
	ac.Attempt++
	ac.AttemptID = newID()
	bindings, err := a.bindings(ctx, ac, input)
	if err != nil {
		return fail(err)
	}
	bindings, err = e.actionIdentityJSON(bindings)
	if err != nil {
		return fail(err)
	}
	r := ActionAttempt{Context: ac, Contract: a.contract, Input: input, Bindings: bindings, InputFingerprint: actionHash(input), BindingsFingerprint: actionHash(bindings), State: "admitted", Revision: 1,
		Attempt: Attempt{ID: ac.AttemptID, RunID: ac.RunID, JobID: ac.JobID, StepID: ac.StepID, Iteration: ac.Iteration, Number: ac.Attempt, Status: RunStatusRunning, StartedAt: time.Now().UTC()}}
	if err = j.AdmitAction(ctx, r); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	if ec.Ordered {
		if err = e.iterationEvidence(ctx, RunRequest{runID: ec.RunID, Metadata: ec.Metadata}); err != nil {
			return fail(err)
		}
	}
	value, callErr := a.handler(ctx, ac, input)
	r.Result, err = e.safeActionResult(value)
	if err == nil && callErr == nil {
		err = validateActionJSON(r.Result.Output, a.output)
	}
	err = errors.Join(callErr, err)
	if err == nil {
		result, err = completeAction(&r)
	} else {
		r.Error = Redact(err.Error(), e.redactionValues)
		r.Attempt.Status = RunStatusFailed
		r.Attempt.FinishedAt = time.Now().UTC()
		r.Attempt.Result = StepExecution{ID: step.ID, Status: RunStatusFailed, Error: r.Error, AttemptID: ac.AttemptID}
	}
	persistErr := e.checkpointAction(ctx, j, &r)
	if persistErr != nil {
		return fail(errors.Join(err, persistErr))
	}
	if err != nil && a.contract.Effect == ActionExternalMutation {
		return fail(err)
	}
	return result, err
}
