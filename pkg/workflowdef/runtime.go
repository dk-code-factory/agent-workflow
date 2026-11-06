package workflowdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"
)

// Failure is both a control signal and the redacted JSON exposed to handlers.
// Hard failures and explicit aborts cannot be consumed by fallback/observer code.
type Failure struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	StepID      string `json:"stepId"`
	Agent       string `json:"agent,omitempty"`
	Attempt     int    `json:"attempt"`
	Retryable   bool   `json:"retryable"`
	EffectState string `json:"effectState"`
	Hard        bool   `json:"hard,omitempty"`
	Abort       bool   `json:"abort,omitempty"`
	Cause       error  `json:"-"`
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }
func (f *Failure) Unwrap() error { return f.Cause }
func fault(code string, err error) *Failure {
	var failure *Failure
	if errors.As(err, &failure) {
		copy := *failure
		copy.Cause = err
		if hard(err) {
			copy.Hard = true
		}
		return &copy
	}
	return &Failure{Code: code, Message: err.Error(), EffectState: "notStarted", Cause: err}
}
func hard(err error) bool {
	if err == nil {
		return false
	}
	if f, ok := err.(*Failure); ok && (f.Hard || f.Abort) {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if hard(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hard(wrapped.Unwrap())
	}
	return false
}

// NeedsAttention distinguishes unsafe execution/storage failures from an
// explicit workflow failure or a safely canceled execution.
func NeedsAttention(err error) bool {
	if err == nil {
		return false
	}
	if f, ok := err.(*Failure); ok && f.Hard && !f.Abort && f.Code != "execution.canceled" {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if NeedsAttention(child) {
				return true
			}
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return NeedsAttention(wrapped.Unwrap())
	}
	return false
}
func errorData(err error) any {
	f := fault("execution.failed", err)
	data, _ := json.Marshal(f)
	value, _ := DecodeJSON(data)
	return value
}

// Tool is controller-owned. Participants cannot select their sender/session or
// call participants outside the admitted conversation.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}
type ToolCall func(context.Context, string, string, json.RawMessage) (json.RawMessage, error)

type Call struct {
	ID               string
	Kind             string
	Role             string
	Prompt           string
	With             any
	Argv             []string
	Action           string
	SessionID        string
	Persistent       bool
	Credentials      map[string]string
	Tools            []Tool
	ToolCall         ToolCall
	SessionCreated   func(string) error
	Output           any
	Validate         func(Reply) (Reply, error)
	Diagnostic       bool
	ConversationPath string
}
type Reply struct {
	Value     any    `json:"value"`
	SessionID string `json:"sessionId,omitempty"`
	Completed bool   `json:"completed,omitempty"`
}

// Host must persist receipts before returning and honor canceled ownership and
// the admitted role policy. Invoke must not automatically repeat unknown effects.
type Host interface {
	Invoke(context.Context, Call) (Reply, error)
	Save(context.Context, *State) error
}

type NodeResult struct {
	Value     any            `json:"value"`
	Exports   map[string]any `json:"exports,omitempty"`
	Recovered bool           `json:"recovered,omitempty"`
}
type CallResult struct {
	Reply   Reply    `json:"reply"`
	Failure *Failure `json:"failure,omitempty"`
	Pending bool     `json:"pending,omitempty"`
	Ordinal int      `json:"ordinal"`
}
type Circuit struct {
	Failures  int       `json:"failures"`
	OpenUntil time.Time `json:"openUntil,omitempty"`
	Probe     string    `json:"probe,omitempty"`
	Epoch     int       `json:"epoch"`
	LastError any       `json:"lastError,omitempty"`
}
type State struct {
	Version         string                   `json:"version"`
	Hash            string                   `json:"hash"`
	RunID           string                   `json:"runId"`
	Started         time.Time                `json:"started"`
	Deadline        time.Time                `json:"deadline"`
	CleanupDeadline time.Time                `json:"cleanupDeadline,omitempty"`
	Attempts        int                      `json:"attempts"`
	Sources         map[string]any           `json:"sources"`
	Input           map[string]any           `json:"input"`
	Nodes           map[string]NodeResult    `json:"nodes"`
	Calls           map[string]CallResult    `json:"calls"`
	Plans           map[string]any           `json:"plans"`
	Circuits        map[string]*Circuit      `json:"circuits"`
	Conversations   map[string]*Conversation `json:"conversations"`
}

type Runtime struct {
	Definition     *Definition
	Host           Host
	State          *State
	RetryFailed    bool
	mu             sync.Mutex
	policies       map[string]map[string]any
	retried        map[string]bool
	maxAttempts    int
	maxParallel    int
	hookTimeout    time.Duration
	cleanupTimeout time.Duration
	slots          chan struct{}
}

func integer(value any, fallback int) int {
	if n, ok := value.(float64); ok {
		return int(n)
	}
	return fallback
}
func milliseconds(value any, fallback time.Duration) time.Duration {
	return time.Duration(integer(value, int(fallback/time.Millisecond))) * time.Millisecond
}

func (r *Runtime) Run(ctx context.Context, runID string, input, sources map[string]any) (any, error) {
	if r.Host == nil || r.Definition == nil {
		return nil, fmt.Errorf("definition and host are required")
	}
	d := r.Definition
	if input == nil {
		input = map[string]any{}
	}
	encodedInput, inputErr := json.Marshal(input)
	if inputErr != nil {
		return nil, inputErr
	}
	if inputErr = json.Unmarshal(encodedInput, &input); inputErr != nil {
		return nil, inputErr
	}
	encodedInput, _ = json.Marshal(input)
	if use, ok := d.Data["input"]; ok {
		if err := d.ValidateValue(use, input); err != nil {
			return nil, fmt.Errorf("workflow input: %w", err)
		}
	}
	data, _ := json.Marshal(d.Data)
	hash := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(hash[:])
	limits := object(d.Data["limits"])
	r.maxAttempts = integer(limits["maxTaskAttempts"], 100)
	r.maxParallel = integer(limits["maxConcurrency"], 4)
	r.hookTimeout = milliseconds(limits["hookTimeoutMs"], 10*time.Second)
	r.cleanupTimeout = milliseconds(limits["cleanupTimeoutMs"], 30*time.Second)
	r.policies = d.breakerPolicies()
	r.retried = map[string]bool{}
	r.slots = make(chan struct{}, r.maxParallel)
	if r.State == nil {
		if sources == nil {
			sources = map[string]any{}
		}
		now := time.Now().UTC()
		r.State = &State{Version: Version, Hash: fingerprint, RunID: runID, Started: now, Deadline: now.Add(milliseconds(limits["timeoutMs"], 30*time.Minute)), Sources: clone(sources).(map[string]any), Input: clone(input).(map[string]any), Nodes: map[string]NodeResult{}, Calls: map[string]CallResult{}, Plans: map[string]any{}, Circuits: map[string]*Circuit{}, Conversations: map[string]*Conversation{}}
	}
	if r.State.Version != Version || r.State.Hash != fingerprint || r.State.RunID != runID {
		return nil, &Failure{Code: "state.integrity", Message: "definition state does not match the admitted workflow", EffectState: "notStarted", Hard: true}
	}
	if r.State.Input == nil {
		return nil, &Failure{Code: "state.integrity", Message: "admitted input snapshot is missing", Hard: true, EffectState: "notStarted"}
	}
	admittedInput, _ := json.Marshal(r.State.Input)
	if string(admittedInput) != string(encodedInput) {
		return nil, &Failure{Code: "state.integrity", Message: "workflow input differs from the admitted invocation", Hard: true, EffectState: "notStarted"}
	}
	if r.State.Sources == nil || r.State.Nodes == nil || r.State.Calls == nil || r.State.Plans == nil || r.State.Circuits == nil || r.State.Conversations == nil || r.State.Started.IsZero() || r.State.Deadline.IsZero() {
		return nil, &Failure{Code: "state.integrity", Message: "incomplete definition checkpoint", EffectState: "notStarted", Hard: true}
	}
	reserved := 0
	for _, call := range r.State.Calls {
		if call.Ordinal < 1 || call.Ordinal > r.maxAttempts {
			return nil, &Failure{Code: "state.integrity", Message: "invalid retained attempt ordinal", Hard: true, EffectState: "unknown"}
		}
		reserved += call.Ordinal
	}
	if reserved != r.State.Attempts || reserved > r.maxAttempts {
		return nil, &Failure{Code: "state.integrity", Message: "retained attempt budget is inconsistent", Hard: true, EffectState: "unknown"}
	}
	for _, conversation := range r.State.Conversations {
		if conversation == nil || conversation.Sessions == nil || conversation.Receipts == nil || conversation.MaxCalls < 1 || conversation.Calls < 0 || conversation.Calls > conversation.MaxCalls || conversation.Turns < 0 || conversation.Turns > conversation.Calls || conversation.Messages < 0 || conversation.Messages > 32 || conversation.Deadline.IsZero() {
			return nil, &Failure{Code: "state.integrity", Message: "incomplete conversation checkpoint", Hard: true, EffectState: "unknown"}
		}
	}
	if err := r.save(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, r.State.Deadline)
	defer cancel()
	env := clone(r.State.Sources).(map[string]any)
	env["input"] = clone(r.State.Input)
	env["steps"] = map[string]any{}
	env["run"] = map[string]any{"id": runID, "invocationId": "root"}
	root := maps.Clone(d.Data)
	if value, ok := d.Data["return"]; ok {
		root["_return"] = value
	}
	result, err := r.node(ctx, root, env, "root", false)
	return result.Value, err
}

func (r *Runtime) save(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveLocked(ctx)
}
func (r *Runtime) saveLocked(ctx context.Context) error {
	data, err := json.Marshal(r.State)
	if err != nil || len(data) > 16<<20 {
		return &Failure{Code: "state.capacity", Message: "definition checkpoint exceeds 16 MiB or contains non-JSON data", EffectState: "notStarted", Hard: true, Cause: err}
	}
	var snapshot State
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	if err := r.Host.Save(context.WithoutCancel(ctx), &snapshot); err != nil {
		return &Failure{Code: "state.storage", Message: err.Error(), EffectState: "notStarted", Hard: true, Cause: err}
	}
	return nil
}

func (r *Runtime) planned(ctx context.Context, key string, prepare func() (any, error)) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if value, ok := r.State.Plans[key]; ok {
		return clone(value), nil
	}
	value, err := prepare()
	if err != nil {
		return nil, err
	}
	r.State.Plans[key] = clone(value)
	if err := r.saveLocked(ctx); err != nil {
		return nil, err
	}
	return clone(value), nil
}

func (r *Runtime) sequence(ctx context.Context, steps []any, env map[string]any, path string, observer bool) (NodeResult, error) {
	result := NodeResult{Exports: map[string]any{}}
	for i, raw := range steps {
		child := object(raw)
		p := fmt.Sprintf("%s/%d", path, i)
		out, err := r.node(ctx, child, env, p, observer)
		if err != nil {
			return out, err
		}
		maps.Copy(object(env["steps"]), out.Exports)
		maps.Copy(result.Exports, out.Exports)
		result.Value = out.Value
	}
	return result, nil
}

func (r *Runtime) node(ctx context.Context, node, env map[string]any, path string, observer bool) (NodeResult, error) {
	r.mu.Lock()
	cached, ok := r.State.Nodes[path]
	r.mu.Unlock()
	if ok {
		return NodeResult{clone(cached.Value), maps.Clone(cached.Exports), cached.Recovered}, nil
	}
	if err := ctx.Err(); err != nil {
		return NodeResult{}, &Failure{Code: "execution.canceled", Message: err.Error(), EffectState: "notStarted", Hard: true, Cause: err}
	}
	env = contextCopy(env)
	env["run"] = maps.Clone(object(env["run"]))
	object(env["run"])["invocationId"] = path
	result := NodeResult{Exports: map[string]any{}}
	var err error
	if !observer {
		err = r.hooks(ctx, node, "onStart", env, path, nil, nil)
	}
	if err == nil {
		result, err = r.body(ctx, node, env, path, observer)
	}
	if err != nil && !hard(err) && len(list(node["onError"])) > 0 {
		original := err
		handler := contextCopy(env)
		handler["error"] = errorData(err)
		failed := map[string]any{"id": text(node["id"]), "path": path}
		for _, key := range []string{"agent", "conversation"} {
			if value, ok := node[key]; ok {
				failed[key] = clone(value)
			}
		}
		if prompt, ok := node["prompt"]; ok {
			if rendered, renderErr := Prompt(text(prompt), env); renderErr == nil {
				failed["prompt"] = rendered
			}
		}
		if value, ok := node["with"]; ok {
			if rendered, renderErr := Resolve(value, env); renderErr == nil {
				failed["with"] = rendered
			}
		}
		failed["_node"] = clone(node)
		if _, ok := node["agent"]; ok {
			failed["_agentPath"] = path
		}
		r.mu.Lock()
		if convo := r.State.Conversations[path]; convo != nil {
			failed["transcript"] = clone(convo.Transcript)
		}
		r.mu.Unlock()
		handler["failed"] = failed
		replacement, handlerErr := r.sequence(ctx, list(node["onError"]), handler, path+"/onError", observer)
		if handlerErr != nil {
			err = errors.Join(original, handlerErr)
		} else {
			if schema, ok := node["output"]; ok {
				handlerErr = r.Definition.ValidateValue(schema, replacement.Value)
			}
			if handlerErr != nil {
				err = fault("validation.output", handlerErr)
			} else {
				result.Value = replacement.Value
				result.Recovered = true
				err = nil
			}
		}
	}
	if result.Exports == nil {
		result.Exports = map[string]any{}
	}
	if id := text(node["id"]); id != "" {
		result.Exports[id] = clone(result.Value)
	}
	if !observer {
		phase := "onSuccess"
		if err != nil {
			phase = "onFailure"
		}
		err = errors.Join(err, r.hooks(ctx, node, phase, env, path, result.Value, err))
		err = errors.Join(err, r.hooks(ctx, node, "finally", env, path, result.Value, err))
	}
	if err != nil {
		return result, err
	}
	r.mu.Lock()
	r.State.Nodes[path] = result
	err = r.saveLocked(ctx)
	r.mu.Unlock()
	return result, err
}

func (r *Runtime) hooks(ctx context.Context, node map[string]any, phase string, env map[string]any, path string, value any, subjectErr error) error {
	if len(list(node[phase])) == 0 {
		return nil
	}
	hctx := ctx
	if phase == "finally" && (subjectErr != nil || ctx.Err() != nil) {
		r.mu.Lock()
		if r.State.CleanupDeadline.IsZero() {
			r.State.CleanupDeadline = time.Now().Add(r.cleanupTimeout)
		}
		deadline := r.State.CleanupDeadline
		saveErr := r.saveLocked(ctx)
		r.mu.Unlock()
		if saveErr != nil {
			return saveErr
		}
		var cancel context.CancelFunc
		hctx, cancel = context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
	}
	hctx, cancel := context.WithTimeout(hctx, r.hookTimeout)
	defer cancel()
	he := contextCopy(env)
	event := map[string]any{"phase": phase, "nodeId": text(node["id"]), "path": path, "status": "succeeded", "result": clone(value)}
	if subjectErr != nil {
		he["error"] = errorData(subjectErr)
		event["error"] = he["error"]
		event["status"] = "failed"
	}
	if phase == "onStart" {
		event["status"] = "running"
	}
	he["event"] = event
	var err error
	if phase == "finally" {
		for i, raw := range list(node[phase]) {
			out, hookErr := r.node(hctx, object(raw), he, fmt.Sprintf("%s/%s/%d", path, phase, i), true)
			err = errors.Join(err, hookErr)
			if hookErr == nil {
				maps.Copy(object(he["steps"]), out.Exports)
			}
			if hard(hookErr) {
				break
			}
		}
	} else {
		_, err = r.sequence(hctx, list(node[phase]), he, path+"/"+phase, true)
	}
	if err == nil || phase == "onStart" || hard(err) {
		return err
	}
	r.mu.Lock()
	r.State.Plans[path+"/"+phase+"/error"] = errorData(err)
	saveErr := r.saveLocked(ctx)
	r.mu.Unlock()
	return saveErr
}
