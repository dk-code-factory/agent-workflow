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

func hasLifecycle(step Step) bool {
	return len(step.OnResult)+len(step.OnComplete)+len(step.OnIncomplete)+len(step.OnError)+len(step.OnFailure)+len(step.Finally) > 0
}

// Handler lists have local IDs; effect IDs include their full handler ancestry.
func lifecycleSteps(steps []Step) []Step {
	var out []Step
	for _, step := range steps {
		out = append(out, step)
		for _, children := range [][]Step{step.OnResult, step.OnComplete, step.OnIncomplete, step.OnError, step.OnFailure, step.Finally, step.Parallel, step.Sequence} {
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
		if step.Conversation != nil && (depth > 0 || hasLifecycle(step) || step.HandlerTimeout != "") {
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
			if !(strings.HasPrefix(step.Uses, "agent/") || len(step.Argv) > 0 || step.Run != "") {
				return fmt.Errorf("lifecycle handlers support agent and command steps only")
			}
		}
		if step.HandlerTimeout != "" {
			d, err := time.ParseDuration(step.HandlerTimeout)
			if err != nil || d <= 0 || d > 5*time.Minute || !hasLifecycle(step) {
				return fmt.Errorf("handlerTimeout requires handlers and a duration greater than zero and at most 5m")
			}
		}
		for _, list := range [][]Step{step.OnResult, step.OnComplete, step.OnIncomplete, step.OnError, step.OnFailure, step.Finally} {
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
	for id, a := range attempts {
		if strings.HasPrefix(id, step.ID+"/") && (a.Phase == "lifecycle_task_completed" || a.Phase == "lifecycle_completed") {
			if failure := lifecycleError(a.Result); hardExecutionFailure(failure) {
				unsafe = errors.Join(unsafe, failure)
			}
		}
	}
	for _, hook := range []struct {
		kind string
		list []Step
	}{{"onResult", step.OnResult}, {"onComplete", step.OnComplete}, {"onIncomplete", step.OnIncomplete}, {"onError", step.OnError}, {"onFailure", step.OnFailure}, {"finally", step.Finally}} {
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
			result.Lifecycle = &StepLifecycle{Candidate: cloneJSONMap(result.Outputs), Agent: nativeString(result.Outputs, "agent"), OriginalError: result.Error, OriginalClass: result.FailureClass, Deadline: time.Now().UTC().Add(bound), Handlers: map[string][]StepExecution{}, Skipped: map[string]string{}}
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
		handlerEC.Hook = true
		ordinals := map[string]int{}
		persistSelection := func() error {
			if !durable {
				return nil
			}
			attempt.Result.Lifecycle = life
			if err := journal.RecordAttempt(context.WithoutCancel(ctx), attempt); err != nil {
				return errors.Join(errStepPersistence, err)
			}
			return nil
		}
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
			ordinals[kind]++
			local := handlerEC
			source := any(ec.Event)
			if ec.Hook {
				source = ec.Event["source"]
			}
			candidate := result.Outputs["structured"]
			if candidate == nil {
				candidate = result.Outputs["text"]
			}
			local.Event = map[string]any{"type": kind, "agent": life.Agent, "result": candidate, "verification": life.Verification, "error": nil, "source": source}
			if err != nil {
				local.Event["error"] = map[string]any{"message": err.Error(), "class": classifyFailure(err)}
			}
			if persistErr := persistSelection(); persistErr != nil {
				return nil, persistErr
			}
			local.Steps = map[string]StepExecution{}
			for id, r := range ec.Steps {
				local.Steps[id] = r
			}
			var replacement map[string]any
			life.Handlers[kind] = nil
			for i, childStep := range list {
				sid := stepID(childStep, i)
				childStep.ID = step.ID + "/" + kind + "/" + sid
				if kind == "onResult" {
					childStep.ID = fmt.Sprintf("%s/onResult/candidate_%d/%s", step.ID, ordinals[kind], sid)
				}
				if childStep.With == nil {
					childStep.With = map[string]any{}
				} else {
					childStep.With = cloneJSONMap(childStep.With)
				}
				if childStep.Uses == "agent/run" && childStep.With["input"] == nil {
					childStep.With["input"] = "${{ event }}"
				}
				if childStep.Uses == "agent/run" && (kind == "onError" || kind == "onIncomplete") && childStep.With["output"] == nil && step.With["output"] != nil {
					childStep.With["output"] = cloneJSONValue(step.With["output"])
				}
				if kind == "onResult" && childStep.With["output"] == nil {
					childStep.With["output"] = verdictOutput()
				}
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

		verificationOrdinal := 0
		verify := func() error {
			if !completionAware(step) {
				return nil
			}
			candidateComplete, verifyErr := completionValue(result.Outputs)
			if verifyErr != nil {
				return verifyErr
			}
			verdict := map[string]any{"structured": map[string]any{"complete": true}}
			if len(step.OnResult) > 0 {
				verdict, verifyErr = run("onResult", step.OnResult)
				if verifyErr != nil {
					return verifyErr
				}
				if verdict == nil {
					return fmt.Errorf("onResult produced no successful verification verdict")
				}
			}
			accepted, verifyErr := completionValue(verdict)
			if verifyErr != nil {
				return fmt.Errorf("onResult: %w", verifyErr)
			}
			life.Verification = cloneJSONValue(verdict["structured"])
			verificationOrdinal++
			if verificationOrdinal <= len(life.Verifications) {
				if actionHash(life.Verifications[verificationOrdinal-1]) != actionHash(life.Verification) {
					return fmt.Errorf("%w: retained verification verdict changed", ErrActionIntegrity)
				}
			} else {
				life.Verifications = append(life.Verifications, life.Verification)
			}
			result.Outputs["complete"] = candidateComplete && accepted
			return persistSelection()
		}
		replacementCandidate := func(kind string, list []Step) map[string]any {
			var candidate map[string]any
			for i, receipt := range life.Handlers[kind] {
				if i < len(list) && list[i].Uses == "agent/run" && receipt.Status == RunStatusSucceeded {
					candidate = receipt.Outputs
				}
			}
			return candidate
		}
		incomplete := func() error {
			if !completionAware(step) || result.Outputs["complete"] != false {
				return nil
			}
			_, incompleteErr := run("onIncomplete", step.OnIncomplete)
			if incompleteErr != nil {
				return incompleteErr
			}
			replacement := replacementCandidate("onIncomplete", step.OnIncomplete)
			if replacement == nil {
				return nil
			}
			if err := validateLifecycleReplacement(step, replacement, ec); err != nil {
				return err
			}
			result.Outputs = cloneJSONMap(replacement)
			if life.Agent != "" {
				result.Outputs["agent"] = life.Agent
			}
			return verify()
		}
		if err == nil {
			err = verify()
		}
		incompleteRan := false
		if err == nil && completionAware(step) && result.Outputs["complete"] == false {
			incompleteRan = true
			err = incomplete()
		}
		if err != nil && life.OriginalError == "" {
			life.OriginalError = err.Error()
			life.OriginalClass = classifyFailure(err)
		}
		if err != nil && len(step.OnError) > 0 {
			if life.OriginalError == "" {
				life.OriginalError = err.Error()
				life.OriginalClass = classifyFailure(err)
			}
			replacement, fallbackErr := run("onError", step.OnError)
			if step.Uses == "agent/run" {
				replacement = replacementCandidate("onError", step.OnError)
			}
			if fallbackErr == nil && replacement != nil && life.Skipped["onError"] == "" {
				fallbackErr = validateLifecycleReplacement(step, replacement, ec)
				if fallbackErr == nil {
					result.Outputs = cloneJSONMap(replacement)
					if life.Agent != "" {
						result.Outputs["agent"] = life.Agent
					}
					originalErr := err
					err = nil
					fallbackErr = verify()
					if fallbackErr == nil && !incompleteRan {
						incompleteRan = true
						fallbackErr = incomplete()
					}
					if fallbackErr == nil {
						fallbackErr = e.settleAgentTask(ctx, ec, life)
					}
					if fallbackErr == nil {
						result.Status = RunStatusSucceeded
						result.Error = ""
						result.FailureClass = ""
					} else {
						err = originalErr
					}
				}
			}
			if fallbackErr != nil {
				err = errors.Join(err, fallbackErr)
			}
		}
		if err == nil && completionAware(step) && result.Outputs["complete"] == true {
			_, err = run("onComplete", step.OnComplete)
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
		if strings.HasPrefix(step.Uses, "agent/") || step.Conversation != nil || step.With["output"] != nil {
			output, err := resolveNamedOutput(w, step.With["output"])
			if err != nil {
				return err
			}
			if output != nil {
				steps[i].With["output"] = output
			}
		}
		for _, list := range [][]Step{step.OnResult, step.OnComplete, step.OnIncomplete, step.OnError, step.OnFailure, step.Finally, step.Parallel, step.Sequence} {
			if err := expandLifecycleOutputs(w, list); err != nil {
				return err
			}
		}
	}
	return nil
}
