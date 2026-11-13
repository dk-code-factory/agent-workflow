package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

func hasLifecycle(step Step) bool { return len(step.OnError)+len(step.OnFailure)+len(step.Finally) > 0 }

// Handler lists have local IDs; effect IDs include their full handler ancestry.
func lifecycleSteps(steps []Step) []Step {
	var out []Step
	for _, step := range steps {
		out = append(out, step)
		for _, children := range [][]Step{step.OnError, step.OnFailure, step.Finally, step.Parallel, step.Sequence} {
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
		if step.Protocol != "" && depth > 0 {
			return fmt.Errorf("structured helpers cannot appear inside ordinary lifecycle handlers")
		}
		if step.Conversation != nil && (depth > 0 || step.OnError != nil || step.OnFailure != nil || step.Finally != nil || step.HandlerTimeout != "") {
			return fmt.Errorf("conversations cannot own or appear inside lifecycle handlers")
		}
		if isStepBlock(step) {
			if depth > 0 || hasLifecycle(step) || step.HandlerTimeout != "" {
				return fmt.Errorf("structural blocks cannot own or appear inside lifecycle handlers")
			}
			for _, list := range [][]Step{step.Parallel, step.Sequence} {
				if err := validateLifecycleShape(list, depth, count); err != nil {
					return err
				}
			}
		}
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

type retainedLifecycleFailure struct {
	message, class, unsafeClass string
	cause                       error
}

func (err *retainedLifecycleFailure) Error() string { return err.message }
func (err *retainedLifecycleFailure) Unwrap() error { return err.cause }

func lifecycleError(result StepExecution) error {
	if result.Status == RunStatusSucceeded || result.Status == RunStatusSkipped {
		return nil
	}
	var cause error
	switch result.FailureClass {
	case "canceled":
		cause = context.Canceled
	case "timeout":
		cause = context.DeadlineExceeded
	case "provider_failure":
		cause = ErrProviderFailure
	case "usage_exhausted":
		cause = ErrUsageExhausted
	case "iteration_blocked":
		cause = errIterationBlocked
	case "action_integrity":
		cause = ErrActionIntegrity
	case "prerequisite":
		cause = operational.ErrUnavailable
	case "needs_attention":
		if result.Status == RunStatusNeedsAttention {
			cause = process.ErrCleanup
		}
	}
	if result.UnsafeFailureClass == "canceled" && result.FailureClass != "canceled" {
		cause = errors.Join(cause, errors.New(result.Error), context.Canceled)
	}
	return &retainedLifecycleFailure{message: result.Error, class: result.FailureClass, unsafeClass: result.UnsafeFailureClass, cause: cause}
}

func lifecycleContextError(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	if err == nil {
		err = ctx.Err()
	}
	if hardExecutionFailure(err) {
		return err
	}
	return &retainedLifecycleFailure{message: err.Error(), class: classifyFailure(err), unsafeClass: "canceled", cause: errors.Join(err, context.Canceled)}
}

func lifecycleAttempts(ctx context.Context, store RunStore, run string) ([]Attempt, error) {
	if redacted, ok := store.(redactedStore); ok {
		return lifecycleAttempts(ctx, redacted.Journal, run)
	}
	if journal, ok := store.(interface {
		Attempts(context.Context, string) ([]Attempt, error)
	}); ok {
		return journal.Attempts(ctx, run)
	}
	return nil, fmt.Errorf("%w: lifecycle resume requires readable child receipts", ErrActionIntegrity)
}

// Restore observed children before admission checks, including nested failures
// whose parent had not yet published its final receipt when execution stopped.
func restoreLifecycleHandlers(step Step, life *StepLifecycle, attempts map[string]Attempt) error {
	var unsafe error
	for _, hook := range []struct {
		kind string
		list []Step
	}{{"onError", step.OnError}, {"onFailure", step.OnFailure}, {"finally", step.Finally}} {
		for i, child := range hook.list {
			sid := stepID(child, i)
			child.ID = step.ID + "/" + hook.kind + "/" + sid
			a, ok := attempts[child.ID]
			if !ok || (a.Phase != "lifecycle_completed" && a.Phase != "lifecycle_task_completed") {
				continue
			}
			r := a.Result
			if r.Lifecycle != nil && a.Phase == "lifecycle_task_completed" {
				r.Lifecycle.Handlers = map[string][]StepExecution{}
				unsafe = errors.Join(unsafe, restoreLifecycleHandlers(child, r.Lifecycle, attempts))
			}
			if failure := lifecycleError(r); hardExecutionFailure(failure) {
				unsafe = errors.Join(unsafe, failure)
			}
			r.ID = sid
			life.Handlers[hook.kind] = append(life.Handlers[hook.kind], r)
		}
	}
	return unsafe
}

func (e *Executor) executeLifecycleStep(ctx context.Context, step Step, ec expressionContext, slots chan struct{}, retry bool, child bool) (result StepExecution, returned error) {
	result = StepExecution{ID: step.ID, Status: RunStatusSucceeded}
	defer func() {
		if returned != nil {
			returned = lifecycleContextError(ctx, returned)
			result.Status = executionFailureStatus(ctx, returned)
			result.Error = returned.Error()
			result.FailureClass = classifyFailure(returned)
			result.UnsafeFailureClass = unsafeFailureClass(returned)
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
	if step.Protocol == ContainerActionV1 {
		return e.executeContainerAction(ctx, step, ec, slots)
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
		err = lifecycleContextError(ctx, err)
		if err != nil {
			result.Status = executionFailureStatus(ctx, err)
			result.Error = err.Error()
			result.FailureClass = classifyFailure(err)
			result.UnsafeFailureClass = unsafeFailureClass(err)
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
					return result, errors.Join(err, iterationPersistence(errors.Join(errStepPersistence, persistErr), ec.Ordered))
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
		if resuming {
			attempts, loadErr := lifecycleAttempts(ctx, e.Store, ec.RunID)
			if loadErr != nil {
				return result, errors.Join(err, loadErr)
			}
			latest := map[string]Attempt{}
			for _, a := range attempts {
				if a.RunID == ec.RunID && a.JobID == ec.JobID && a.Iteration == ec.IterationNumber && a.Number > latest[a.StepID].Number {
					latest[a.StepID] = a
				}
			}
			life.Handlers = map[string][]StepExecution{}
			err = errors.Join(err, restoreLifecycleHandlers(step, life, latest))
		}
		// This receipt is persisted before any handler effect. Its absolute deadline
		// also bounds a resumed handler instead of granting a fresh time budget.
		hctx, cancel := context.WithDeadline(ctx, life.Deadline)
		defer cancel()
		handlerEC := ec
		prompt := ""
		for _, field := range []string{"prompt", "message"} {
			if value, renderErr := renderValue(step.With[field], ec); renderErr == nil {
				prompt, _ = value.(string)
				if prompt != "" {
					break
				}
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
					return nil, fmt.Errorf("%s: %w", kind, lifecycleContextError(hctx, hctx.Err()))
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
				if hctx.Err() != nil {
					childErr = errors.Join(childErr, lifecycleContextError(hctx, hctx.Err()))
				}
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
	err = lifecycleContextError(ctx, err)
	result.FinishedAt = time.Now().UTC()
	if err != nil {
		result.Status = executionFailureStatus(ctx, err)
		result.Error = err.Error()
		result.FailureClass = classifyFailure(err)
		result.UnsafeFailureClass = unsafeFailureClass(err)
	}
	if durable {
		if hasLifecycle(step) || child {
			attempt.Phase = "lifecycle_completed"
		}
		if persistErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); persistErr != nil {
			return result, errors.Join(err, iterationPersistence(errors.Join(errStepPersistence, persistErr), ec.Ordered))
		}
	}
	return result, err
}

func validateLifecycleReplacement(step Step, outputs map[string]any, ec expressionContext) error {
	if !strings.HasPrefix(step.Uses, "agent/") {
		return nil
	}
	output, err := renderValue(step.With["output"], ec)
	if err != nil {
		return err
	}
	contract, err := ParseAgentOutputContract(output)
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
		if strings.HasPrefix(step.Uses, "agent/") || step.Conversation != nil {
			output, err := resolveNamedOutput(w, step.With["output"])
			if err != nil {
				return err
			}
			if output != nil {
				steps[i].With["output"] = output
			}
		}
		for _, list := range [][]Step{step.OnError, step.OnFailure, step.Finally, step.Parallel, step.Sequence} {
			if err := expandLifecycleOutputs(w, list); err != nil {
				return err
			}
		}
	}
	return nil
}
