package workflows

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
	RunStatusSkipped   = "skipped"
	RunStatusCanceled  = "canceled"
)

type Run struct {
	Usage        *UsageSummary           `json:"normalized_usage,omitempty"`
	Generation   int64                   `json:"execution_generation,omitempty"`
	Owner        string                  `json:"-"`
	FailureClass string                  `json:"failure_class,omitempty"`
	OriginID     string                  `json:"origin_run_id,omitempty"`
	Version      int                     `json:"version,omitempty"`
	ID           string                  `json:"id"`
	WorkflowRef  string                  `json:"workflow_ref"`
	Status       string                  `json:"status"`
	StartedAt    time.Time               `json:"started_at"`
	FinishedAt   time.Time               `json:"finished_at,omitempty"`
	Jobs         map[string]JobExecution `json:"jobs"`
	Outputs      map[string]any          `json:"outputs,omitempty"`
	Error        string                  `json:"error,omitempty"`
}

type RunStore interface {
	Save(context.Context, *Run) error
}

type Executor struct {
	// BeforeFinalize retains execution ownership until supervised workers stop.
	BeforeFinalize   func(context.Context, string) error
	Capacity         *Capacity
	BindTools        ToolBinder
	CommandExecution runtimeexec.Policy
	Ready            func(context.Context) error

	redactionValues    map[string]string
	Actions            ActionRunner
	ApplicationActions *ActionRegistry
	applicationActions map[string]*registeredAction
	actionContracts    map[string]ActionContract
	ActionFactory      func(json.RawMessage) (ActionRunner, error)
	WorkspaceDir       string
	DefinitionsDir     string
	Agents             AgentRunner
	Store              RunStore
	MaxParallel        int
	Roles              json.RawMessage
	RoleRunner         func(json.RawMessage) (AgentRunner, error)
	Slots              chan struct{}
}

type RunRequest struct {
	Execution       runtimeexec.Policy
	RequestID       string
	Metadata        map[string]any
	Ref             string
	Inputs          map[string]any
	Secrets         map[string]string
	Event           map[string]any
	SecretRefs      map[string]string
	RetryUncertain  bool
	actionContracts map[string]ActionContract
	runID           string
	iteration       int
	ordered         bool
}

// Load snapshots and validates the entire reusable workflow closure before any
// subprocess starts. Definitions cannot change underneath an admitted run.
func (e *Executor) Load(ref string) (map[string]*Workflow, error) {
	all := map[string]*Workflow{}
	active := map[string]bool{}
	var load func(string, int) error
	load = func(ref string, depth int) error {
		if depth > 8 {
			return fmt.Errorf("reusable workflow depth exceeds 8")
		}
		resolved, err := (Resolver{WorkspaceDir: e.WorkspaceDir, DefinitionsDir: e.DefinitionsDir}).ResolveLocal(ref)
		if err != nil {
			return err
		}
		ref = resolved.Canonical
		if active[ref] {
			return fmt.Errorf("reusable workflow cycle at %s", ref)
		}
		if all[ref] != nil {
			return nil
		}
		f, err := os.Open(resolved.Path)
		if err != nil {
			return err
		}
		data, readErr := readDefinition(f)
		_ = f.Close()
		if readErr != nil {
			return readErr
		}
		w, err := Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		if err := Validate(w); err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
		if len(all) >= 128 {
			return fmt.Errorf("workflow closure exceeds 128 definitions")
		}
		active[ref] = true
		all[ref] = w
		for _, j := range w.Jobs {
			if j.Uses != "" {
				if err := load(j.Uses, depth+1); err != nil {
					return err
				}
				canonical, _ := CanonicalLocalRef(j.Uses)
				if all[canonical].On.WorkflowCall == nil {
					return fmt.Errorf("%s must declare on.workflow_call", canonical)
				}
			}
		}
		active[ref] = false
		return nil
	}
	if err := load(ref, 0); err != nil {
		return nil, err
	}
	if catalog, ok := e.Agents.(interface{ HasAgent(string) bool }); ok {
		for ref, w := range all {
			for _, j := range w.Jobs {
				for _, s := range j.Steps {
					if strings.HasPrefix(s.Uses, "agent/") && !catalog.HasAgent(strings.TrimPrefix(s.Uses, "agent/")) {
						return nil, fmt.Errorf("%s references unconfigured agent %q", ref, s.Uses)
					}
				}
			}
		}
	}
	return all, nil
}

func (e *Executor) Run(ctx context.Context, req RunRequest) (*Run, error) {
	// Each run gets its own credential redactor; the executor remains reusable.
	if e.Slots != nil && cap(e.Slots) == 0 {
		return nil, fmt.Errorf("execution capacity must be positive")
	}
	copy := *e
	e = &copy
	req.Execution = e.CommandExecution
	e.redactionValues = e.redactions(req.Secrets)
	if j, ok := e.Store.(Journal); ok {
		e.Store = redactedStore{Journal: j, secrets: e.redactions(req.Secrets)}
	}
	if source, ok := e.Agents.(interface {
		AdmittedRoles(context.Context) (json.RawMessage, error)
	}); ok {
		roles, err := source.AdmittedRoles(ctx)
		if err != nil {
			return nil, err
		}
		e.Roles = roles
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
	limit := e.MaxParallel
	if limit <= 0 {
		limit = 4
	}

	// Detach caller-owned input maps before workers read them.
	req.Inputs, _ = cloneJSONValue(req.Inputs).(map[string]any)
	req.Event, _ = cloneJSONValue(req.Event).(map[string]any)
	secrets := map[string]string{}
	for k, v := range req.Secrets {
		secrets[k] = v
	}
	req.Secrets = secrets
	slots := e.Slots
	if slots == nil {
		slots = make(chan struct{}, limit)
	}
	run, runErr := e.execute(ctx, ref, req, all, slots, 0, nil)
	return safeResult(run, runErr, e.redactionValues)
}

func (e *Executor) execute(ctx context.Context, ref string, req RunRequest, all map[string]*Workflow, slots chan struct{}, depth int, existing *Run) (*Run, error) {
	if depth > 8 {
		return nil, fmt.Errorf("reusable workflow depth exceeds 8")
	}
	w := all[ref]
	req.ordered = req.ordered || hasOrderedIteration(all)
	inputs, err := resolveInputs(w.On.WorkflowCall, req.Inputs, req.Secrets)
	if err != nil {
		return nil, err
	}
	run := existing
	if run == nil {
		id := req.runID
		if id == "" {
			id = randomID()
		}
		run = &Run{Version: 1, ID: id, WorkflowRef: ref, Status: RunStatusRunning, StartedAt: time.Now().UTC(), Jobs: map[string]JobExecution{}}
		if req.Metadata == nil {
			req.Metadata = map[string]any{}
		}
		if req.Metadata["origin_run_id"] == nil {
			req.Metadata["origin_run_id"] = run.ID
		}
		run.OriginID = nativeString(req.Metadata, "origin_run_id")
		if journal, ok := e.Store.(Journal); ok {
			snap, err := snapshotFor(all, req, e.Roles, e.MaxParallel)
			if err != nil {
				return run, err
			}
			if err = journal.Admit(ctx, run, snap); err != nil {
				return run, err
			}
		} else if err := e.save(ctx, run); err != nil {
			return run, err
		}
	} else {
		run.Status = RunStatusRunning
		run.Error = ""
		run.Outputs = nil
		run.FinishedAt = time.Time{}
		if err := e.save(ctx, run); err != nil {
			return run, err
		}
	}
	if depth == 0 {
		ctx = fenceContext(ctx, run)
	}
	req.runID = run.ID
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		id     string
		result JobExecution
	}
	done := make(chan completion, len(w.Jobs))
	started := map[string]bool{}
	for id := range run.Jobs {
		started[id] = true
	}
	active := 0
	order, _ := topoJobs(w.Jobs)
	var storeErr error
	for len(run.Jobs) < len(w.Jobs) {
		for _, id := range order {
			if started[id] {
				continue
			}
			job := w.Jobs[id]
			ready := true
			needs := map[string]JobExecution{}
			for _, dep := range job.Needs {
				result, ok := run.Jobs[dep]
				if !ok || result.Status == RunStatusWaiting || result.Status == RunStatusInterrupted {
					ready = false
					break
				}
				needs[dep] = result
			}
			if !ready {
				continue
			}
			started[id] = true
			active++
			go func(id string, job Job, needs map[string]JobExecution) {
				result := e.executeJob(ctx, id, job, inputs, req, needs, all, slots, depth)
				done <- completion{id, result}
			}(id, job, needs)
		}
		if active == 0 {
			break
		}
		finished := <-done
		active--
		run.Jobs[finished.id] = finished.result
		if storeErr == nil {
			if err := e.save(context.WithoutCancel(ctx), run); err != nil {
				storeErr = err
				cancel()
			}
		}
	}
	run.Status = RunStatusSucceeded
	var runErr error
	needsAttention := false
	waiting := false
	for _, id := range order {
		job := run.Jobs[id]
		if job.Status == RunStatusWaiting || job.Status == RunStatusInterrupted {
			waiting = true
		}
		if job.Status == RunStatusNeedsAttention {
			needsAttention = true
		}
		if (job.Status == RunStatusFailed && (!w.Jobs[id].ContinueOnError || (req.ordered && w.Jobs[id].ForEach == nil))) || job.Status == RunStatusNeedsAttention {
			jobErr := fmt.Errorf("job %s: %s", id, job.Error)
			if job.FailureClass == "provider_failure" {
				jobErr = fmt.Errorf("%w: %v", ErrProviderFailure, jobErr)
			}
			if job.FailureClass == "usage_exhausted" {
				jobErr = fmt.Errorf("%w: %v", ErrUsageExhausted, jobErr)
			}
			if req.ordered && job.FailureClass != "provider_failure" && job.FailureClass != "usage_exhausted" {
				jobErr = fmt.Errorf("%w: %w", errIterationBlocked, jobErr)
			}
			if job.FailureClass == "action_integrity" {
				jobErr = fmt.Errorf("%w: %v", ErrActionIntegrity, jobErr)
			}
			runErr = errors.Join(runErr, jobErr)
		}
	}
	if waiting {
		run.Status = RunStatusWaiting
	}
	if ctx.Err() != nil {
		run.Status = RunStatusCanceled
		runErr = errors.Join(runErr, ctx.Err())
		if ctx.Err() == context.Canceled && !cancellationOnly(runErr) {
			run.Status = RunStatusFailed
		}
	} else if runErr != nil {
		run.Status = RunStatusFailed
	}
	if needsAttention || errors.Is(runErr, process.ErrCleanup) {
		run.Status = RunStatusNeedsAttention
	}
	if storeErr != nil {
		run.Status = RunStatusFailed
		runErr = errors.Join(runErr, iterationPersistence(storeErr, req.ordered))
	}
	if w.On.WorkflowCall != nil && runErr == nil && run.Status == RunStatusSucceeded {
		run.Outputs = map[string]any{}
		for key, output := range w.On.WorkflowCall.Outputs {
			value, err := renderString(output.Value, expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Jobs: run.Jobs})
			if err != nil {
				runErr = err
				run.Status = RunStatusFailed
				break
			}
			run.Outputs[key] = value
		}
	}
	if runErr != nil {
		run.Error = runErr.Error()
	}
	if depth == 0 {
		if store := invocationStore(e.Store); store != nil {
			if err := store.BeginFinalization(context.WithoutCancel(ctx), run); err != nil {
				runErr = errors.Join(runErr, err)
				run.Status = RunStatusNeedsAttention
				run.Error = runErr.Error()
			}
		}
	}
	if depth == 0 && e.BeforeFinalize != nil {
		if err := e.BeforeFinalize(context.WithoutCancel(ctx), run.ID); err != nil {
			runErr = errors.Join(runErr, err)
			run.Status = RunStatusNeedsAttention
			run.Error = runErr.Error()
		}
	}
	if depth == 0 {
		if store := invocationStore(e.Store); store != nil {
			finalCtx := context.WithoutCancel(ctx)
			if err := errors.Join(store.ReconcileInvocations(finalCtx, run.ID), store.CheckInvocationCleanup(finalCtx, run.ID), store.CheckToolEffects(finalCtx, run.ID)); err != nil {
				runErr = errors.Join(runErr, err)
				run.Status = RunStatusNeedsAttention
				run.Error = runErr.Error()
			}
		}
	}
	if ctx.Err() != nil && run.Status != RunStatusNeedsAttention {
		run.Status = RunStatusCanceled
		runErr = errors.Join(runErr, ctx.Err())
		if ctx.Err() == context.Canceled && !cancellationOnly(runErr) {
			run.Status = RunStatusFailed
		}
		run.Error = runErr.Error()
	}
	if depth == 0 {
		if store := invocationStore(e.Store); store != nil {
			if report, usageErr := store.Usage(context.WithoutCancel(ctx), run.ID); usageErr != nil {
				runErr = errors.Join(runErr, usageErr)
				run.Status = RunStatusNeedsAttention
				run.Error = runErr.Error()
			} else {
				run.Usage = &report.Summary
			}
		}
	}
	run.FailureClass = classifyFailure(runErr)
	run.FinishedAt = time.Now().UTC()
	if j := externalInputJournal(e.Store); j != nil && depth == 0 {
		control, controlErr := j.InputControl(context.WithoutCancel(ctx), run.ID)
		if controlErr != nil {
			runErr = errors.Join(runErr, controlErr)
			run.Status = RunStatusNeedsAttention
			run.Error = runErr.Error()
		}
		if controlErr == nil && control.Paused && !control.Canceled && run.Status != RunStatusNeedsAttention && (runErr == nil || cancellationOnly(runErr)) {
			run.Status = RunStatusInterrupted
			runErr = nil
			run.Error = ""
			run.FailureClass = ""
		}
	}
	if run.Status == RunStatusWaiting || run.Status == RunStatusInterrupted {
		run.FinishedAt = time.Time{}
	}
	if err := e.save(context.WithoutCancel(ctx), run); err != nil {
		return run, errors.Join(runErr, iterationPersistence(err, req.ordered))
	}
	if run.Status == RunStatusInterrupted && cancellationOnly(runErr) {
		runErr = nil
	}
	return run, runErr
}

func (e *Executor) executeJob(ctx context.Context, id string, job Job, inputs map[string]any, req RunRequest, needs map[string]JobExecution, all map[string]*Workflow, slots chan struct{}, depth int) JobExecution {
	if job.ForEach != nil {
		return e.executeIteration(ctx, id, job, inputs, req, needs, all, slots, depth)
	}
	limit := 1
	if job.Repeat != nil {
		limit = job.Repeat.MaxAttempts
	}
	var history []JobIteration
	previous := map[string]any{}
	for iteration := 1; iteration <= limit; iteration++ {
		req.iteration = iteration
		result := e.executeJobOnce(ctx, id, job, inputs, req, needs, all, slots, depth, previous)
		history = append(history, JobIteration{Number: iteration, Status: result.Status, Outputs: result.Outputs, Steps: result.Steps, Error: result.Error})
		result.Iterations = history
		if job.Repeat == nil || result.Status != RunStatusSucceeded {
			return result
		}
		allowed, err := evalIf(job.Repeat.Until, expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Needs: needs, Steps: result.Steps, Iteration: map[string]any{"number": iteration, "previous": previous, "outputs": result.Outputs, "status": result.Status}})
		if err != nil {
			result.Status = executionFailureStatus(ctx, err)
			result.FailureClass = classifyFailure(err)
			result.Error = err.Error()
			return result
		}
		if allowed {
			return result
		}
		previous = map[string]any{"outputs": result.Outputs, "status": result.Status, "steps": stepsAsMap(result.Steps)}
		if iteration == limit {
			result.Status = RunStatusFailed
			result.Error = fmt.Sprintf("repeat exhausted after %d attempts", limit)
			result.FailureClass = "limit"
			return result
		}
	}
	panic("invalid repeat limit")
}

func stepsAsMap(steps map[string]StepExecution) map[string]any {
	m := map[string]any{}
	for id, s := range steps {
		m[id] = map[string]any{"outputs": s.Outputs, "status": s.Status, "error": s.Error}
	}
	return m
}

func (e *Executor) executeJobOnce(ctx context.Context, id string, job Job, inputs map[string]any, req RunRequest, needs map[string]JobExecution, all map[string]*Workflow, slots chan struct{}, depth int, previous map[string]any) (out JobExecution) {
	out = JobExecution{ID: id, Status: RunStatusSucceeded, Steps: map[string]StepExecution{}, Outputs: map[string]any{}}
	fail := func(err error) JobExecution {
		out.Status = executionFailureStatus(ctx, err)
		out.Error = err.Error()
		out.FailureClass = classifyFailure(err)
		return out
	}
	ec := expressionContext{Inputs: inputs, Secrets: req.Secrets, Event: req.Event, Needs: needs, Steps: out.Steps, Metadata: req.Metadata, RunID: req.runID, JobID: id, Ordered: req.ordered, IterationNumber: req.iteration, Iteration: map[string]any{"number": req.iteration, "previous": previous}}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	for _, result := range needs {
		if result.Status == RunStatusNeedsAttention {
			out.Status = RunStatusSkipped
			return out
		}
	}
	if job.If == "" {
		for dep, result := range needs {
			// A continued failure counts as successful for dependency admission.
			w := allMustJob(all, req.Ref, dep)
			if result.Status != RunStatusSucceeded && !(result.Status == RunStatusFailed && w.ContinueOnError) {
				out.Status = RunStatusSkipped
				return out
			}
		}
	}
	allowed, err := evalIf(job.If, ec)
	if err != nil {
		return fail(err)
	}
	if !allowed {
		out.Status = RunStatusSkipped
		return out
	}
	if job.Uses != "" {
		args, err := renderMap(job.With, ec)
		if err != nil {
			return fail(err)
		}
		childReq := RunRequest{ordered: req.ordered, Execution: req.Execution, Ref: job.Uses, Inputs: args, Event: req.Event, Secrets: map[string]string{}, Metadata: req.Metadata}
		switch secretMap := job.Secrets.(type) {
		case string:
			if secretMap != "inherit" {
				return fail(fmt.Errorf("secrets must be inherit or a mapping"))
			}
			childReq.Secrets = req.Secrets
		case map[string]any:
			for key, v := range secretMap {
				val, err := renderValue(v, ec)
				if err != nil {
					return fail(err)
				}
				s, ok := val.(string)
				if !ok {
					return fail(fmt.Errorf("secret %s must be a string", key))
				}
				childReq.Secrets[key] = s
			}
		case nil:
		default:
			return fail(fmt.Errorf("secrets must be inherit or a mapping"))
		}
		ref, _ := CanonicalLocalRef(job.Uses)
		childReq.actionContracts = req.actionContracts
		childReq.SecretRefs = req.SecretRefs
		childReq.RetryUncertain = req.RetryUncertain
		hash := sha256.Sum256([]byte(req.runID + "/" + id + fmt.Sprint(req.iteration)))
		childReq.runID = hex.EncodeToString(hash[:16])
		if logical := nativeString(req.Metadata, "iteration_logical_run_id"); req.ordered {
			if logical == "" {
				logical = req.runID
			}
			childReq.Metadata, _ = cloneJSONValue(req.Metadata).(map[string]any)
			childReq.Metadata["iteration_logical_run_id"] = actionHash([]any{logical, id, req.iteration})
		}
		var existing *Run
		if journal, ok := e.Store.(Journal); ok {
			saved, _, loadErr := journal.Load(ctx, childReq.runID)
			if loadErr == nil {
				if saved.Status == RunStatusSucceeded && !iterationRetry(saved) {
					out.ChildRunID = saved.ID
					out.ChildJobs = saved.Jobs
					out.Outputs = saved.Outputs
					return out
				}
				existing = saved
				resetIterationDependents(existing, all)
			} else if !errors.Is(loadErr, sql.ErrNoRows) {
				return fail(loadErr)
			}
		}
		child, err := e.execute(ctx, ref, childReq, all, slots, depth+1, existing)
		if err != nil {
			result := fail(err)
			if child != nil && child.Status == RunStatusNeedsAttention {
				result.Status = RunStatusNeedsAttention
			}
			return result
		}
		out.Status = child.Status
		out.ChildRunID = child.ID
		out.ChildJobs = child.Jobs
		out.Outputs = child.Outputs
		return out
	}
	for i, step := range job.Steps {
		sid := stepID(step, i)
		step.ID = sid
		ec.Attempt = nil
		result := StepExecution{ID: sid, Status: RunStatusSucceeded}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		allowed, err := evalIf(step.If, ec)
		if err == nil && !allowed {
			result.Status = RunStatusSkipped
			out.Steps[sid] = result
			continue
		}
		if err == nil && step.Uses == "input/wait" {
			result, err = e.executeInputWait(ctx, step, ec)
		} else if err == nil && isApplicationAction(step.Uses) {
			result, err = e.executeApplicationAction(ctx, step, ec)
		} else if err == nil {
			var attempt Attempt
			var reused bool
			journal, durable := e.Store.(Journal)
			if durable {
				if req.ordered {
					attempt, reused, err = e.beginIterationStep(ctx, journal, step, ec, req.RetryUncertain)
				} else {
					attempt, reused, err = journal.Begin(ctx, req.runID, id, sid, req.iteration, req.RetryUncertain)
				}
			}
			if err == nil && reused {
				result = attempt.Result
			} else if err == nil {
				result.StartedAt = time.Now().UTC()
				result.AttemptID = attempt.ID
				if durable {
					ec.Attempt = &attempt
				}
				result.Outputs, err = e.executeStep(invocationContext(ctx, e.Store, ec.Attempt), step, ec, slots)
				result.FinishedAt = time.Now().UTC()
				if err != nil {
					result.Status = executionFailureStatus(ctx, err)
					result.Error = err.Error()
					result.FailureClass = classifyFailure(err)
				}
				if durable {
					err = errors.Join(err, iterationPersistence(journal.Complete(context.WithoutCancel(ctx), attempt, result), req.ordered))
				}
			}
		}
		if err != nil {
			result.Status = executionFailureStatus(ctx, err)
			result.FailureClass = classifyFailure(err)
			result.Error = err.Error()
		}
		out.Steps[sid] = result
		if result.Status == RunStatusWaiting && err == nil {
			out.Status = RunStatusWaiting
			return out
		}
		if err != nil && (req.ordered || !step.ContinueOnError || hardExecutionFailure(err)) {
			return fail(err)
		}
	}
	for key, expr := range job.Outputs {
		value, err := renderString(expr, ec)
		if err != nil {
			return fail(err)
		}
		out.Outputs[key] = value
	}
	return out
}

func allMustJob(all map[string]*Workflow, ref, id string) Job {
	canonical, _ := CanonicalLocalRef(ref)
	if w := all[canonical]; w != nil {
		return w.Jobs[id]
	}
	return Job{}
}

func (e *Executor) executeStep(ctx context.Context, step Step, ec expressionContext, slots chan struct{}) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.Ready != nil {
		if err := e.Ready(ctx); err != nil {
			return nil, fmt.Errorf("%w: %v", operational.ErrUnavailable, err)
		}
	}
	if ec.Ordered {
		if err := e.iterationEvidence(ctx, RunRequest{runID: ec.RunID, Metadata: ec.Metadata}); err != nil {
			return nil, err
		}
	}
	if IsControllerAction(step.Uses) {
		if e.Actions == nil {
			return nil, fmt.Errorf("controller actions are not configured")
		}
		args, err := renderMap(step.With, ec)
		if err != nil {
			return nil, err
		}
		return e.Actions.RunAction(ctx, ActionRequest{RunID: ec.RunID, JobID: ec.JobID, StepID: step.ID, Iteration: ec.IterationNumber, Uses: step.Uses, With: args, Metadata: ec.Metadata, Secrets: e.redactions(ec.Secrets), Needs: ec.Needs, Steps: ec.Steps})
	}
	if len(step.Argv) > 0 || step.Run != "" {
		return e.executeCommand(ctx, step, ec, slots)
	}
	if e.Agents == nil {
		return nil, fmt.Errorf("agent runner is required")
	}
	args, err := renderMap(step.With, ec)
	if err != nil {
		return nil, err
	}
	prompt := nativeString(args, "prompt")
	if prompt == "" {
		prompt = nativeString(args, "message")
	}
	if prompt == "" {
		return nil, fmt.Errorf("agent prompt is empty or not a string")
	}
	workspace := nativeString(args, "workspace")
	if workspace != "" && !filepath.IsAbs(workspace) {
		workspace = filepath.Join(e.WorkspaceDir, workspace)
	}
	contract, err := ParseAgentOutputContract(args["output"])
	if err != nil {
		return nil, err
	}
	if contract != nil {
		if err := contract.Validate(); err != nil {
			return nil, err
		}
		originalSchema, _ := json.Marshal(contract.Schema)
		safeSchema, _ := json.Marshal(redactValue(contract.Schema, e.redactions(ec.Secrets)))
		if string(originalSchema) != string(safeSchema) {
			return nil, fmt.Errorf("structured schema contains credential material; use a credential reference outside the schema")
		}
		prompt += "\n\n" + contract.Instruction()
	}
	invocation := ""
	if ec.Attempt != nil {
		invocation = ec.Attempt.ID
	}
	owner := nativeString(ec.Metadata, "origin_run_id")
	if owner == "" {
		owner = ec.RunID
	}
	req := AgentRequest{RunID: owner, InvocationID: invocation, AgentID: step.Uses[len("agent/"):], Model: nativeString(args, "model"), Prompt: prompt, Workspace: workspace, Output: contract}
	queued := time.Now()
	pool := e.Capacity
	if pool == nil {
		pool = &Capacity{Slots: slots}
	}
	lease, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	if e.BindTools != nil {
		generation := int64(1)
		ownerToken := ""
		if f, ok := ctx.Value(fenceKey{}).(executionFence); ok {
			generation = f.generation
			ownerToken = f.owner
		}
		req.Generation = generation
		binding, err := e.BindTools(ctx, ToolInvocation{RunID: owner, Owner: ownerToken, Generation: generation, InvocationID: invocation, Role: req.AgentID, Workspace: workspace, Secrets: e.redactions(ec.Secrets)})
		if err != nil {
			return nil, err
		}
		req.Tools = &InvocationTools{Binding: binding, Lease: lease}
	}
	queueTime := time.Since(queued).Milliseconds()
	a := ec.Attempt
	if a == nil {
		a = &Attempt{}
	}
	persist := func() error {
		if journal, ok := e.Store.(Journal); ok && ec.Attempt != nil {
			return journal.RecordAttempt(context.WithoutCancel(ctx), *a)
		}
		return nil
	}
	call := func(request AgentRequest) (map[string]any, error) {
		store := invocationStore(e.Store)
		identity := UsageIdentity{ID: invocation, RunID: owner, TaskAttemptID: a.ID, Kind: "task", Model: request.Model}
		if source, ok := e.Agents.(interface {
			UsageProvider(string, string) (string, string)
		}); ok {
			identity.Provider, identity.Model = source.UsageProvider(request.AgentID, request.Model)
		}
		if request.RepairOnly {
			identity.ID = repairUsageID(a.ID, a.RepairCount)
			identity.Kind = "repair"
			identity.TaskAttemptID = a.TaskAttemptID
		}
		if store != nil && invocation != "" {
			if err := store.BeginUsage(ctx, identity); err != nil {
				return nil, err
			}
			request.ObserveUsage = func(update UsageUpdate) error {
				return store.ObserveUsage(context.WithoutCancel(ctx), identity, update)
			}
		}
		if ec.Ordered {
			if err := e.iterationEvidence(ctx, RunRequest{runID: ec.RunID, Metadata: ec.Metadata}); err != nil {
				return nil, err
			}
		}
		outputs, err := e.Agents.RunAgent(ctx, request)
		if outputs != nil {
			outputs = cloneJSONValue(outputs).(map[string]any)
			outputs["queue_time_ms"] = queueTime
		}
		if store != nil && invocation != "" {
			update := usageUpdate(outputs, true)
			if observeErr := store.ObserveUsage(context.WithoutCancel(ctx), identity, update); observeErr != nil {
				err = errors.Join(err, iterationPersistence(observeErr, ec.Ordered))
			}
		}
		return outputs, err
	}
	var outputs map[string]any
	if contract != nil && schemaPhase(a.Phase) {
		outputs = cloneJSONValue(a.TaskOutputs).(map[string]any)
		if len(a.RepairOutputs) > 0 {
			outputs = cloneJSONValue(a.RepairOutputs[len(a.RepairOutputs)-1]).(map[string]any)
		}
		if outputs == nil {
			outputs = map[string]any{}
		}
	} else {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outputs, err = call(req)
		if err != nil {
			return outputs, err
		}
		if outputs == nil {
			return nil, fmt.Errorf("agent returned no outputs")
		}
		if contract == nil {
			return outputs, nil
		}
		a.TaskOutputs = cloneJSONValue(outputs).(map[string]any)
		a.TaskAttemptID = a.ID
		a.Phase = "task_completed"
		if err = persist(); err != nil {
			return outputs, err
		}
	}
	repairs := contract.RepairAttempts
	if repairs > 3 {
		repairs = 3
	}
	repairLimit := a.RepairCount + repairs
	for {
		if err := ctx.Err(); err != nil {
			return outputs, err
		}
		responseText := nativeString(outputs, "text")
		if structured, ok := outputs["structured"]; ok {
			encoded, err := json.Marshal(structured)
			if err != nil {
				return failedSchemaOutputs(outputs), err
			}
			responseText = string(encoded)
		}
		validated := ValidateAgentStructuredOutput(responseText, contract)
		if validated.Valid {
			outputs["structured"] = validated.Structured
			outputs["schema_repair_count"] = a.RepairCount
			return outputs, nil
		}
		if a.RepairCount >= repairLimit {
			return failedSchemaOutputs(outputs), fmt.Errorf("invalid structured output: %s (schema repair exhausted)", validated.Error)
		}
		req.RepairOnly = true
		req.Prompt = "Previous response failed validation: " + validated.Error + "\nFormat the following data as a corrected complete JSON value. Do not execute tools or repeat the original task.\n" + responseText + "\n" + contract.Instruction()
		previousPhase := a.Phase
		a.RepairCount++
		a.Phase = "repair_admitted"
		if err := persist(); err != nil {
			a.RepairCount--
			a.Phase = previousPhase
			return failedSchemaOutputs(outputs), err
		}
		repaired, err := call(req)
		if err != nil {
			return failedSchemaOutputs(outputs), fmt.Errorf("structured output repair failed: %w", err)
		}
		if repaired == nil {
			return failedSchemaOutputs(outputs), fmt.Errorf("structured output repair returned no result")
		}
		a.RepairOutputs = append(a.RepairOutputs, cloneJSONValue(repaired).(map[string]any))
		a.Phase = "repair_completed"
		if err = persist(); err != nil {
			return failedSchemaOutputs(outputs), err
		}
		outputs = repaired
	}

}

func (e *Executor) save(ctx context.Context, run *Run) error {
	if e.Store != nil {
		if _, ok := e.Store.(Journal); !ok {
			return e.Store.Save(ctx, redactRun(run, e.redactionValues))
		}
		return e.Store.Save(ctx, run)
	}
	return nil
}

func randomID() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(token[:])
}

// Resume uses the admitted closure, never current workflow files. Uncertain
// attempts require an explicit acknowledgement before another effect is launched.
func (e *Executor) Resume(ctx context.Context, id string, secrets map[string]string, retry bool) (result *Run, err error) {
	if e.Slots != nil && cap(e.Slots) == 0 {
		return nil, fmt.Errorf("execution capacity must be positive")
	}
	copy := *e
	e = &copy
	journal, ok := e.Store.(Journal)
	if !ok {
		return nil, fmt.Errorf("legacy JSON records cannot resume")
	}
	e.redactionValues = e.redactions(secrets)
	defer func() { result, err = safeResult(result, err, e.redactionValues) }()
	e.Store = redactedStore{Journal: journal, secrets: e.redactionValues}
	run, snap, err := journal.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	// Check the retained closure before admission or an unanswered-wait return.
	// A legacy Journal can load input runs without exposing the input extension.
	if hasInputWait(snap.Definitions) && externalInputJournal(e.Store) == nil {
		return nil, fmt.Errorf("input/wait requires a durable ExternalInputJournal (%s)", DurableExternalInputV1)
	}
	e.CommandExecution = snap.Execution
	if origin := nativeString(snap.Metadata, "origin_run_id"); origin != "" && origin != run.ID {
		return nil, fmt.Errorf("resume originating run %s", origin)
	}
	if run.Status == RunStatusRunning {
		return nil, fmt.Errorf("run is still owned; recover it before resume")
	}
	if run.Status != RunStatusQueued {
		if admissions, ok := journal.(interface {
			QueueResume(context.Context, string, string, bool, int) (*Run, error)
		}); ok {
			run, err = admissions.QueueResume(ctx, id, "", retry, 0)
			if err != nil {
				return nil, err
			}
		}
	}
	if run.Status == RunStatusWaiting || run.Status == RunStatusInterrupted {
		return run, nil
	}
	if e.ActionFactory != nil {
		actions, err := e.ActionFactory(snap.Roles)
		if err != nil {
			return nil, err
		}
		e.Actions = actions
	}
	if e.RoleRunner != nil {
		runner, err := e.RoleRunner(snap.Roles)
		if err != nil {
			return nil, err
		}
		e.Agents = runner
		e.redactionValues = e.redactions(secrets)
		e.Store = redactedStore{Journal: journal, secrets: e.redactionValues}
	}
	for _, key := range snap.RequiredSecrets {
		if _, ok := secrets[key]; !ok {
			return nil, fmt.Errorf("secret %q must be available again", key)
		}
	}
	for ref, w := range snap.Definitions {
		b, err := json.Marshal(w)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(b)
		if hex.EncodeToString(hash[:]) != snap.Hashes[ref] {
			return nil, fmt.Errorf("snapshot hash mismatch for %s", ref)
		}
	}
	if err = e.preflight(ctx, snap.Definitions); err != nil {
		return nil, err
	}
	e.Roles = snap.Roles
	if err = e.validateIterationRecovery(ctx, run, snap, secrets); err != nil {
		return nil, err
	}
	if run.Status == RunStatusSucceeded && !iterationRetry(run) {
		if err = e.validateActionRecovery(ctx, run.ID, snap); err != nil {
			return nil, err
		}
		return run, nil
	}
	previousStatus := run.Status
	run.Status = RunStatusRunning
	if err = journal.Claim(ctx, run, previousStatus); err != nil {
		return nil, err
	}
	if err = errors.Join(e.validateActionRecovery(fenceContext(ctx, run), run.ID, snap), e.validateInputRecovery(fenceContext(ctx, run), run.ID)); err != nil {
		run.Status = RunStatusNeedsAttention
		run.Error = err.Error()
		run.FinishedAt = time.Now().UTC()
		return run, errors.Join(err, e.save(fenceContext(ctx, run), run))
	}
	resetIterationDependents(run, snap.Definitions)
	req := RunRequest{actionContracts: snap.ActionContracts, Execution: snap.Execution, Ref: run.WorkflowRef, Inputs: snap.Inputs, Event: snap.Event, Secrets: secrets, SecretRefs: snap.SecretRefs, RetryUncertain: retry, Metadata: snap.Metadata}
	slots := e.Slots
	if slots == nil {
		limit := snap.Parallel
		if limit <= 0 {
			limit = 4
		}
		slots = make(chan struct{}, limit)
	}
	result, runErr := e.execute(ctx, run.WorkflowRef, req, snap.Definitions, slots, 0, run)
	return result, runErr
}

func (e *Executor) redactions(secrets map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range secrets {
		out[k] = v
	}
	if source, ok := e.Agents.(interface{ SecretValues() map[string]string }); ok {
		for k, v := range source.SecretValues() {
			out[k] = v
		}
	}
	return out
}

func failedSchemaOutputs(outputs map[string]any) map[string]any {
	out, _ := cloneJSONValue(outputs).(map[string]any)
	if structured, ok := out["structured"]; ok {
		out["native_structured"] = structured
		delete(out, "structured")
	}
	return out
}
