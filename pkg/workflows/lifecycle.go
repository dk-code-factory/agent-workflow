package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func hasLifecycle(step Step) bool { return len(step.OnError)+len(step.OnFailure)+len(step.Finally) > 0 }

// Each sequence has local IDs; effect IDs include its full handler ancestry.
func lifecycleSteps(steps []Step) []Step {
	var out []Step
	for _, step := range steps {
		out = append(out, step)
		for _, children := range [][]Step{step.OnError, step.OnFailure, step.Finally} {
			out = append(out, lifecycleSteps(children)...)
		}
	}
	return out
}

func validateLifecycleShape(steps []Step, depth int, count *int) error {
	if len(steps) > 0 && depth > 8 {
		return fmt.Errorf("handler nesting exceeds 8")
	}
	for _, step := range steps {
		if depth > 0 {
			*count++
			if *count > 100 {
				return fmt.Errorf("handler steps exceed 100 per job")
			}
		}
		if depth > 0 || hasLifecycle(step) {
			if !(strings.HasPrefix(step.Uses, "agent/") || step.Agent != nil || len(step.Argv) > 0 || step.Run != "") {
				return fmt.Errorf("lifecycle handlers support agent and command steps only")
			}
		}
		if step.HandlerTimeout != "" {
			d, err := time.ParseDuration(step.HandlerTimeout)
			if err != nil || d <= 0 || d > 5*time.Minute || !hasLifecycle(step) {
				return fmt.Errorf("handlerTimeout requires handlers and a duration greater than zero and at most 5m")
			}
		}
		for _, list := range [][]Step{step.OnError, step.OnFailure, step.Finally} {
			if err := validateLifecycleShape(list, depth+1, count); err != nil {
				return err
			}
		}
	}
	return nil
}

func lifecycleError(result StepExecution) error {
	if result.Status == RunStatusSucceeded || result.Status == RunStatusSkipped {
		return nil
	}
	switch result.FailureClass {
	case "canceled":
		return context.Canceled
	case "timeout":
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, result.Error)
	case "provider_failure":
		return fmt.Errorf("%w: %s", ErrProviderFailure, result.Error)
	case "action_integrity", "needs_attention", "prerequisite":
		return fmt.Errorf("uncertain retained %s: %s", result.FailureClass, result.Error)
	}
	return errors.New(result.Error)
}

func (e *Executor) executeLifecycleStep(ctx context.Context, step Step, ec expressionContext, slots chan struct{}, retry bool, child bool) (result StepExecution, returned error) {
	result = StepExecution{ID: step.ID, Status: RunStatusSucceeded}
	defer func() {
		if returned != nil {
			result.Status = executionFailureStatus(ctx, returned)
			result.Error = returned.Error()
			result.FailureClass = classifyFailure(returned)
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := e.checkEffectAdmission(ctx, ec.RunID); err != nil {
		return result, err
	}
	allowed, err := evalIf(step.If, ec)
	if err == nil && !allowed {
		result.Status = RunStatusSkipped
		return result, nil
	}
	if err != nil {
		result.Status = executionFailureStatus(ctx, err)
		result.Error = err.Error()
		result.FailureClass = classifyFailure(err)
		return result, err
	}
	journal, durable := e.Store.(Journal)
	var attempt Attempt
	if durable {
		var reused bool
		if ec.Ordered {
			attempt, reused, err = e.beginIterationStep(ctx, journal, step, ec, retry)
		} else {
			attempt, reused, err = journal.Begin(ctx, ec.RunID, ec.JobID, step.ID, ec.IterationNumber, retry)
		}
		if err != nil {
			return result, err
		}
		if reused {
			return attempt.Result, lifecycleError(attempt.Result)
		}
		ec.Attempt = &attempt
	}
	resuming := attempt.Phase == "lifecycle_task_completed"
	if resuming {
		result = attempt.Result
		err = lifecycleError(result)
	} else {
		result.StartedAt = time.Now().UTC()
		result.AttemptID = attempt.ID
		result.Outputs, err = e.executeStep(invocationContext(ctx, e.Store, ec.Attempt), step, ec, slots)
		if err != nil {
			result.Status = executionFailureStatus(ctx, err)
			result.Error = err.Error()
			result.FailureClass = classifyFailure(err)
		}
		if hasLifecycle(step) {
			bound := 30 * time.Second
			if step.HandlerTimeout != "" {
				bound, _ = time.ParseDuration(step.HandlerTimeout)
			}
			result.Lifecycle = &StepLifecycle{OriginalError: result.Error, OriginalClass: result.FailureClass, Deadline: time.Now().UTC().Add(bound), Handlers: map[string][]StepExecution{}, Skipped: map[string]string{}}
			if durable {
				attempt.Phase = "lifecycle_task_completed"
				attempt.Result = result
				if persistErr := journal.RecordAttempt(context.WithoutCancel(ctx), attempt); persistErr != nil {
					return result, errors.Join(err, persistErr)
				}
			}
		}
	}
	if hasLifecycle(step) {
		life := result.Lifecycle
		if life == nil {
			return result, fmt.Errorf("uncertain lifecycle receipt missing task result")
		}
		if life.Handlers == nil {
			life.Handlers = map[string][]StepExecution{}
		}
		if life.Skipped == nil {
			life.Skipped = map[string]string{}
		}
		// This receipt is persisted before any handler effect. Its absolute deadline
		// also bounds a resumed handler instead of granting a fresh time budget.
		hctx, cancel := context.WithDeadline(ctx, life.Deadline)
		defer cancel()
		handlerEC := ec
		prompt := ""
		if args, renderErr := renderMap(step.With, ec); renderErr == nil {
			prompt = nativeString(args, "prompt")
			if prompt == "" {
				prompt = nativeString(args, "message")
			}
		}
		handlerEC.Failed = map[string]any{"id": step.ID, "uses": step.Uses, "prompt": Redact(prompt, e.redactions(ec.Secrets))}
		handlerEC.Error = map[string]any{"message": Redact(life.OriginalError, e.redactions(ec.Secrets)), "class": life.OriginalClass}
		run := func(kind string, list []Step) (map[string]any, error) {
			if len(list) == 0 {
				return nil, nil
			}
			if ctx.Err() != nil || hardExecutionFailure(err) || hctx.Err() != nil {
				reason := "unsafe failure"
				if ctx.Err() != nil {
					reason = "execution canceled"
				} else if hctx.Err() != nil {
					reason = "handler deadline exhausted"
				}
				life.Skipped[kind] = reason
				if hctx.Err() != nil && ctx.Err() == nil && !hardExecutionFailure(err) {
					return nil, fmt.Errorf("%s: %w", kind, hctx.Err())
				}
				return nil, nil
			}
			local := handlerEC
			local.Steps = map[string]StepExecution{}
			for id, r := range ec.Steps {
				local.Steps[id] = r
			}
			var replacement map[string]any
			life.Handlers[kind] = nil
			for i, childStep := range list {
				sid := stepID(childStep, i)
				childStep.ID = step.ID + "/" + kind + "/" + sid
				childResult, childErr := e.executeLifecycleStep(hctx, childStep, local, slots, retry, true)
				childResult.ID = sid
				life.Handlers[kind] = append(life.Handlers[kind], childResult)
				local.Steps[sid] = childResult
				if childErr != nil && (!childStep.ContinueOnError || hardExecutionFailure(childErr)) {
					return nil, fmt.Errorf("%s %s: %w", kind, sid, childErr)
				}
				if childResult.Status == RunStatusSucceeded {
					replacement = childResult.Outputs
				}
			}
			return replacement, nil
		}
		if err != nil && len(step.OnError) > 0 {
			replacement, fallbackErr := run("onError", step.OnError)
			if fallbackErr == nil && replacement != nil && life.Skipped["onError"] == "" {
				fallbackErr = validateLifecycleReplacement(step, replacement, ec)
				if fallbackErr == nil {
					result.Outputs = replacement
					result.Status = RunStatusSucceeded
					result.Error = ""
					result.FailureClass = ""
					err = nil
				}
			}
			if fallbackErr != nil {
				err = errors.Join(err, fallbackErr)
			}
		}
		if err != nil {
			_, observerErr := run("onFailure", step.OnFailure)
			err = errors.Join(err, observerErr)
		}
		_, finalErr := run("finally", step.Finally)
		err = errors.Join(err, finalErr)
	}
	result.FinishedAt = time.Now().UTC()
	if err != nil {
		result.Status = executionFailureStatus(ctx, err)
		result.Error = err.Error()
		result.FailureClass = classifyFailure(err)
	}
	if durable {
		if hasLifecycle(step) || child {
			attempt.Phase = "lifecycle_completed"
		}
		if persistErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); persistErr != nil {
			return result, errors.Join(err, persistErr)
		}
	}
	return result, err
}

func validateLifecycleReplacement(step Step, outputs map[string]any, ec expressionContext) error {
	if !strings.HasPrefix(step.Uses, "agent/") {
		return nil
	}
	args, err := renderMap(step.With, ec)
	if err != nil {
		return err
	}
	contract, err := ParseAgentOutputContract(args["output"])
	if err != nil || contract == nil {
		return err
	}
	value, ok := outputs["structured"]
	if !ok {
		return fmt.Errorf("structured output fallback is missing structured result")
	}
	if err := validateJSONSchemaValue(value, contract.Schema, "$"); err != nil {
		return fmt.Errorf("structured output fallback: %w", err)
	}
	return nil
}

func expandLifecycleOutputs(w *Workflow, steps []Step) error {
	for i, step := range steps {
		if strings.HasPrefix(step.Uses, "agent/") {
			output, err := resolveNamedOutput(w, step.With["output"])
			if err != nil {
				return err
			}
			if output != nil {
				steps[i].With["output"] = output
			}
		}
		for _, list := range [][]Step{step.OnError, step.OnFailure, step.Finally} {
			if err := expandLifecycleOutputs(w, list); err != nil {
				return err
			}
		}
	}
	return nil
}
