package workflowdef

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var decisionSchema = map[string]any{
	"type": "object", "properties": map[string]any{"allow": map[string]any{"type": "boolean"}, "reason": map[string]any{"type": "string", "maxLength": float64(1024)}},
	"required": []any{"allow"}, "additionalProperties": false,
}

func (d *Definition) breakerPolicies() map[string]map[string]any {
	policies := map[string]map[string]any{}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if policy, ok := node["breaker"]; ok {
			if role := text(node["agent"]); role != "" {
				policies[role] = object(policy)
			}
			for _, role := range list(node["conversation"]) {
				policies[text(role)] = object(policy)
			}
		}
		for _, field := range []string{"steps", "parallel", "then", "else", "onError", "onStart", "onSuccess", "onFailure", "finally"} {
			for _, child := range list(node[field]) {
				walk(object(child))
			}
		}
	}
	walk(d.Data)
	return policies
}

func (r *Runtime) task(ctx context.Context, node, env map[string]any, path string, modify func(*Call)) (any, error) {
	with, err := Resolve(node["with"], env)
	if err != nil {
		return nil, fault("binding.failed", err)
	}
	prompt, err := Prompt(text(node["prompt"]), env)
	if err != nil {
		return nil, fault("binding.failed", err)
	}
	call := Call{Kind: "agent", Role: text(node["agent"]), Prompt: prompt, With: with, Action: text(node["action"]), Credentials: map[string]string{}}
	if _, ok := node["run"]; ok {
		call.Kind = "command"
		for _, arg := range list(node["run"]) {
			call.Argv = append(call.Argv, text(arg))
		}
	}
	if call.Action != "" {
		call.Kind = "action"
	}
	if call.Kind == "agent" && containsRecovery(node) {
		call.Persistent = true
	}
	if output, ok := node["output"]; ok {
		if name, ok := output.(string); ok {
			call.Output = object(r.Definition.Data["schemas"])[name]
		} else {
			call.Output = output
		}
	}
	for slot, name := range object(node["credentials"]) {
		call.Credentials[slot] = text(object(object(r.Definition.Data["sources"])[text(name)])["secret"])
	}
	if modify != nil {
		modify(&call)
	}
	var reply Reply
	for retry := 0; retry <= integer(node["retry"], 0); retry++ {
		call.ID = fmt.Sprintf("%s/call/%d", path, retry)
		call.SessionCreated = func(session string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			key := "session:" + call.ID
			if previous := text(r.State.Plans[key]); previous != "" && previous != session {
				return &Failure{Code: "session.integrity", Message: "original session identity changed", Hard: true, EffectState: "unknown"}
			}
			r.State.Plans[key] = session
			return r.saveLocked(ctx)
		}
		taskCtx, cancel := context.WithTimeout(ctx, milliseconds(node["timeoutMs"], 2*time.Minute))
		reply, err = r.invoke(taskCtx, call, env, false)
		cancel()
		if err == nil {
			return reply.Value, nil
		}
		var failure *Failure
		if !errors.As(err, &failure) || hard(err) || !failure.Retryable || failure.EffectState == "unknown" || failure.Code == "circuit.open" || failure.Code == "circuit.denied" || retry == integer(node["retry"], 0) {
			return nil, err
		}
		delay := time.Second
		for n := 0; n < retry && delay < 30*time.Second; n++ {
			delay *= 2
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return reply.Value, err
}

func (r *Runtime) invoke(ctx context.Context, call Call, env map[string]any, skipPolicy bool) (Reply, error) {
	r.mu.Lock()
	previous, exists := r.State.Calls[call.ID]
	if exists && !previous.Pending {
		if previous.Failure == nil {
			r.mu.Unlock()
			return Reply{Value: clone(previous.Reply.Value), SessionID: previous.Reply.SessionID, Completed: previous.Reply.Completed}, nil
		}
		if !r.RetryFailed || r.retried[call.ID] {
			r.mu.Unlock()
			return previous.Reply, previous.Failure
		}
		r.retried[call.ID] = true
	}
	ordinal := previous.Ordinal
	if !exists || !previous.Pending {
		ordinal++
	}
	r.mu.Unlock()
	policy := r.policies[call.Role]
	epoch := 0
	if call.Kind == "agent" {
		if err := r.circuitAllows(ctx, call.Role, call.ID, policy); err != nil {
			return Reply{}, err
		}
		if !skipPolicy && (policy["agent"] != nil || policy["run"] != nil) {
			if err := r.checkPolicy(ctx, call, env, policy, ordinal); err != nil {
				return Reply{}, errors.Join(err, r.releaseProbe(ctx, call.Role, call.ID))
			}
		}
	}
	r.mu.Lock()
	if call.Kind == "agent" {
		c := r.State.Circuits[call.Role]
		if !c.OpenUntil.IsZero() && (time.Now().Before(c.OpenUntil) || c.Probe != call.ID) {
			r.mu.Unlock()
			return Reply{}, &Failure{Code: "circuit.open", Message: "agent circuit opened before dispatch", EffectState: "notStarted"}
		}
		epoch = c.Epoch
	}
	if !exists || !previous.Pending {
		if r.State.Attempts >= r.maxAttempts {
			r.mu.Unlock()
			return Reply{}, errors.Join(fault("limit.attempts", fmt.Errorf("workflow task-attempt budget exhausted")), r.releaseProbe(ctx, call.Role, call.ID))
		}
		if call.ConversationPath != "" {
			convo := r.State.Conversations[call.ConversationPath]
			if convo.Calls >= convo.MaxCalls {
				r.mu.Unlock()
				return Reply{}, errors.Join(fault("conversation.exhausted", fmt.Errorf("conversation turn budget exhausted")), r.releaseProbe(ctx, call.Role, call.ID))
			}
			convo.Calls++
		}
		r.State.Attempts++
		r.State.Calls[call.ID] = CallResult{Pending: true, Ordinal: ordinal}
		if err := r.saveLocked(ctx); err != nil {
			r.mu.Unlock()
			return Reply{}, err
		}
	}
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Reply{}, &Failure{Code: "execution.canceled", Message: err.Error(), EffectState: "notStarted", Hard: true, Cause: err}
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return Reply{}, &Failure{Code: "execution.canceled", Message: ctx.Err().Error(), Hard: true, EffectState: "notStarted", Cause: ctx.Err()}
	}
	reply, err := r.Host.Invoke(ctx, call)
	<-r.slots
	if err == nil {
		reply.Value, err = r.normalizeReply(call, reply.Value)
	}
	if err == nil && call.Validate != nil {
		reply, err = call.Validate(reply)
	}
	var failure *Failure
	if err != nil {
		failure = fault("execution.failed", err)
		failure.StepID = call.ID
		failure.Agent = call.Role
		failure.Attempt = ordinal
		if failure.EffectState == "unknown" {
			failure.Hard = true
		}
	}
	r.mu.Lock()
	r.State.Calls[call.ID] = CallResult{Reply: reply, Failure: failure, Ordinal: ordinal}
	if call.Kind == "agent" {
		r.observeCircuit(call.Role, call.ID, epoch, policy, failure, call.Diagnostic && !reply.Completed)
	}
	saveErr := r.saveLocked(ctx)
	r.mu.Unlock()
	if saveErr != nil {
		return reply, errors.Join(err, saveErr)
	}
	if failure != nil {
		return reply, failure
	}
	return reply, nil
}

func (r *Runtime) normalizeReply(call Call, value any) (any, error) {
	// Provider/action adapters may use json.Number or integer Go values. Bindings
	// and JSONPath operate on the same JSON number representation on every run.
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fault("validation.output", err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fault("validation.output", err)
	}
	if call.Output == nil {
		if call.Kind == "agent" {
			if stringValue, ok := value.(string); ok {
				return map[string]any{"text": stringValue}, nil
			}
		}
		return value, nil
	}
	if m, ok := value.(map[string]any); ok {
		if call.Kind == "agent" {
			if structured, ok := m["structured"]; ok {
				value = structured
			} else if raw, ok := m["text"].(string); ok {
				decoded, err := DecodeJSON([]byte(raw))
				if err != nil {
					return value, fault("validation.output", err)
				}
				value = decoded
			}
		}
		if call.Kind == "command" {
			if raw, ok := m["stdout"].(string); ok {
				decoded, err := DecodeJSON([]byte(raw))
				if err != nil {
					return value, fault("validation.output", err)
				}
				value = decoded
			}
		}
	}
	if err := r.Definition.ValidateValue(call.Output, value); err != nil {
		failure := fault("validation.output", err)
		failure.EffectState = "completed"
		return value, failure
	}
	return value, nil
}

func (r *Runtime) circuitAllows(ctx context.Context, role, id string, policy map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.State.Circuits[role]
	if c == nil {
		c = &Circuit{}
		r.State.Circuits[role] = c
	}
	if c.OpenUntil.IsZero() {
		return nil
	}
	if time.Now().Before(c.OpenUntil) || (c.Probe != "" && c.Probe != id) {
		return &Failure{Code: "circuit.open", Message: "agent circuit is open", Agent: role, StepID: id, EffectState: "notStarted"}
	}
	if c.Probe == "" {
		c.Probe = id
		if err := r.saveLocked(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) releaseProbe(ctx context.Context, role, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.State.Circuits[role]; c != nil && c.Probe == id {
		c.Probe = ""
		return r.saveLocked(ctx)
	}
	return nil
}

// Called under mu. Epochs fence late completions from an older breaker state.
func (r *Runtime) observeCircuit(role, id string, epoch int, policy map[string]any, failure *Failure, diagnostic bool) {
	c := r.State.Circuits[role]
	if c == nil || c.Epoch != epoch {
		return
	}
	if !c.OpenUntil.IsZero() && c.Probe != id {
		return
	}
	if failure == nil {
		if !diagnostic || c.Probe == id {
			wasOpen := !c.OpenUntil.IsZero()
			c.Failures = 0
			c.OpenUntil = time.Time{}
			c.Probe = ""
			c.LastError = nil
			if wasOpen {
				c.Epoch++
			}
		}
		return
	}
	if failure.Hard || failure.Abort || strings.HasPrefix(failure.Code, "circuit.") {
		return
	}
	c.Failures++
	c.LastError = errorData(failure)
	if c.Probe == id || c.Failures >= integer(policy["failures"], 3) {
		c.OpenUntil = time.Now().Add(milliseconds(policy["cooldownMs"], 30*time.Second))
		c.Probe = ""
		c.Epoch++
	}
}

func (r *Runtime) checkPolicy(ctx context.Context, guard Call, env, policy map[string]any, ordinal int) error {
	r.mu.Lock()
	c := r.State.Circuits[guard.Role]
	snapshot := map[string]any{"role": guard.Role, "invocation": guard.ID, "state": "closed", "consecutiveFailures": float64(c.Failures), "attemptsRemaining": float64(r.maxAttempts - r.State.Attempts), "lastError": clone(c.LastError)}
	if !c.OpenUntil.IsZero() {
		snapshot["state"] = "half-open"
	}
	r.mu.Unlock()
	pe := contextCopy(env)
	pe["breaker"] = snapshot
	prompt := text(policy["prompt"])
	if prompt == "" {
		prompt = "Decide whether the guarded call may proceed. Return JSON with boolean allow and optional reason. State: {{ $.breaker }}"
	}
	prompt, err := Prompt(prompt, pe)
	if err != nil {
		return fault("circuit.policy_failed", err)
	}
	call := Call{ID: fmt.Sprintf("%s/policy/%d", guard.ID, ordinal), Kind: "agent", Role: text(policy["agent"]), Prompt: prompt, With: snapshot, Output: decisionSchema, ConversationPath: guard.ConversationPath}
	if policy["run"] != nil {
		call.Kind = "command"
		call.Role = ""
		for _, arg := range list(policy["run"]) {
			call.Argv = append(call.Argv, text(arg))
		}
	}
	policyCtx, cancel := context.WithTimeout(ctx, min(milliseconds(policy["timeoutMs"], 10*time.Second), 10*time.Second))
	defer cancel()
	reply, err := r.invoke(policyCtx, call, pe, true)
	if err != nil {
		if hard(err) {
			return err
		}
		return fault("circuit.policy_failed", fmt.Errorf("breaker checker failed: %w", err))
	}
	decision := object(reply.Value)
	if decision["allow"] == true {
		return nil
	}
	reason := text(decision["reason"])
	if reason == "" {
		reason = "custom breaker denied the call"
	}
	r.mu.Lock()
	c = r.State.Circuits[guard.Role]
	c.OpenUntil = time.Now().Add(milliseconds(policy["cooldownMs"], 30*time.Second))
	c.Probe = ""
	c.Epoch++
	err = r.saveLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	return &Failure{Code: "circuit.denied", Message: reason, StepID: guard.ID, Agent: guard.Role, EffectState: "notStarted"}
}

// SchemaJSON returns a detached inline resource for provider structured output.
func (d *Definition) SchemaJSON(use any) (json.RawMessage, error) {
	if name, ok := use.(string); ok {
		use = object(d.Data["schemas"])[name]
	}
	if _, err := d.schema(use); err != nil {
		return nil, err
	}
	return json.Marshal(use)
}

// Roles includes policies, recovery helpers, and conversation participants.
func (d *Definition) Roles() []string {
	roles := map[string]bool{}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		for _, key := range []string{"agent", "recover"} {
			if role := text(node[key]); role != "" {
				roles[role] = true
			}
		}
		for _, role := range list(node["conversation"]) {
			roles[text(role)] = true
		}
		if policy := object(node["breaker"]); policy != nil {
			if role := text(policy["agent"]); role != "" {
				roles[role] = true
			}
		}
		for _, field := range []string{"steps", "parallel", "then", "else", "onError", "onStart", "onSuccess", "onFailure", "finally"} {
			for _, child := range list(node[field]) {
				walk(object(child))
			}
		}
	}
	walk(d.Data)
	return slicesSortedKeys(roles)
}
func slicesSortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
