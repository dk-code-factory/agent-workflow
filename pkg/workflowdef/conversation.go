package workflowdef

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Delivery struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Message string `json:"message"`
}
type MessageReceipt struct {
	Fingerprint string          `json:"fingerprint"`
	Reply       json.RawMessage `json:"reply"`
}
type Conversation struct {
	Roles      []string                  `json:"roles"`
	Sessions   map[string]string         `json:"sessions"`
	Queue      []Delivery                `json:"queue"`
	Active     *Delivery                 `json:"active,omitempty"`
	Transcript []any                     `json:"transcript"`
	Receipts   map[string]MessageReceipt `json:"receipts"`
	Turns      int                       `json:"turns"`
	Calls      int                       `json:"calls"`
	MaxCalls   int                       `json:"maxCalls"`
	Messages   int                       `json:"messages"`
	Prompt     string                    `json:"prompt"`
	FinishBy   string                    `json:"finishBy"`
	Finished   bool                      `json:"finished"`
	Result     any                       `json:"result,omitempty"`
	Failure    *Failure                  `json:"failure,omitempty"`
	Deadline   time.Time                 `json:"deadline"`
}

func (r *Runtime) conversation(ctx context.Context, node, env map[string]any, path string) (any, error) {
	r.mu.Lock()
	convo := r.State.Conversations[path]
	r.mu.Unlock()
	if convo == nil {
		prompt, err := Prompt(text(node["prompt"]), env)
		if err != nil {
			return nil, err
		}
		roles := []string{}
		for _, role := range list(node["conversation"]) {
			roles = append(roles, text(role))
		}
		start := text(node["start"])
		if start == "" {
			start = roles[0]
		}
		deadline := r.State.Deadline
		if _, ok := node["timeoutMs"]; ok {
			candidate := time.Now().Add(milliseconds(node["timeoutMs"], 2*time.Minute))
			if candidate.Before(deadline) {
				deadline = candidate
			}
		}
		convo = &Conversation{Roles: roles, Sessions: map[string]string{}, Queue: []Delivery{{ID: path + "/start", From: "engine", To: start, Message: prompt}}, Transcript: []any{}, Receipts: map[string]MessageReceipt{}, Prompt: prompt, FinishBy: text(node["finishBy"]), MaxCalls: integer(node["maxTurns"], 8), Deadline: deadline}
		if role, session := text(node["_originalRole"]), text(node["_originalSession"]); role != "" {
			convo.Sessions[role] = session
		}
		r.mu.Lock()
		r.State.Conversations[path] = convo
		err = r.saveLocked(ctx)
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithDeadline(ctx, convo.Deadline)
	defer cancel()
	for {
		r.mu.Lock()
		if convo.Finished && convo.Active == nil {
			value := clone(convo.Result)
			r.mu.Unlock()
			return value, nil
		}
		if convo.Failure != nil {
			failure := *convo.Failure
			r.mu.Unlock()
			return nil, &failure
		}
		if convo.Active == nil {
			if len(convo.Queue) == 0 {
				r.mu.Unlock()
				return nil, fault("conversation.stalled", fmt.Errorf("conversation has no runnable messages or final result"))
			}
			message := convo.Queue[0]
			convo.Queue = convo.Queue[1:]
			convo.Active = &message
			if err := r.saveLocked(ctx); err != nil {
				r.mu.Unlock()
				return nil, err
			}
		}
		active := *convo.Active
		turn := convo.Turns
		session := convo.Sessions[active.To]
		r.mu.Unlock()
		prompt := fmt.Sprintf("Shared task: %s\nYou are %s. Participants: %s. Only %s may finish. Use send(to, message) to communicate and finish(result) to complete.\nMessage from %s:\n%s", convo.Prompt, active.To, strings.Join(convo.Roles, ", "), convo.FinishBy, active.From, active.Message)
		call := Call{ID: fmt.Sprintf("%s/turn/%d", path, turn), Kind: "agent", Role: active.To, Prompt: prompt, SessionID: session, Persistent: true, ConversationPath: path, Diagnostic: text(node["_originalRole"]) == active.To}
		call.SessionCreated = func(id string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if old := convo.Sessions[active.To]; old != "" && old != id {
				return &Failure{Code: "session.integrity", Message: "participant session identity changed", Hard: true, EffectState: "unknown"}
			}
			convo.Sessions[active.To] = id
			return r.saveLocked(ctx)
		}
		call.Tools = r.conversationTools(node, convo.Roles)
		call.ToolCall = func(tc context.Context, id, name string, args json.RawMessage) (json.RawMessage, error) {
			return r.messageTool(tc, path, call.ID, active.To, node, id, name, args)
		}
		call.Validate = func(reply Reply) (Reply, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if reply.SessionID != "" {
				if old := convo.Sessions[active.To]; old != "" && old != reply.SessionID {
					return reply, &Failure{Code: "session.integrity", Message: "participant session identity changed", Hard: true, EffectState: "unknown"}
				}
				convo.Sessions[active.To] = reply.SessionID
			}
			if convo.Sessions[active.To] == "" {
				return reply, &Failure{Code: "session.unavailable", Message: "provider did not retain a participant session", EffectState: "unknown", Hard: true}
			}
			if convo.Failure != nil {
				return reply, convo.Failure
			}
			if !convo.Finished && len(convo.Queue) == 0 {
				return reply, fault("conversation.stalled", fmt.Errorf("participant returned without a message or finish"))
			}
			reply.Completed = convo.Finished
			return reply, nil
		}
		_, err := r.invoke(ctx, call, env, false)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		convo.Active = nil
		if convo.Finished && len(convo.Queue) > 0 {
			for _, queued := range convo.Queue {
				convo.Transcript = append(convo.Transcript, map[string]any{"discarded": queued.ID, "to": queued.To})
			}
			convo.Queue = nil
		}
		convo.Turns++
		err = r.saveLocked(ctx)
		r.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (r *Runtime) conversationTools(node map[string]any, roles []string) []Tool {
	to := make([]any, len(roles))
	for i, role := range roles {
		to[i] = role
	}
	sendSchema, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"enum": to}, "message": map[string]any{"type": "string", "maxLength": float64(65536)}}, "required": []any{"to", "message"}, "additionalProperties": false})
	finishSchema := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)
	if use, ok := node["output"]; ok {
		finishSchema, _ = r.Definition.SchemaJSON(use)
	}
	return []Tool{{Name: "send", Description: "Send a message to another declared participant. Sender identity is assigned by the engine.", Schema: sendSchema}, {Name: "finish", Description: "Complete this conversation with its result. Only the designated finisher may call this.", Schema: finishSchema}}
}

func (r *Runtime) messageTool(ctx context.Context, path, turn, role string, node map[string]any, id, name string, args json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("message tool call ID is required")
	}
	value, err := DecodeJSON(args)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte(name+"\x00"), args...))
	fingerprint := hex.EncodeToString(sum[:])
	key := turn + "/" + id
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.State.Conversations[path]
	if old, exists := c.Receipts[key]; exists {
		if old.Fingerprint != fingerprint {
			c.Failure = &Failure{Code: "message.integrity", Message: "tool request ID reused with changed arguments", EffectState: "unknown", Hard: true}
			return nil, errors.Join(c.Failure, r.saveLocked(ctx))
		}
		return append(json.RawMessage(nil), old.Reply...), nil
	}
	if c.Active == nil || c.Active.To != role || turn != fmt.Sprintf("%s/turn/%d", path, c.Turns) {
		return nil, fmt.Errorf("participant does not own the active turn")
	}
	if c.Finished || c.Failure != nil {
		return nil, fmt.Errorf("conversation delivery is closed")
	}
	var reply json.RawMessage
	switch name {
	case "send":
		m := object(value)
		if len(m) != 2 || !has(m, "message") || !has(m, "to") {
			return nil, fmt.Errorf("send requires exactly to and message")
		}
		to, toOK := m["to"].(string)
		message, messageOK := m["message"].(string)
		if !toOK || !messageOK || to == role || !slices.Contains(c.Roles, to) || len(message) > 65536 {
			return nil, fmt.Errorf("invalid conversation recipient or message")
		}
		if c.Messages >= 32 {
			c.Failure = fault("conversation.messages_exhausted", fmt.Errorf("conversation message budget exhausted"))
			if err := r.saveLocked(ctx); err != nil {
				return nil, err
			}
			return nil, c.Failure
		}
		delivery := Delivery{ID: key, From: role, To: to, Message: message}
		c.Queue = append(c.Queue, delivery)
		c.Messages++
		c.Transcript = append(c.Transcript, map[string]any{"from": role, "to": to, "message": message})
		reply = json.RawMessage(`{"accepted":true}`)
	case "finish":
		if role != c.FinishBy {
			return nil, fmt.Errorf("only %s may finish", c.FinishBy)
		}
		use := any(map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false})
		if declared, ok := node["output"]; ok {
			use = declared
		}
		if err := r.Definition.ValidateValue(use, value); err != nil {
			c.Failure = fault("validation.output", err)
			c.Failure.EffectState = "completed"
			if saveErr := r.saveLocked(ctx); saveErr != nil {
				return nil, saveErr
			}
			return nil, c.Failure
		}
		c.Finished = true
		c.Result = value
		c.Transcript = append(c.Transcript, map[string]any{"from": role, "finish": clone(value)})
		reply = json.RawMessage(`{"accepted":true}`)
	default:
		return nil, fmt.Errorf("unknown conversation tool %q", name)
	}
	c.Receipts[key] = MessageReceipt{Fingerprint: fingerprint, Reply: reply}
	if err := r.saveLocked(ctx); err != nil {
		return nil, err
	}
	return reply, nil
}

func (r *Runtime) recoverAgent(ctx context.Context, node, env map[string]any, path string) (any, error) {
	failed := object(env["failed"])
	original := object(failed["_node"])
	originalRole := text(original["agent"])
	originalPath := text(failed["_agentPath"])
	if originalRole == "" || originalPath == "" || originalRole == text(node["recover"]) {
		return nil, fault("recovery.unavailable", fmt.Errorf("recovery requires a distinct helper and an original agent invocation"))
	}
	r.mu.Lock()
	keys := make([]string, 0)
	for key := range r.State.Calls {
		if strings.HasPrefix(key, originalPath+"/call/") && !strings.Contains(strings.TrimPrefix(key, originalPath+"/call/"), "/") {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		x, _ := strconv.Atoi(strings.TrimPrefix(a, originalPath+"/call/"))
		y, _ := strconv.Atoi(strings.TrimPrefix(b, originalPath+"/call/"))
		return x - y
	})
	session := ""
	if len(keys) > 0 {
		key := keys[len(keys)-1]
		session = r.State.Calls[key].Reply.SessionID
		if session == "" {
			session = text(r.State.Plans["session:"+key])
		}
	}
	r.mu.Unlock()
	if session == "" {
		return nil, fault("recovery.unavailable", fmt.Errorf("original agent has no retained session"))
	}
	detail, _ := json.Marshal(map[string]any{"error": env["error"], "task": text(failed["prompt"]), "input": failed["with"]})
	prompt := "Recover the failed task with the original agent. Helper: diagnose and send recovery instructions to the original. Original agent: preserve your role and permissions; finish only with the successful original task result. Context: " + string(detail)
	if extra := text(node["prompt"]); extra != "" {
		rendered, err := Prompt(extra, env)
		if err != nil {
			return nil, err
		}
		prompt += "\n" + rendered
	}
	conversation := map[string]any{"conversation": []any{text(node["recover"]), originalRole}, "start": node["recover"], "finishBy": originalRole, "prompt": prompt, "maxTurns": float64(integer(node["maxTurns"], 4)), "timeoutMs": float64(milliseconds(node["timeoutMs"], 2*time.Minute) / time.Millisecond), "_originalRole": originalRole, "_originalSession": session}
	if output, ok := original["output"]; ok {
		conversation["output"] = output
	}
	return r.conversation(ctx, conversation, env, path+"/recovery")
}

func containsRecovery(node map[string]any) bool {
	if node["recover"] != nil {
		return true
	}
	for _, key := range []string{"steps", "parallel", "then", "else", "onError"} {
		for _, child := range list(node[key]) {
			if containsRecovery(object(child)) {
				return true
			}
		}
	}
	return false
}

// SessionRoles identifies roles whose adapter must retain resumable sessions.
func (d *Definition) SessionRoles() []string {
	roles := map[string]bool{}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		for _, role := range list(node["conversation"]) {
			roles[text(role)] = true
		}
		if node["agent"] != nil && containsRecovery(node) {
			roles[text(node["agent"])] = true
		}
		if node["recover"] != nil {
			roles[text(node["recover"])] = true
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

func (d *Definition) Operations() []map[string]any {
	out := []map[string]any{}
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node["agent"] != nil || node["run"] != nil || node["action"] != nil {
			out = append(out, maps.Clone(node))
		}
		if policy := object(node["breaker"]); policy != nil && (policy["agent"] != nil || policy["run"] != nil) {
			out = append(out, maps.Clone(policy))
		}
		for _, field := range []string{"steps", "parallel", "then", "else", "onError", "onStart", "onSuccess", "onFailure", "finally"} {
			for _, child := range list(node[field]) {
				walk(object(child))
			}
		}
	}
	walk(d.Data)
	return out
}
