package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Each sequence owns its Steps map. A parallel join is the only place where
// branch-local results enter its parent's expression context.
func (e *Executor) executeStepSequence(ctx context.Context, steps []Step, prefix string, ec expressionContext, req RunRequest, slots chan struct{}) (string, error) {
	for i, step := range steps {
		path := stepPath(prefix, i)
		if step.ID == "" {
			step.ID = path
		}
		result, err := e.executeDeclaredStep(ctx, step, path, ec, req, slots)
		ec.Steps[step.ID] = result
		if err != nil && (isStepBlock(step) || req.ordered || !step.ContinueOnError || hardExecutionFailure(err)) {
			return result.Status, err
		}
		if result.Status == RunStatusWaiting && err == nil {
			return RunStatusWaiting, nil
		}
	}
	return RunStatusSucceeded, nil
}

func (e *Executor) executeDeclaredStep(ctx context.Context, step Step, path string, ec expressionContext, req RunRequest, slots chan struct{}) (result StepExecution, err error) {
	result = StepExecution{ID: step.ID, Status: RunStatusSucceeded}
	ec.Attempt = nil
	defer func() {
		if onlyError(err, ErrExternalInputCanceled) {
			result.Status, result.Error, result.FailureClass, err = RunStatusWaiting, "", "", nil
		} else if err != nil {
			result.Status, result.FailureClass, result.Error = executionFailureStatus(ctx, err), classifyFailure(err), err.Error()
		}
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	if err = e.checkEffectAdmission(ctx, req.runID); err != nil {
		return
	}
	var allowed bool
	allowed, err = evalIf(step.If, ec)
	if err != nil {
		return
	}
	if !allowed {
		result.Status = RunStatusSkipped
		return
	}
	if isStepBlock(step) {
		return e.executeStepBlock(ctx, step, path, ec, req, slots)
	}
	if step.Conversation != nil {
		return e.executeConversationStep(ctx, step, ec, req, slots)
	}
	if hasLifecycle(step) {
		return e.executeLifecycleStep(ctx, step, ec, slots, req.RetryUncertain, false)
	}
	if step.Uses == "input/wait" {
		return e.executeInputWait(ctx, step, ec)
	}
	if isApplicationAction(step.Uses) {
		return e.executeApplicationAction(ctx, step, ec)
	}
	var attempt Attempt
	var reused bool
	journal, durable := e.Store.(Journal)
	if durable {
		if req.ordered {
			attempt, reused, err = e.beginIterationStep(ctx, journal, step, ec, req.RetryUncertain)
		} else {
			attempt, reused, err = journal.Begin(ctx, req.runID, ec.JobID, step.ID, req.iteration, req.RetryUncertain)
		}
	}
	if err != nil {
		return
	}
	if reused {
		return attempt.Result, nil
	}
	result.StartedAt, result.AttemptID = time.Now().UTC(), attempt.ID
	if durable {
		ec.Attempt = &attempt
	}
	result.Outputs, err = e.executeStep(invocationContext(ctx, e.Store, ec.Attempt), step, ec, slots)
	result.FinishedAt = time.Now().UTC()
	if onlyError(err, ErrExternalInputCanceled) {
		result.Status, err = RunStatusWaiting, nil
	}
	if err != nil {
		result.Status, result.Error, result.FailureClass = executionFailureStatus(ctx, err), err.Error(), classifyFailure(err)
	}
	if durable {
		if persistErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); persistErr != nil {
			err = errors.Join(err, iterationPersistence(errors.Join(errStepPersistence, persistErr), req.ordered))
		}
	}
	return
}

func branchContext(ec expressionContext) (expressionContext, error) {
	// Preserve typed execution records and JSON numbers while detaching nested
	// output maps. Non-durable callers can supply values JSON cannot encode.
	data, err := json.Marshal(ec)
	if err != nil {
		return expressionContext{}, err
	}
	var copy expressionContext
	if err := decodeJSONWithNumbers(data, &copy); err != nil {
		return expressionContext{}, err
	}
	if copy.Steps == nil {
		copy.Steps = map[string]StepExecution{}
	}
	copy.Attempt = nil
	return copy, nil
}

func (e *Executor) executeStepBlock(ctx context.Context, step Step, path string, ec expressionContext, req RunRequest, slots chan struct{}) (result StepExecution, err error) {
	result = StepExecution{ID: step.ID, Status: RunStatusSucceeded, StartedAt: time.Now().UTC()}
	// Prepare every snapshot before starting branches or recording orchestration.
	// A serialization failure must not dispatch work with an empty context.
	contexts := make([]expressionContext, len(step.Parallel))
	for i := range contexts {
		contexts[i], err = branchContext(ec)
		if err != nil {
			result.FinishedAt = time.Now().UTC()
			return result, fmt.Errorf("parallel branch snapshot: %w", err)
		}
	}
	journal, durable := e.Store.(Journal)
	var attempt Attempt
	var reused bool
	if durable {
		// Orchestration has no effects. Children retain their independent replay
		// guards; a completed block is reconstructed from those child receipts.
		if j := blockJournal(e.Store); j != nil {
			attempt, reused, err = j.BeginBlock(ctx, req.runID, ec.JobID, step.ID, req.iteration)
		} else {
			attempt, reused, err = journal.Begin(ctx, req.runID, ec.JobID, step.ID, req.iteration, true)
			if err == nil && !reused {
				attempt.Phase = "orchestration"
				err = journal.RecordAttempt(ctx, attempt)
			}
		}
		if err != nil {
			return result, err
		}
		result.AttemptID = attempt.ID
	}
	if step.Sequence != nil {
		result.Status, err = e.executeStepSequence(ctx, step.Sequence, path+"_sequence", ec, req, slots)
	} else {
		type branchResult struct {
			index  int
			result StepExecution
			steps  map[string]StepExecution
			err    error
		}
		children := make([]branchResult, len(step.Parallel))
		done := make(chan branchResult, len(children))
		branchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		for i, child := range step.Parallel {
			childPath := stepPath(path+"_parallel", i)
			if child.ID == "" {
				child.ID = childPath
			}
			childEC := contexts[i]
			go func(i int, child Step, childPath string, childEC expressionContext) {
				r, failure := e.executeDeclaredStep(branchCtx, child, childPath, childEC, req, slots)
				childEC.Steps[child.ID] = r
				// A continued leaf failure is a completed branch. Structural
				// branches propagate all unhandled errors from their sequences.
				if failure != nil && !isStepBlock(child) && child.ContinueOnError && !req.ordered && !hardExecutionFailure(failure) {
					failure = nil
				}
				done <- branchResult{i, r, childEC.Steps, failure}
			}(i, child, childPath, childEC)
		}
		for range children {
			child := <-done
			children[child.index] = child
			if child.err != nil {
				cancel()
			}
		}
		ordered := make([]any, 0, len(children))
		for _, child := range children {
			for id, observed := range child.steps {
				if _, incoming := ec.Steps[id]; !incoming {
					ec.Steps[id] = observed
				}
			}
			ordered = append(ordered, map[string]any{"id": child.result.ID, "status": child.result.Status})
			if child.result.Status == RunStatusWaiting {
				result.Status = RunStatusWaiting
			}
			err = errors.Join(err, child.err)
		}
		result.Outputs = map[string]any{"branches": ordered}
	}
	result.FinishedAt = time.Now().UTC()
	if err != nil {
		result.Status, result.Error, result.FailureClass = executionFailureStatus(ctx, err), err.Error(), classifyFailure(err)
	}
	if durable && !reused {
		if persistErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); persistErr != nil {
			err = errors.Join(err, iterationPersistence(errors.Join(errStepPersistence, persistErr), req.ordered))
		}
	}
	return result, err
}

type stepBlockJournal interface {
	BeginBlock(context.Context, string, string, string, int) (Attempt, bool, error)
}

func blockJournal(store RunStore) stepBlockJournal {
	if wrapped, ok := store.(redactedStore); ok {
		return blockJournal(wrapped.Journal)
	}
	j, _ := store.(stepBlockJournal)
	return j
}
