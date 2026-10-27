package workflows

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/operational"
)

// Adapters must classify provider failures explicitly. Arbitrary execution,
// storage, instruction, checkout, ownership and cleanup errors never skip items.
var ErrProviderFailure = errors.New("classified provider failure")
var ErrUsageExhausted = errors.New("provider usage exhausted")

var errIterationBlocked = errors.New("ordered iteration blocked")

func iterationChildID(parent, job, key string, attempt int) string {
	return actionHash([]any{parent, job, key, attempt})[:32]
}

func iterationItemsValue(items []IterationItemResult) []any {
	out := make([]any, 0, len(items))
	for _, r := range items {
		out = append(out, map[string]any{"key": r.Key, "number": r.Number, "attempt": r.Attempt, "child_id": r.ChildID, "status": r.Status, "outputs": r.Outputs, "failure_class": r.FailureClass, "error": r.Error, "jobs": jobsValue(r.Jobs)})
	}
	return out
}
func hasOrderedIteration(all map[string]*Workflow) bool {
	for _, w := range all {
		for _, j := range w.Jobs {
			if j.ForEach != nil {
				return true
			}
		}
	}
	return false
}
func iterationRetry(run *Run) bool {
	for _, job := range run.Jobs {
		if iterationRetry(&Run{Jobs: job.ChildJobs}) {
			return true
		}
		for _, item := range job.Items {
			if item.Status != "completed" || iterationRetry(&Run{Jobs: item.Jobs}) {
				return true
			}
		}
	}
	return false
}
func (e *Executor) iterationEvidence(ctx context.Context, req RunRequest) error {
	j := iterationJournal(e.Store)
	if j == nil {
		return fmt.Errorf("durable IterationJournal unavailable")
	}
	if err := j.CheckIteration(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrActionIntegrity, err)
	}
	origin := nativeString(req.Metadata, "origin_run_id")
	if origin == "" {
		origin = req.runID
	}
	if e.Ready != nil {
		if err := e.Ready(ctx); err != nil {
			return fmt.Errorf("%w: %v", operational.ErrUnavailable, err)
		}
	}
	return e.validateActionRecovery(ctx, origin, Snapshot{ActionContracts: e.actionContracts})
}
func (e *Executor) iterationPlan(id string, job Job, inputs map[string]any, req RunRequest, needs map[string]JobExecution, all map[string]*Workflow) (IterationPlan, error) {
	ec := expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Needs: needs, Metadata: req.Metadata}
	raw := job.ForEach.Items
	if text, ok := raw.(string); ok {
		var err error
		raw, err = renderString(text, ec)
		if err != nil {
			return IterationPlan{}, err
		}
	}
	// Normalize Go callers' slices/maps and YAML numbers to JSON data without
	// interpreting strings inside the returned payload as expression source.
	b, err := actionJSON(raw)
	if err != nil {
		return IterationPlan{}, err
	}
	var items []any
	if string(b) == "null" {
		return IterationPlan{}, fmt.Errorf("for-each.items must return an array")
	}
	if err = decodeJSONWithNumbers(b, &items); err != nil {
		return IterationPlan{}, fmt.Errorf("for-each.items must return an array: %w", err)
	}
	p := IterationPlan{RunID: req.runID, JobID: id, Items: []IterationPlanItem{}}
	seen := map[string]bool{}
	for i, item := range items {
		ec.Item = item
		ec.Iteration = map[string]any{"number": i + 1}
		expr := strings.TrimSpace(job.ForEach.Key)
		if wholeExpression(expr) {
			expr = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expr, "${{"), "}}"))
		}
		keyValue, err := evalExpression(expr, ec)
		if err != nil {
			return p, err
		}
		key, ok := keyValue.(string)
		if !ok || strings.TrimSpace(key) == "" || seen[key] {
			return p, fmt.Errorf("for-each item %d: missing, non-string or duplicate key %q", i+1, key)
		}
		seen[key] = true
		ec.Iteration["key"] = key
		args, err := renderMap(job.With, ec)
		if err != nil {
			return p, err
		}
		secrets, err := callSecrets(job.Secrets, req.Secrets, ec)
		if err != nil {
			return p, err
		}
		ref, _ := CanonicalLocalRef(job.Uses)
		args, err = resolveInputs(all[ref].On.WorkflowCall, args, secrets)
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, IterationPlanItem{Key: key, Number: i + 1, Item: item, Inputs: args})
	}
	contracts := req.actionContracts
	if len(contracts) == 0 {
		contracts = nil
	}
	event := req.Event
	if len(event) == 0 {
		event = nil
	}
	metadata, _ := cloneJSONValue(req.Metadata).(map[string]any)
	delete(metadata, "iteration_ancestors")
	p.Fingerprint = actionHash([]any{p.Items, job, all, contracts, e.Roles, event, metadata, req.Execution})
	// Identity data must be persisted exactly, never with substituted secrets.
	b, err = actionJSON(p)
	if err == nil {
		_, err = e.actionIdentityJSON(b)
	}
	return p, err
}
func callSecrets(spec any, parent map[string]string, ec expressionContext) (map[string]string, error) {
	out := map[string]string{}
	switch m := spec.(type) {
	case nil:
	case string:
		if m != "inherit" {
			return nil, fmt.Errorf("secrets must be inherit or a mapping")
		}
		return parent, nil
	case map[string]any:
		for key, v := range m {
			value, err := renderValue(v, ec)
			if err != nil {
				return nil, err
			}
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("secret %s must be a string", key)
			}
			out[key] = text
		}
	default:
		return nil, fmt.Errorf("secrets must be inherit or a mapping")
	}
	return out, nil
}
func (e *Executor) executeIteration(ctx context.Context, id string, job Job, inputs map[string]any, req RunRequest, needs map[string]JobExecution, all map[string]*Workflow, slots chan struct{}, depth int) JobExecution {
	out := JobExecution{ID: id, Status: RunStatusSucceeded, Outputs: map[string]any{}}
	finish := func() JobExecution {
		completed := 0
		blocked := false
		for _, item := range out.Items {
			if item.Status == "completed" {
				completed++
			}
			if item.Status != "completed" && item.Status != "exhausted" {
				blocked = true
			}
		}
		out.Complete = completed > 0 && !blocked && out.Status != RunStatusNeedsAttention && out.Status != RunStatusCanceled
		out.Outputs = map[string]any{"items": iterationItemsValue(out.Items), "complete": out.Complete}
		return out
	}
	fail := func(err error) JobExecution {
		out.Status = RunStatusNeedsAttention
		out.Error = err.Error()
		out.FailureClass = classifyFailure(err)
		return finish()
	}
	ec := expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Needs: needs}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	for dep, r := range needs {
		if r.Status == RunStatusNeedsAttention || (job.If == "" && r.Status != RunStatusSucceeded && !(r.Status == RunStatusFailed && allMustJob(all, req.Ref, dep).ContinueOnError)) {
			out.Status = RunStatusSkipped
			return finish()
		}
	}
	allowed, err := evalIf(job.If, ec)
	if err != nil {
		return fail(err)
	}
	if !allowed {
		out.Status = RunStatusSkipped
		return finish()
	}
	journal := iterationJournal(e.Store)
	if journal == nil {
		return fail(fmt.Errorf("durable IterationJournal unavailable"))
	}
	checkpoint, err := journal.IterationRun(ctx, req.runID, nativeString(req.Metadata, "iteration_logical_run_id"), id)
	if err != nil {
		return fail(err)
	}
	planReq := req
	planReq.runID = checkpoint
	p, err := e.iterationPlan(id, job, inputs, planReq, needs, all)
	if err != nil {
		return fail(err)
	}
	if err = journal.PinIteration(ctx, p); err != nil {
		return fail(err)
	}
	records, err := journal.IterationAttempts(ctx, p.RunID, id)
	if err != nil {
		return fail(err)
	}
	latest := map[string]IterationItemResult{}
	for _, r := range records {
		latest[r.Key] = r
	}
	out.Items = make([]IterationItemResult, len(p.Items))
	for i, item := range p.Items {
		out.Items[i] = IterationItemResult{PlanFingerprint: p.Fingerprint, Key: item.Key, Number: item.Number, Status: "pending"}
		if old, ok := latest[item.Key]; ok {
			out.Items[i] = old
		}
	}
	for i, item := range p.Items {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		if err = e.iterationEvidence(ctx, req); err != nil {
			return fail(err)
		}
		last, exists := latest[item.Key]
		if exists && last.Status == "completed" {
			out.Items[i] = last
			continue
		}
		// Recover a child's durable terminal record after a lost item checkpoint.
		if exists {
			child, childSnap, loadErr := journal.Load(ctx, last.ChildID)
			if loadErr == nil && child.Status != RunStatusSucceeded && len(child.Jobs) == len(all[child.WorkflowRef].Jobs) {
				allSucceeded := true
				for _, job := range child.Jobs {
					if job.Status != RunStatusSucceeded {
						allSucceeded = false
					}
				}
				if allSucceeded {
					childSecrets, secretErr := callSecrets(job.Secrets, req.Secrets, expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Needs: needs, Item: item.Item, Iteration: map[string]any{"number": item.Number, "key": item.Key}})
					if secretErr != nil {
						return fail(secretErr)
					}
					child, loadErr = e.execute(ctx, child.WorkflowRef, RunRequest{Ref: child.WorkflowRef, runID: child.ID, Inputs: childSnap.Inputs, Secrets: childSecrets, SecretRefs: req.SecretRefs, Metadata: childSnap.Metadata, Event: req.Event, Execution: req.Execution, actionContracts: req.actionContracts, RetryUncertain: req.RetryUncertain, ordered: true}, all, slots, depth+1, child)
				}
			}
			if loadErr == nil && child.Status == RunStatusSucceeded && !iterationRetry(child) {
				last.Status = "completed"
				last.Outputs = child.Outputs
				last.Jobs = child.Jobs
				last.Error = ""
				last.FailureClass = ""
				if err = e.appendIteration(ctx, journal, p.RunID, id, &last); err != nil {
					return fail(err)
				}
				out.Items[i] = last
				continue
			}
			if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
				return fail(loadErr)
			}
			// Preserve the old uncertain observation before allocating a new child.
			if latest[item.Key].Status == "pending" {
				last.Status = "interrupted"
				last.FailureClass = "owner_loss"
				last.Error = "child completion was not committed"
				if err = e.appendIteration(ctx, journal, p.RunID, id, &last); err != nil {
					return fail(err)
				}
			}
		}
		if exists {
			if err = e.guardIterationRetry(ctx, last, req); err != nil {
				out.Items[i] = last
				return fail(err)
			}
		}
		r := IterationItemResult{PlanFingerprint: p.Fingerprint, Key: item.Key, Number: item.Number, Attempt: last.Attempt + 1, Status: "pending"}
		r.ChildID = iterationChildID(p.RunID, id, item.Key, r.Attempt)
		if err = e.appendIteration(ctx, journal, p.RunID, id, &r); err != nil {
			return fail(err)
		}
		out.Items[i] = r
		ec.Item = item.Item
		ec.Iteration = map[string]any{"key": item.Key, "number": item.Number}
		secrets, err := callSecrets(job.Secrets, req.Secrets, ec)
		if err != nil {
			return fail(err)
		}
		ref, _ := CanonicalLocalRef(job.Uses)
		metadata, _ := cloneJSONValue(req.Metadata).(map[string]any)
		if metadata == nil {
			metadata = map[string]any{}
		}
		ancestors, _ := metadata["iteration_ancestors"].([]any)
		metadata["iteration_ancestors"] = append(ancestors, r.ChildID)
		logical := nativeString(req.Metadata, "iteration_logical_run_id")
		if logical == "" {
			logical = req.runID
		}
		metadata["iteration_logical_run_id"] = actionHash([]any{logical, id, item.Key})
		childReq := RunRequest{Ref: ref, Inputs: item.Inputs, Secrets: secrets, SecretRefs: req.SecretRefs, Event: req.Event, Metadata: metadata, Execution: req.Execution, actionContracts: req.actionContracts, RetryUncertain: req.RetryUncertain, runID: r.ChildID, ordered: true}
		child, childErr := e.execute(ctx, ref, childReq, all, slots, depth+1, nil)
		r.Status = "completed"

		if child != nil {
			r.Outputs = child.Outputs
			r.Jobs = child.Jobs
			r.FailureClass = child.FailureClass
			r.Error = child.Error
		}
		if child != nil && iterationRetry(child) && childErr == nil {
			r.Status = "exhausted"
			r.FailureClass = "usage_exhausted"
			r.Error = "nested iteration contains exhausted skips"
		}
		if childErr == nil && child != nil && iterationRetry(child) {
			switch nestedIterationOutcome(child.Jobs) {
			case "provider_failure":
				childErr = fmt.Errorf("%w: nested iteration contains unresolved provider failures", ErrProviderFailure)
			case "interrupted", "pending":
				childErr = fmt.Errorf("%w: nested iteration is unfinished", errIterationBlocked)
			}
		}
		stop := false
		if childErr != nil {
			r.Error = childErr.Error()
			r.FailureClass = classifyFailure(childErr)
			switch {
			case child != nil && child.Status == RunStatusNeedsAttention:
				r.Status = "interrupted"
				stop = true
			case ctx.Err() != nil:
				r.Status = "interrupted"
				stop = true
			case r.FailureClass == "usage_exhausted":
				r.Status = "exhausted"
			case r.FailureClass == "provider_failure":
				r.Status = "provider_failure"
				out.Status = RunStatusFailed
				out.FailureClass = r.FailureClass
				out.Error = r.Error
			default:
				r.Status = "interrupted"
				stop = true
			}
		}
		if err = e.appendIteration(ctx, journal, p.RunID, id, &r); err != nil {
			return fail(errors.Join(childErr, err))
		}
		out.Items[i] = r
		if stop {
			return fail(childErr)
		}
	}
	return finish()
}
func (e *Executor) appendIteration(ctx context.Context, j IterationJournal, run, job string, r *IterationItemResult) error {
	r.Error = Redact(r.Error, e.redactionValues)
	r.Outputs, _ = redactValue(r.Outputs, e.redactionValues).(map[string]any)
	if r.Jobs != nil {
		r.Jobs = redactRun(&Run{Jobs: r.Jobs}, e.redactionValues).Jobs
	}
	r.Fingerprint = iterationResultHash(*r)
	return j.AppendIteration(context.WithoutCancel(ctx), run, job, *r)
}

// Fresh physical children cannot silently replay prior mutations. Read-only
// attempts can be retried after the usual uncertainty acknowledgement. Mutation
// reconciliation must prove completion or no effect before a fresh child is safe.
func (e *Executor) guardIterationRetry(ctx context.Context, last IterationItemResult, req RunRequest) error {
	j := actionJournal(e.Store)
	if j != nil {
		origin := nativeString(req.Metadata, "origin_run_id")
		records, err := j.ActionAttempts(ctx, origin)
		if err != nil {
			return err
		}
		latest := map[string]ActionAttempt{}
		for _, r := range records {
			latest[r.Context.OperationKey] = r
		}
		for _, r := range latest {
			// Descendants share the origin; identify ancestry from their admitted metadata.
			if r.Context.RunID != last.ChildID {
				_, snap, err := j.Load(ctx, r.Context.RunID)
				if err != nil {
					return err
				}
				ancestors, _ := snap.Metadata["iteration_ancestors"].([]any)
				found := false
				for _, a := range ancestors {
					if a == last.ChildID {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			if r.Contract.Effect != ActionExternalMutation {
				continue
			}
			if r.State == "no_effect" {
				continue
			}
			if r.State != "completed" {
				generation := r.Context.Generation
				if fence, ok := ctx.Value(fenceKey{}).(executionFence); ok {
					generation = fence.generation
				}
				if _, err = e.reconcileAction(ctx, j, &r, generation); err != nil {
					err = fmt.Errorf("%w: %v", ErrActionIntegrity, err)

					return err
				}
				if r.State == "no_effect" {
					continue
				}
			}
			// Completed mutations are reused by stable logical operation identity
			// inside the fresh physical child; changed inputs still block replay.
		}
	}
	if !req.RetryUncertain {
		return fmt.Errorf("explicit --retry-uncertain is required for item %s", last.Key)
	}
	return nil
}

// Invalidate every dependent aggregate when an item will be retried. Completed
// effect inputs are checked again by the step/action journals before replay.
func resetIterationDependents(run *Run, all map[string]*Workflow) {
	w := all[run.WorkflowRef]
	if w == nil {
		return
	}
	dirty := map[string]bool{}
	for id, j := range w.Jobs {
		r, ok := run.Jobs[id]
		if !ok || r.Status != RunStatusSucceeded || (j.ForEach != nil && itemsIncomplete(r.Items)) || iterationRetry(&Run{Jobs: r.ChildJobs}) {
			dirty[id] = true
		}
	}
	order, _ := topoJobs(w.Jobs)
	for _, id := range order {
		for _, dep := range w.Jobs[id].Needs {
			if dirty[dep] {
				dirty[id] = true
			}
		}
		if dirty[id] {
			delete(run.Jobs, id)
		}
	}
}
func itemsIncomplete(items []IterationItemResult) bool {
	for _, r := range items {
		if r.Status != "completed" {
			return true
		}
	}
	return false
}

func (e *Executor) beginIterationStep(ctx context.Context, j Journal, step Step, ec expressionContext, retry bool) (Attempt, bool, error) {
	// Rendering once treats referenced item values as data, even when they contain
	// expression delimiters. Only fields used by dispatch enter this fingerprint.
	source := map[string]any{"uses": step.Uses, "with": step.With, "run": step.Run, "shell": step.Shell, "working-directory": step.WorkingDirectory, "timeout": step.Timeout}
	argv := make([]any, len(step.Argv))
	for i, arg := range step.Argv {
		argv[i] = arg
	}
	source["argv"] = argv
	rendered, err := renderMap(source, ec)
	if err != nil {
		return Attempt{}, false, err
	}
	fingerprint := actionHash(rendered)
	input := iterationJournal(e.Store)
	if input == nil {
		return Attempt{}, false, fmt.Errorf("ordered iteration requires durable IterationJournal")
	}
	return input.BeginInput(ctx, ec.RunID, ec.JobID, step.ID, ec.IterationNumber, retry, fingerprint, strings.HasPrefix(step.Uses, "agent/"), nativeString(ec.Metadata, "iteration_logical_run_id"))
}

func jobsValue(jobs map[string]JobExecution) map[string]any {
	b, _ := json.Marshal(jobs)
	var v map[string]any
	_ = decodeJSONWithNumbers(b, &v)
	return v
}

// Validate the journal even when all jobs would otherwise take a success
// shortcut. Traverse physical children so nested plans and result fingerprints
// receive the same checks as a top-level iteration.
func (e *Executor) validateIterationRecovery(ctx context.Context, run *Run, snap Snapshot, secrets map[string]string) error {
	if !hasOrderedIteration(snap.Definitions) {
		return nil
	}
	j := iterationJournal(e.Store)
	if j == nil {
		return fmt.Errorf("durable IterationJournal unavailable")
	}
	seen := map[string]bool{}
	var visit func(*Run, Snapshot, map[string]string) error
	visit = func(r *Run, s Snapshot, credentials map[string]string) error {
		if seen[r.ID] {
			return nil
		}
		seen[r.ID] = true
		for ref, w := range s.Definitions {
			if actionHash(w) != s.Hashes[ref] {
				return fmt.Errorf("%w: snapshot hash mismatch for %s", ErrActionIntegrity, ref)
			}
		}
		w := s.Definitions[r.WorkflowRef]
		if w == nil {
			return fmt.Errorf("%w: missing child definition", ErrActionIntegrity)
		}
		inputs, err := resolveInputs(w.On.WorkflowCall, s.Inputs, credentials)
		if err != nil {
			return err
		}
		req := RunRequest{Ref: r.WorkflowRef, runID: r.ID, Inputs: inputs, Secrets: credentials, Event: s.Event, Metadata: s.Metadata, actionContracts: s.ActionContracts, Execution: s.Execution}
		for id, job := range w.Jobs {
			needs := map[string]JobExecution{}
			for _, dep := range job.Needs {
				if result, ok := r.Jobs[dep]; ok {
					needs[dep] = result
				}
			}
			ec := expressionContext{Inputs: inputs, Secrets: credentials, Event: s.Event, Needs: needs}
			if job.ForEach != nil {
				checkpoint, err := j.IterationRun(ctx, r.ID, nativeString(s.Metadata, "iteration_logical_run_id"), id)
				if err != nil {
					return err
				}
				planReq := req
				planReq.runID = checkpoint
				plan, err := j.IterationPlan(ctx, checkpoint, id)
				if errors.Is(err, sql.ErrNoRows) {
					if len(r.Jobs[id].Items) > 0 {
						return fmt.Errorf("%w: iteration plan missing", ErrActionIntegrity)
					}
					continue
				}
				if err != nil {
					return err
				}
				// A changed dependency can legitimately change a pending job's list. It
				// will be rejected against the pinned plan at dispatch, before any effect.
				if len(needs) == len(job.Needs) {
					expected, err := e.iterationPlan(id, job, inputs, planReq, needs, s.Definitions)
					if err != nil {
						return err
					}
					if actionHash(plan) != actionHash(expected) {
						return fmt.Errorf("%w: retained ordered plan changed for %s", ErrActionIntegrity, id)
					}
				}
				records, err := j.IterationAttempts(ctx, checkpoint, id)
				if err != nil {
					return err
				}
				for _, record := range records {
					if record.Fingerprint != iterationResultHash(record) || record.PlanFingerprint != plan.Fingerprint || record.Number < 1 || record.Number > len(plan.Items) || record.Key != plan.Items[record.Number-1].Key || record.ChildID != iterationChildID(checkpoint, id, record.Key, record.Attempt) {
						return fmt.Errorf("%w: retained iteration identity changed", ErrActionIntegrity)
					}
					child, childSnap, err := j.Load(ctx, record.ChildID)
					if errors.Is(err, sql.ErrNoRows) && record.Status == "pending" {
						continue
					}
					if err != nil {
						return err
					}
					if record.Status == "completed" && (child.Status != RunStatusSucceeded || actionHash(child.Outputs) != actionHash(record.Outputs) || actionHash(child.Jobs) != actionHash(record.Jobs)) {
						return fmt.Errorf("%w: completed child result changed", ErrActionIntegrity)
					}
					ec.Item = plan.Items[record.Number-1].Item
					ec.Iteration = map[string]any{"number": record.Number, "key": record.Key}
					childSecrets, err := callSecrets(job.Secrets, credentials, ec)
					if err != nil {
						return err
					}
					if err = visit(child, childSnap, childSecrets); err != nil {
						return err
					}
				}
			} else if job.Uses != "" {
				for _, iteration := range r.Jobs[id].Iterations {
					hash := sha256.Sum256([]byte(r.ID + "/" + id + fmt.Sprint(iteration.Number)))
					childID := hex.EncodeToString(hash[:16])
					child, childSnap, err := j.Load(ctx, childID)
					if errors.Is(err, sql.ErrNoRows) && iteration.Status == RunStatusSkipped {
						continue
					}
					if err != nil {
						return err
					}
					ec.Iteration = map[string]any{"number": iteration.Number}
					childSecrets, err := callSecrets(job.Secrets, credentials, ec)
					if err != nil {
						return err
					}
					if err = visit(child, childSnap, childSecrets); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return visit(run, snap, secrets)
}

func nestedIterationOutcome(jobs map[string]JobExecution) string {
	severity := map[string]int{"": 0, "completed": 0, "exhausted": 1, "provider_failure": 2, "pending": 3, "interrupted": 4}
	outcome := ""
	retain := func(value string) {
		if severity[value] > severity[outcome] {
			outcome = value
		}
	}
	for _, job := range jobs {
		retain(nestedIterationOutcome(job.ChildJobs))
		for _, item := range job.Items {
			retain(item.Status)
			retain(nestedIterationOutcome(item.Jobs))
		}
	}
	return outcome
}
