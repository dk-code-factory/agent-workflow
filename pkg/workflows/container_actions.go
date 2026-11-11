package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/google/jsonschema-go/jsonschema"
)

type definitionKey struct{}
type credentialRefsKey struct{}

func credentialRefsFrom(ctx context.Context) string {
	refs, _ := ctx.Value(credentialRefsKey{}).(map[string]string)
	if len(refs) == 0 {
		return ""
	}
	b, _ := json.Marshal(refs)
	return string(b)
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func secretEnvironmentReference(value string) (string, bool) {
	if !wholeExpression(value) {
		return "", false
	}
	text := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "${{"), "}}"))
	if !strings.HasPrefix(text, "secrets.") {
		return "", false
	}
	name := strings.TrimPrefix(text, "secrets.")
	return name, idPattern.MatchString(name)
}
func renderCommandEnv(env map[string]string, ec expressionContext) ([]string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if !environmentName.MatchString(key) || strings.HasPrefix(key, "AGENT_WORKFLOW_") {
			return nil, fmt.Errorf("invalid/reserved environment key %q", key)
		}
		v, err := renderString(env[key], ec)
		if err != nil {
			return nil, err
		}
		text, ok := v.(string)
		if !ok || strings.ContainsRune(text, 0) {
			return nil, fmt.Errorf("env.%s must render to a string without NUL", key)
		}
		out = append(out, key+"="+text)
	}
	return out, nil
}
func resolveContainerSchema(w *Workflow, raw any) (json.RawMessage, *jsonschema.Resolved, error) {
	if name, ok := raw.(string); ok {
		var found bool
		raw, found = w.Schemas[name]
		if !found {
			return nil, nil, fmt.Errorf("unknown workflow-local schema %q", name)
		}
	}
	mapping, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("schema requires an inline mapping or workflow-local name")
	}
	schema, err := normalizeSchemaMap(mapping)
	if err != nil {
		return nil, nil, err
	}
	if schema["type"] != "object" {
		return nil, nil, fmt.Errorf("structured helper schemas require type: object")
	}
	if err = validateNamedSchema(schema, "schema", 0); err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil, nil, err
	}
	var s jsonschema.Schema
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, nil, err
	}
	r, err := s.Resolve(nil)
	return b, r, err
}
func validateContainerProtocol(w *Workflow, job string, s Step) error {
	for key, value := range s.Env {
		if !environmentName.MatchString(key) || strings.HasPrefix(key, "AGENT_WORKFLOW_") || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid/reserved environment assignment %q", key)
		}
	}
	if len(s.Env) > 0 && len(s.Argv) == 0 && s.Run == "" && !isDockerStep(s) {
		return fmt.Errorf("env requires a command or Docker step")
	}
	if s.Protocol == "" {
		if s.Effect != "" || s.InputSchema != nil || s.OutputSchema != nil || s.Input != nil {
			return fmt.Errorf("structured fields require protocol: action-v1")
		}
		return nil
	}
	if s.Protocol != ContainerActionV1 {
		return fmt.Errorf("unsupported container protocol %q", s.Protocol)
	}
	if w.Jobs[job].Container == nil && !isDockerStep(s) {
		return fmt.Errorf("action-v1 requires a job container or Docker step")
	}
	if isStepBlock(s) || hasLifecycle(s) || s.HandlerTimeout != "" {
		return fmt.Errorf("structured helpers cannot own blocks or ordinary lifecycle handlers")
	}
	if s.Effect != ActionReadOnly && s.Effect != ActionExternalMutation {
		return fmt.Errorf("action-v1 requires explicit read_only or external_mutation effect")
	}
	for _, raw := range []any{s.InputSchema, s.OutputSchema} {
		if _, _, err := resolveContainerSchema(w, raw); err != nil {
			return err
		}
	}
	for _, text := range append(append([]string(nil), s.Argv...), s.Run, s.Shell, s.WorkingDirectory) {
		if strings.Contains(text, "${{") {
			return fmt.Errorf("structured executable commands and settings must be static; use input for business data")
		}
	}
	for key, value := range s.Env {
		if !environmentName.MatchString(key) || strings.HasPrefix(key, "AGENT_WORKFLOW_") {
			return fmt.Errorf("invalid/reserved environment key %q", key)
		}
		if strings.Contains(value, "${{") {
			if _, ok := secretEnvironmentReference(value); !ok {
				return fmt.Errorf("structured env expressions must reference one secret directly")
			}
		}
	}
	return nil
}
func buildActionContract(name, implementation string, effect ActionEffect, input, output json.RawMessage) ActionContract {
	c := ActionContract{Name: name, Implementation: implementation, Effect: effect, InputSchema: input, OutputSchema: output}
	c.Fingerprint = actionHash(c)
	return c
}
func containerActionName(ref, job, step string) string {
	return "container_" + actionHash([]string{ref, job, step})
}
func (e *Executor) configureContainerActions(all map[string]*Workflow) error {
	if e.applicationActions == nil {
		e.applicationActions = map[string]*registeredAction{}
	}
	for ref, w := range all {
		for job, j := range w.Jobs {
			for _, step := range containerContractSteps(j.Steps) {
				if step.Protocol != ContainerActionV1 {
					continue
				}
				if actionJournal(e.Store) == nil {
					return fmt.Errorf("structured helpers require durable ActionJournal")
				}
				p := step.ContainerImplementation
				if !isDockerStep(step) && j.Container != nil {
					p = j.Container.Implementation
				}
				if p == nil {
					return fmt.Errorf("structured helper container was not admitted")
				}
				in, ir, err := resolveContainerSchema(w, step.InputSchema)
				if err != nil {
					return err
				}
				out, or, err := resolveContainerSchema(w, step.OutputSchema)
				if err != nil {
					return err
				}
				name := containerActionName(ref, job, step.ID)
				// Hash admitted container policy and authored step settings, without current credential values.
				implementation := actionHash(struct {
					Container *ContainerImplementation
					Step      Step
				}{p, step})
				if p.ContractVersion >= 1 {
					argv, err := structuredCommand(step, p.Image)
					if err != nil {
						return err
					}
					bundle := ""
					if p.Bundle != nil {
						bundle = p.Bundle.ID
					}
					implementation = actionHash(struct {
						Selection                             any
						ImageID, OS, Architecture             string
						Command                               []string
						Protocol, WorkingDir, Timeout         string
						Policy                                any
						GuardianSHA, GuardianPlatform, Bundle string
						Env                                   map[string]string
					}{p.Selection, p.Image.ID, p.Image.OS, p.Image.Architecture, argv, step.Protocol, step.WorkingDirectory, step.Timeout, p.Policy, p.Guardian.SHA256, p.Guardian.Platform, bundle, step.Env})
				}
				a := &registeredAction{contract: buildActionContract(name, implementation, step.Effect, in, out), input: ir, output: or}
				a.recovery = validateContainerRecoveryRequest
				helper := step
				selected := p
				call := func(ctx context.Context, ac ActionContext, phase string, input, bindings json.RawMessage, result ActionValue) (containerResponse, error) {
					return e.containerHook(ctx, helper, selected, ac, phase, input, bindings, result)
				}
				a.bindings = func(ctx context.Context, ac ActionContext, input json.RawMessage) (json.RawMessage, error) {
					v, err := call(ctx, ac, "bindings", input, nil, ActionValue{})
					if err != nil {
						return nil, err
					}
					if err = validateContainerCapabilities(v, helper.Effect); err != nil {
						return nil, err
					}
					return v.Bindings, nil
				}
				a.handler = func(ctx context.Context, ac ActionContext, input json.RawMessage) (ActionValue, error) {
					v, err := call(ctx, ac, "execute", input, nil, ActionValue{})
					return v.Result, err
				}
				a.validate = func(ctx context.Context, ac ActionContext, input, bindings json.RawMessage, result ActionValue) error {
					v, err := call(ctx, ac, "validate_checkpoint", input, bindings, result)
					if err != nil {
						return err
					}
					if v.Valid == nil || !*v.Valid {
						return fmt.Errorf("helper checkpoint validation failed")
					}
					return nil
				}
				a.reconcile = func(ctx context.Context, ac ActionContext, input, bindings json.RawMessage, result ActionValue) (actionReconciliation, error) {
					v, err := call(ctx, ac, "reconcile", input, bindings, result)
					return actionReconciliation{Outcome: v.Outcome, Proof: v.Proof, Result: v.Result}, err
				}
				e.applicationActions[name] = a
				e.actionContracts[name] = a.contract
			}
		}
	}
	return nil
}

type containerResponse struct {
	Protocol       string          `json:"protocol"`
	Phase          string          `json:"phase"`
	Capabilities   []string        `json:"capabilities,omitempty"`
	ReadOnlyPhases []string        `json:"read_only_phases,omitempty"`
	Bindings       json.RawMessage `json:"bindings,omitempty"`
	Result         ActionValue     `json:"result,omitempty"`
	Valid          *bool           `json:"valid,omitempty"`
	Outcome        string          `json:"outcome,omitempty"`
	Proof          json.RawMessage `json:"proof,omitempty"`
}

func validateContainerCapabilities(v containerResponse, effect ActionEffect) error {
	has := func(list []string, key string) bool {
		for _, s := range list {
			if s == key {
				return true
			}
		}
		return false
	}
	for _, phase := range []string{"bindings", "execute", "validate_checkpoint"} {
		if !has(v.Capabilities, phase) {
			return fmt.Errorf("helper does not advertise required %s capability", phase)
		}
	}
	for _, phase := range []string{"bindings", "validate_checkpoint"} {
		if !has(v.ReadOnlyPhases, phase) {
			return fmt.Errorf("helper must declare %s read-only", phase)
		}
	}
	if effect == ActionExternalMutation && (!has(v.Capabilities, "reconcile") || !has(v.ReadOnlyPhases, "reconcile")) {
		return fmt.Errorf("mutation helper requires read-only reconciliation capability")
	}
	return nil
}

type boundedProtocolOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedProtocolOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, fmt.Errorf("helper output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}
func decodeContainerResponse(data []byte, phase string) (containerResponse, error) {
	var v containerResponse
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return v, fmt.Errorf("helper JSON response: %w", err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return v, fmt.Errorf("helper response requires exactly one JSON document")
	}
	if v.Protocol != ContainerActionV1 || v.Phase != phase {
		return v, fmt.Errorf("incompatible helper protocol/phase %q/%q", v.Protocol, v.Phase)
	}
	return v, nil
}
func encodeContainerRequest(f executionFence, ac ActionContext, phase string, input, bindings json.RawMessage, result ActionValue) ([]byte, error) {
	request := struct {
		Protocol     string          `json:"protocol"`
		Phase        string          `json:"phase"`
		Context      ActionContext   `json:"context"`
		RuntimeOwner map[string]any  `json:"runtime_owner"`
		Input        json.RawMessage `json:"input"`
		Checkpoint   map[string]any  `json:"checkpoint,omitempty"`
	}{ContainerActionV1, phase, ac, map[string]any{"run_id": f.id, "generation": f.generation}, input, nil}
	if phase == "validate_checkpoint" || phase == "reconcile" {
		request.Checkpoint = map[string]any{"bindings": bindings, "result": result}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("helper request exceeds 1 MiB")
	}
	return raw, nil
}

func validateContainerRecoveryRequest(ctx context.Context, ac ActionContext, input, bindings json.RawMessage, result ActionValue) error {
	f, owned := ctx.Value(fenceKey{}).(executionFence)
	if !owned {
		return fmt.Errorf("structured helper hooks require current execution ownership")
	}
	// Future validation uses a fresh 32-character run ID; reserve the full
	// generation width so a later owner cannot outgrow this admitted budget.
	f.id = strings.Repeat("0", max(32, len(f.id)))
	f.generation = math.MaxInt64
	if _, err := encodeContainerRequest(f, ac, "validate_checkpoint", input, bindings, result); err != nil {
		return fmt.Errorf("helper recovery request exceeds the combined input/bindings/result budget: %w", err)
	}
	return nil
}

func (e *Executor) containerHook(ctx context.Context, step Step, p *ContainerImplementation, ac ActionContext, phase string, input, bindings json.RawMessage, result ActionValue) (containerResponse, error) {
	f, owned := ctx.Value(fenceKey{}).(executionFence)
	if !owned {
		return containerResponse{}, fmt.Errorf("structured helper hooks require current execution ownership")
	}
	if err := e.checkEffectAdmission(ctx, f.id); err != nil {
		return containerResponse{}, err
	}
	if e.Ready != nil {
		if err := e.Ready(ctx); err != nil {
			return containerResponse{}, err
		}
	}
	raw, err := encodeContainerRequest(f, ac, phase, input, bindings, result)
	if err != nil {
		return containerResponse{}, err
	}
	argv, err := structuredCommand(step, p.Image)
	if err != nil {
		return containerResponse{}, err
	}
	env := make([]string, 0, len(step.Env))
	keys := make([]string, 0, len(step.Env))
	for key := range step.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := step.Env[key]
		if _, secret := secretEnvironmentReference(value); secret {
			var references map[string]string
			if err := json.Unmarshal([]byte(ac.CredentialRefs), &references); err != nil {
				return containerResponse{}, fmt.Errorf("invalid retained credential references")
			}
			root, ok := references[key]
			if !ok {
				return containerResponse{}, fmt.Errorf("missing retained credential reference for env.%s", key)
			}
			var found bool
			value, found = e.runSecrets[root]
			if !found {
				return containerResponse{}, fmt.Errorf("secret %q must be available again", root)
			}
		}
		env = append(env, key+"="+value)
	}
	timeout := 5 * time.Minute
	if step.Timeout != "" {
		timeout, err = time.ParseDuration(step.Timeout)
		if err != nil {
			return containerResponse{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Each hook has its own physical invocation. Retained original context remains
	// in the request; current fencing and cleanup use the active runtime owner.
	ctx = invocationContext(ctx, e.Store, &Attempt{ID: "helper-" + phase + "-" + runtimeexec.NewID()})
	var stdout = boundedProtocolOutput{limit: 1 << 20}
	var stderr = boundedProtocolOutput{limit: 4 << 20}
	// Recovery/validation hooks use independent containers with the same admitted
	// image and command. They reuse a held job capacity lease when one exists.
	if phase != "execute" || ctx.Value(jobContainerKey{}) == nil {
		step.Uses = "docker://" + p.Image.ID
		step.ContainerImplementation = p
	}
	code, _, err := e.runContainerIO(ctx, step, argv, env, containerapi.Streams{Stdin: bytes.NewReader(raw), Stdout: &stdout, Stderr: &stderr}, e.Slots)
	if err != nil || code != 0 || stdout.overflow || stderr.overflow {
		return containerResponse{}, fmt.Errorf("helper %s failed (exit %d): %w; %s", phase, code, errors.Join(err, fmt.Errorf("invalid helper execution")), Redact(stderr.String(), e.redactionValues))
	}
	return decodeContainerResponse(stdout.Bytes(), phase)
}
func (e *Executor) executeContainerAction(ctx context.Context, step Step, ec expressionContext, slots chan struct{}) (StepExecution, error) {
	refs := map[string]string{}
	for key, value := range step.Env {
		secret, ok := secretEnvironmentReference(value)
		if !ok {
			continue
		}
		if _, found := ec.Secrets[secret]; !found {
			return StepExecution{}, fmt.Errorf("secret %q is unavailable", secret)
		}
		root, found := ec.CredentialAliases[secret]
		if !found {
			return StepExecution{}, fmt.Errorf("env.%s requires a forwarded root credential reference", key)
		}
		refs[key] = root

	}
	ctx = context.WithValue(ctx, credentialRefsKey{}, refs)
	ref, _ := ctx.Value(definitionKey{}).(string)
	lowered := step
	lowered.Uses = "action/" + containerActionName(ref, ec.JobID, step.ID)
	lowered.With = step.Input
	return e.executeApplicationAction(ctx, lowered, ec)
}
func (e *Executor) isStructuredAttempt(ctx context.Context, a Attempt) bool {
	store := invocationStore(e.Store)
	if store == nil {
		return false
	}
	var n int
	return store.db.QueryRowContext(ctx, "SELECT count(*) FROM action_attempts WHERE attempt_id=?", a.ID).Scan(&n) == nil && n > 0
}
func (e *Executor) validateCompletedContainerActions(ctx context.Context, original *Run, snap Snapshot) (returned error) {
	has := false
	for _, w := range snap.Definitions {
		for _, j := range w.Jobs {
			for _, s := range containerContractSteps(j.Steps) {
				has = has || s.Protocol == ContainerActionV1
			}
		}
	}
	if !has {
		return e.validateActionRecovery(ctx, original.ID, snap)
	}
	journal := e.Store.(Journal)
	validation := &Run{ID: randomID(), OriginID: original.ID, Owner: randomID(), Generation: 1, Version: 1, WorkflowRef: original.WorkflowRef, Status: RunStatusRunning, StartedAt: time.Now().UTC(), Jobs: map[string]JobExecution{}}
	validationSnapshot := snap
	validationSnapshot.RequestID = ""
	if err := journal.Admit(ctx, validation, validationSnapshot); err != nil {
		return err
	}
	owned := fenceContext(ctx, validation)
	defer func() {
		finish := context.WithoutCancel(owned)
		store := invocationStore(e.Store)
		if store != nil {
			returned = errors.Join(returned, store.BeginFinalization(finish, validation), store.ReconcileInvocations(finish, validation.ID), store.CheckInvocationCleanup(finish, validation.ID))
		}
		validation.Status = RunStatusSucceeded
		if returned != nil {
			validation.Status = RunStatusNeedsAttention
			validation.Error = returned.Error()
		}
		validation.FinishedAt = time.Now().UTC()
		returned = errors.Join(returned, e.save(finish, validation))
	}()
	return e.validateActionRecovery(owned, original.ID, snap)
}

func forwardCredentialAliases(declaration any, parent map[string]string) map[string]string {
	if text, ok := declaration.(string); ok && text == "inherit" {
		return parent
	}
	out := map[string]string{}
	if m, ok := declaration.(map[string]any); ok {
		for key, v := range m {
			if text, ok := v.(string); ok {
				if name, ok := secretEnvironmentReference(text); ok {
					if root, found := parent[name]; found {
						out[key] = root
					}
				}
			}
		}
	}
	return out
}

func structuredCommand(step Step, image containerapi.Image) ([]string, error) {
	if isDockerStep(step) {
		return dockerCommand(step, image)
	}
	if step.Run != "" {
		shell := step.Shell
		if shell == "" {
			shell = "sh"
		}
		return []string{shell, "-c", step.Run}, nil
	}
	return append([]string(nil), step.Argv...), nil
}

// Handler IDs mirror executeLifecycleStep; block IDs retain physical index paths.
func containerContractSteps(steps []Step) []Step {
	var out []Step
	var add func(Step)
	add = func(step Step) {
		out = append(out, step)
		for _, hook := range []struct {
			kind string
			list []Step
		}{{"onError", step.OnError}, {"onFailure", step.OnFailure}, {"finally", step.Finally}} {
			for i, child := range hook.list {
				child.ID = step.ID + "/" + hook.kind + "/" + stepID(child, i)
				add(child)
			}
		}
	}
	for _, step := range FlattenSteps(steps) {
		add(step)
	}
	return out
}
