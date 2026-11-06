package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflowdef"
)

func mustDefinitionJSON(d *workflowdef.Definition) []byte { b, _ := json.Marshal(d); return b }

// Jobs is a manifest for existing role/action admission. The definition runtime
// alone schedules these operations; the manifest is never executed as a DAG.
func definitionWorkflow(d *workflowdef.Definition) *Workflow {
	steps := []Step{}
	for i, role := range d.Roles() {
		steps = append(steps, Step{ID: fmt.Sprintf("role_%d", i), Uses: "agent/" + role, With: map[string]any{"prompt": "definition manifest"}})
	}
	for i, op := range d.Operations() {
		if action, ok := op["action"].(string); ok {
			steps = append(steps, Step{ID: fmt.Sprintf("action_%d", i), Uses: "action/" + action})
		}
		if args, ok := op["run"].([]any); ok {
			step := Step{ID: fmt.Sprintf("command_%d", i)}
			for _, arg := range args {
				step.Argv = append(step.Argv, arg.(string))
			}
			steps = append(steps, step)
		}
	}
	return &Workflow{Definition: d, Name: "JSON workflow", On: WorkflowTriggers{Manual: map[string]any{}}, Jobs: map[string]Job{"definition": {Steps: steps}}}
}

func (e *Executor) preflightDefinition(d *workflowdef.Definition) error {
	if _, ok := e.Store.(Journal); !ok {
		return fmt.Errorf("JSON workflows require a durable Journal")
	}
	if len(d.Roles()) > 0 && e.Agents == nil {
		return fmt.Errorf("JSON workflow requires an agent runner")
	}
	for _, op := range d.Operations() {
		if credentials, ok := op["credentials"].(map[string]any); ok && len(credentials) > 0 {
			return fmt.Errorf("this executor has no credential-slot adapter; configure credentials on the admitted agent role")
		}
	}
	roles := d.SessionRoles()
	if len(roles) == 0 {
		return nil
	}
	sessions, ok := e.Agents.(SessionRunner)
	if !ok {
		return fmt.Errorf("conversations and recovery require a SessionRunner")
	}
	for _, role := range roles {
		caps, err := sessions.Capabilities(role)
		if err != nil {
			return err
		}
		if !caps.Sessions || !caps.Resume || !caps.ManagedPolicy {
			return fmt.Errorf("agent %s requires resumable sessions and managed tool policy", role)
		}
	}
	return nil
}

// Source data is captured before admission and never reread during resume.
func (e *Executor) pinDefinitionSources(all map[string]*Workflow, req *RunRequest) error {
	ref, _ := CanonicalLocalRef(req.Ref)
	w := all[ref]
	if w == nil || w.Definition == nil {
		return nil
	}
	if req.Inputs == nil {
		req.Inputs = map[string]any{}
	}
	if use, ok := w.Definition.Data["input"]; ok {
		if err := w.Definition.ValidateValue(use, req.Inputs); err != nil {
			return fmt.Errorf("workflow input: %w", err)
		}
	}
	resolved, err := (Resolver{WorkspaceDir: e.WorkspaceDir, DefinitionsDir: e.DefinitionsDir}).ResolveLocal(ref)
	if err != nil {
		return err
	}
	sources, err := w.Definition.Sources(filepath.Dir(resolved.Path), os.LookupEnv)
	if err != nil {
		return err
	}
	metadata, _ := cloneJSONValue(req.Metadata).(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["definition_sources"] = sources
	req.Metadata = metadata
	return nil
}

type definitionHost struct {
	e     *Executor
	run   *Run
	req   RunRequest
	slots chan struct{}
	mu    sync.Mutex
}

func (h *definitionHost) Save(ctx context.Context, state *workflowdef.State) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.run.DefinitionState = state
	return h.e.save(ctx, h.run)
}

func (e *Executor) executeDefinition(ctx context.Context, d *workflowdef.Definition, run *Run, req RunRequest, inputs map[string]any, slots chan struct{}, depth int) (*Run, error) {
	host := &definitionHost{e: e, run: run, req: req, slots: slots}
	rt := &workflowdef.Runtime{Definition: d, Host: host, State: run.DefinitionState, RetryFailed: req.RetryUncertain}
	sources, _ := req.Metadata["definition_sources"].(map[string]any)
	if rt.State == nil && sources == nil {
		return e.finalizeRun(ctx, run, &workflowdef.Failure{Code: "state.integrity", Message: "admitted source snapshot is missing", Hard: true}, req, depth)
	}
	value, err := rt.Run(ctx, run.ID, inputs, sources)
	run.Status = RunStatusSucceeded
	if err != nil {
		run.Status = executionFailureStatus(ctx, err)
		run.Error = err.Error()
		if errors.Is(err, context.DeadlineExceeded) || cancellationOnly(err) {
			run.Status = RunStatusCanceled
		}
		if workflowdef.NeedsAttention(err) {
			run.Status = RunStatusNeedsAttention
		}
	} else {
		if output, ok := value.(map[string]any); ok {
			run.Outputs = output
		} else {
			run.Outputs = map[string]any{"result": value}
		}
	}
	job := run.Jobs["definition"]
	job.ID, job.Status, job.Error, job.Outputs = "definition", run.Status, run.Error, run.Outputs
	run.Jobs["definition"] = job
	return e.finalizeRun(ctx, run, err, req, depth)
}

func definitionStepID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:16])
}

func (h *definitionHost) Invoke(ctx context.Context, call workflowdef.Call) (workflowdef.Reply, error) {
	e := *h.e
	e.Agents = &definitionAgent{base: e.Agents, call: call}
	binder := e.BindTools
	e.BindTools = func(ctx context.Context, inv ToolInvocation) (ToolBinding, error) {
		var base ToolBinding
		var err error
		if binder != nil {
			base, err = binder(ctx, inv)
			if err != nil {
				return nil, err
			}
		}
		return &definitionTools{base: base, call: call, admit: func() error { return e.checkEffectAdmission(ctx, h.run.ID) }}, nil
	}
	id := definitionStepID(call.ID)
	ec := expressionContext{Literal: true, RunID: h.run.ID, JobID: "definition", Metadata: h.req.Metadata, Secrets: h.req.Secrets}
	step := Step{ID: id}
	switch call.Kind {
	case "command":
		step.Argv = call.Argv
		step.stdin, _ = json.Marshal(redactValue(call.With, h.e.redactionValues))
	case "action":
		step.Uses = "action/" + call.Action
		if call.With != nil {
			var ok bool
			step.With, ok = call.With.(map[string]any)
			if !ok {
				return workflowdef.Reply{}, fmt.Errorf("action with must be an object")
			}
		}
	default:
		step.Uses = "agent/" + call.Role
		prompt := call.Prompt
		if call.With != nil {
			data, _ := json.Marshal(call.With)
			prompt += "\nTask input (data): " + string(data)
		}
		if call.Output != nil {
			schema, _ := json.Marshal(call.Output)
			prompt += "\nReturn only JSON matching this JSON Schema: " + string(schema)
		}
		step.With = map[string]any{"prompt": prompt}
	}
	var result StepExecution
	var err error
	if call.Kind == "action" {
		result, err = e.executeApplicationAction(ctx, step, ec)
	} else {
		journal := e.Store.(Journal)
		var attempt Attempt
		var reused bool
		attempt, reused, err = journal.Begin(ctx, h.run.ID, "definition", id, 0, h.req.RetryUncertain)
		if err == nil && reused {
			result = attempt.Result
		} else if err == nil {
			ec.Attempt = &attempt
			result = StepExecution{ID: id, AttemptID: attempt.ID, Status: RunStatusSucceeded, StartedAt: time.Now().UTC()}
			result.Outputs, err = e.executeStep(invocationContext(ctx, e.Store, &attempt), step, ec, h.slots)
			result.FinishedAt = time.Now().UTC()
			if err != nil {
				result.Status = executionFailureStatus(ctx, err)
				result.Error = err.Error()
				result.FailureClass = classifyFailure(err)
			}
			if saveErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); saveErr != nil {
				err = errors.Join(err, &workflowdef.Failure{Code: "state.storage", Message: saveErr.Error(), Hard: true, EffectState: "unknown", Cause: saveErr})
			}
		}
	}
	// Preserve the native attempt record in run inspection and UI responses.
	h.mu.Lock()
	job := h.run.Jobs["definition"]
	job.ID = "definition"
	if job.Steps == nil {
		job.Steps = map[string]StepExecution{}
	}
	job.Steps[id] = result
	job.Status = result.Status
	h.run.Jobs["definition"] = job
	h.mu.Unlock()
	outputs, _ := redactValue(result.Outputs, h.e.redactionValues).(map[string]any)
	session := nativeString(outputs, "_definition_session_id")
	delete(outputs, "_definition_session_id")
	reply := workflowdef.Reply{Value: outputs, SessionID: session}
	if err == nil && result.Status != "" && result.Status != RunStatusSucceeded {
		err = fmt.Errorf("retained invocation %s: %s", result.Status, result.Error)
	}
	if err != nil {
		var f *workflowdef.Failure
		if errors.As(err, &f) {
			safe := *f
			safe.Message = Redact(err.Error(), h.e.redactionValues)
			safe.Hard = safe.Hard || hardExecutionFailure(err) || result.Status == RunStatusNeedsAttention
			safe.Cause = err
			return reply, &safe
		}
		code := "execution.failed"
		isHard := hardExecutionFailure(err) || result.Status == RunStatusNeedsAttention
		if classifyFailure(err) == "needs_attention" {
			code = "execution.uncertain"
			isHard = true
		}
		if ctx.Err() != nil {
			code = "execution.canceled"
			isHard = true
			err = errors.Join(err, ctx.Err())
		}
		effect := "completed"
		if isHard {
			effect = "unknown"
		}
		if code == "execution.canceled" {
			effect = "notStarted"
		}
		return reply, &workflowdef.Failure{Code: code, Message: Redact(err.Error(), h.e.redactionValues), Hard: isHard, EffectState: effect, Cause: err}
	}
	if call.Kind == "command" {
		reply.Value = map[string]any{"exitCode": outputs["exit_code"], "stdout": outputs["stdout"], "stderr": outputs["stderr"]}
	}
	return reply, nil
}

// The engine adds conversation tools to the admitted role's existing tools.
// Dispatch remains bound to this exact participant and invocation.
type definitionTools struct {
	admit func() error
	base  ToolBinding
	call  workflowdef.Call
}

const definitionToolServer = "workflow-conversation"

func (b *definitionTools) Catalog() []ToolDefinition {
	out := []ToolDefinition{}
	if b.base != nil {
		out = append(out, b.base.Catalog()...)
	}
	for _, t := range b.call.Tools {
		out = append(out, ToolDefinition{Server: definitionToolServer, Name: t.Name, Description: t.Description, InputSchema: t.Schema, Fingerprint: actionHash(t.Schema)})
	}
	return out
}
func (b *definitionTools) Call(ctx context.Context, req ToolRequest) (json.RawMessage, error) {
	if req.Server == definitionToolServer && b.call.ToolCall != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b.admit != nil {
			if err := b.admit(); err != nil {
				return nil, err
			}
		}
		return b.call.ToolCall(ctx, req.CallID, req.Tool, req.Arguments)
	}
	if b.base == nil {
		return nil, fmt.Errorf("tool is unavailable")
	}
	return b.base.Call(ctx, req)
}

type definitionAgent struct {
	base AgentRunner
	call workflowdef.Call
}

func (a *definitionAgent) SecretValues() map[string]string {
	if source, ok := a.base.(interface{ SecretValues() map[string]string }); ok {
		return source.SecretValues()
	}
	return nil
}

func (a *definitionAgent) UsageProvider(role, model string) (string, string) {
	if source, ok := a.base.(interface {
		UsageProvider(string, string) (string, string)
	}); ok {
		return source.UsageProvider(role, model)
	}
	return "", model
}
func (a *definitionAgent) RunAgent(ctx context.Context, req AgentRequest) (map[string]any, error) {
	if a.call.Output != nil {
		schema, ok := a.call.Output.(map[string]any)
		if !ok {
			schema = map[string]any{}
			if a.call.Output == false {
				schema["not"] = map[string]any{}
			}
		}
		// The definition runtime validates the full contract. This request only
		// configures the provider's native structured-output channel; it does not
		// enter the legacy schema-repair loop or repeat task effects.
		req.Output = &AgentOutputContract{Format: "json", Schema: schema, RepairAttempts: 0}
	}
	if !a.call.Persistent {
		return a.base.RunAgent(ctx, req)
	}
	sessions, ok := a.base.(SessionRunner)
	if !ok {
		return nil, fmt.Errorf("agent does not support resumable sessions")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var callbackErr error
	retain := func(id string) {
		if id == "" || a.call.SessionCreated == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if callbackErr == nil {
			callbackErr = a.call.SessionCreated(id)
			if callbackErr != nil {
				cancel()
			}
		}
	}
	result, err := sessions.RunTurn(ctx, SessionRequest{AgentRequest: req, SessionID: a.call.SessionID}, func(event ProviderEvent) { retain(event.SessionID) })
	retain(result.SessionID)
	mu.Lock()
	err = errors.Join(err, callbackErr)
	mu.Unlock()
	outputs, _ := cloneJSONValue(result.Outputs).(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	outputs["_definition_session_id"] = result.SessionID
	if err == nil && result.Status != RunStatusSucceeded {
		err = fmt.Errorf("participant turn ended with status %s", result.Status)
	}
	return outputs, err
}
