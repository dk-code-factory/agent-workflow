package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

const conversationToolServer = "workflow-conversation"

// These receipts use the same controller-owned send/finish protocol as JSON
// conversations. Sender identity and turn ownership never come from arguments.
type ConversationDelivery struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Message string `json:"message"`
}
type ConversationReceipt struct {
	Fingerprint string          `json:"fingerprint"`
	Reply       json.RawMessage `json:"reply"`
}
type ConversationState struct {
	Roles            []string                       `json:"roles"`
	Sessions         map[string]string              `json:"sessions"`
	Queue            []ConversationDelivery         `json:"queue"`
	Active           *ConversationDelivery          `json:"active,omitempty"`
	Transcript       []ConversationDelivery         `json:"transcript"`
	Receipts         map[string]ConversationReceipt `json:"receipts"`
	Turns            int                            `json:"turns"`
	Calls            int                            `json:"calls"`
	MaxCalls         int                            `json:"maxCalls"`
	Messages         int                            `json:"messages"`
	MaxMessages      int                            `json:"maxMessages"`
	Prompt           string                         `json:"prompt"`
	FinishBy         string                         `json:"finishBy"`
	Finished         bool                           `json:"finished"`
	Result           any                            `json:"result,omitempty"`
	Failure          string                         `json:"failure,omitempty"`
	IntegrityFailure bool                           `json:"integrityFailure,omitempty"`
	Deadline         time.Time                      `json:"deadline"`
}

func conversationPrompt(step Step) string {
	if step.Prompt != nil {
		return *step.Prompt
	}
	return ""
}

func validateConversationStep(w *Workflow, step Step) error {
	if len(step.Conversation) < 2 || len(step.Conversation) > 16 {
		return fmt.Errorf("conversation requires between 2 and 16 participants")
	}
	seen := map[string]bool{}
	for _, role := range step.Conversation {
		if !idPattern.MatchString(role) || seen[role] {
			return fmt.Errorf("invalid or duplicate conversation participant %q", role)
		}
		seen[role] = true
	}
	if !seen[step.FinishBy] {
		return fmt.Errorf("finishBy must name a declared conversation participant")
	}
	if step.Start != "" && !seen[step.Start] {
		return fmt.Errorf("start must name a declared conversation participant")
	}
	if strings.TrimSpace(conversationPrompt(step)) == "" {
		return fmt.Errorf("conversation requires prompt")
	}
	if step.MaxTurns < 0 || step.MaxTurns > 100 {
		return fmt.Errorf("maxTurns must be between 1 and 100")
	}
	if step.MaxMessages < 0 || step.MaxMessages > 256 {
		return fmt.Errorf("maxMessages must be between 1 and 256")
	}
	if step.Timeout != "" {
		d, err := time.ParseDuration(step.Timeout)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("conversation timeout must be positive and at most 24h")
		}
	}
	if step.Agent != nil || step.Uses != "" || len(step.Argv) != 0 || step.Run != "" || step.Shell != "" || step.WorkingDirectory != "" || step.Context != (RunContext{}) || isStepBlock(step) {
		return fmt.Errorf("conversation cannot combine with another operation or context")
	}
	for key := range step.With {
		if key != "output" && key != "model" && key != "workspace" {
			return fmt.Errorf("unsupported conversation parameter %q", key)
		}
	}
	output, err := resolveNamedOutput(w, step.With["output"])
	if err != nil {
		return err
	}
	contract, err := ParseAgentOutputContract(output)
	if err != nil {
		return err
	}
	if output != nil && !contract.Enabled() {
		return fmt.Errorf("conversation output must declare a JSON contract")
	}
	if contract != nil {
		if typ, ok := contract.Schema["type"]; ok && typ != "object" {
			return fmt.Errorf("conversation completion schema must have type: object")
		}
		if err := contract.Validate(); err != nil {
			return err
		}
		if err := validateNamedSchema(contract.Schema, "with.output.schema", 0); err != nil {
			return err
		}
	}
	return nil
}

type conversationJournal interface {
	stepBlockJournal
	BeginConversation(context.Context, string, string, string, int, string, string) (Attempt, bool, error)
	ConversationAttempt(context.Context, string, string, string, int, string) (Attempt, bool, error)
}

func conversationJournalFor(store RunStore) conversationJournal {
	if wrapped, ok := store.(redactedStore); ok {
		return conversationJournalFor(wrapped.Journal)
	}
	journal, _ := store.(conversationJournal)
	return journal
}
func (e *Executor) preflightConversations(all map[string]*Workflow) error {
	for _, w := range all {
		for _, job := range w.Jobs {
			for _, step := range FlattenSteps(job.Steps) {
				if step.Conversation == nil {
					continue
				}
				if conversationJournalFor(e.Store) == nil {
					return fmt.Errorf("conversation requires a durable orchestration Journal")
				}
				sessions, ok := e.Agents.(SessionRunner)
				if !ok {
					return fmt.Errorf("conversation requires a SessionRunner")
				}
				for _, role := range step.Conversation {
					caps, err := sessions.Capabilities(role)
					if err != nil {
						return err
					}
					if !caps.Sessions || !caps.Resume || !caps.ManagedPolicy {
						return fmt.Errorf("agent %s requires resumable sessions and managed tool policy", role)
					}
				}
			}
		}
	}
	return nil
}

type conversationExecution struct {
	mu       sync.Mutex
	e        *Executor
	attempt  *Attempt
	state    *ConversationState
	contract *AgentOutputContract
	err      error
}

func (c *conversationExecution) save(ctx context.Context) error {
	err := c.e.Store.(Journal).RecordAttempt(ctx, *c.attempt)
	if err != nil {
		c.err = errors.Join(c.err, errStepPersistence, err)
		return c.err
	}
	return nil
}
func (c *conversationExecution) failure() error {
	if c.err != nil {
		return c.err
	}
	if c.state.Failure == "" {
		return nil
	}
	if c.state.IntegrityFailure {
		return fmt.Errorf("%w: %s", ErrActionIntegrity, c.state.Failure)
	}
	return errors.New(c.state.Failure)
}
func (c *conversationExecution) fail(ctx context.Context, err error, integrity bool) error {
	c.state.Failure = err.Error()
	c.state.IntegrityFailure = integrity
	return errors.Join(c.failure(), c.save(context.WithoutCancel(ctx)))
}

func (e *Executor) executeConversationStep(ctx context.Context, step Step, ec expressionContext, req RunRequest, slots chan struct{}) (result StepExecution, err error) {
	result = StepExecution{ID: step.ID, Status: RunStatusSucceeded, StartedAt: time.Now().UTC()}
	journal := e.Store.(Journal)
	definition, err := renderMap(map[string]any{"participants": step.Conversation, "start": step.Start, "finishBy": step.FinishBy, "maxTurns": step.MaxTurns, "maxMessages": step.MaxMessages, "timeout": step.Timeout, "prompt": conversationPrompt(step), "with": step.With}, ec)
	if err != nil {
		return result, err
	}
	logical := nativeString(ec.Metadata, "iteration_logical_run_id")
	attempt, reused, err := conversationJournalFor(e.Store).BeginConversation(ctx, ec.RunID, ec.JobID, step.ID, ec.IterationNumber, actionHash(definition), logical)
	if err != nil {
		return result, err
	}
	if reused {
		return attempt.Result, nil
	}
	result.AttemptID = attempt.ID
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err != nil {
			result.Status, result.Error, result.FailureClass = executionFailureStatus(ctx, err), err.Error(), classifyFailure(err)
		}
		if saveErr := journal.Complete(context.WithoutCancel(ctx), attempt, result); saveErr != nil {
			err = errors.Join(err, errStepPersistence, saveErr)
		}
	}()
	args, err := renderMap(step.With, ec)
	if err != nil {
		return result, err
	}
	contract, err := ParseAgentOutputContract(args["output"])
	if err != nil {
		return result, err
	}
	if contract == nil {
		contract = &AgentOutputContract{Format: "json", Schema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false}}
	}
	if len(contract.Schema) == 0 {
		contract.Schema = map[string]any{"type": "object"}
	}
	if err := contract.Validate(); err != nil {
		return result, err
	}
	originalSchema, _ := json.Marshal(contract.Schema)
	safeSchema, _ := json.Marshal(redactValue(contract.Schema, e.redactions(ec.Secrets)))
	if string(originalSchema) != string(safeSchema) {
		return result, fmt.Errorf("structured schema contains credential material")
	}

	if attempt.Conversation == nil {
		rendered, err := renderString(conversationPrompt(step), ec)
		if err != nil {
			return result, err
		}
		prompt, ok := rendered.(string)
		if !ok || strings.TrimSpace(prompt) == "" {
			return result, fmt.Errorf("conversation prompt is empty or not a string")
		}
		start := step.Start
		if start == "" {
			start = step.Conversation[0]
		}
		maxTurns, maxMessages, timeout := step.MaxTurns, step.MaxMessages, 2*time.Minute
		if maxTurns == 0 {
			maxTurns = 8
		}
		if maxMessages == 0 {
			maxMessages = 32
		}
		if step.Timeout != "" {
			timeout, _ = time.ParseDuration(step.Timeout)
		}
		attempt.Conversation = &ConversationState{Roles: slices.Clone(step.Conversation), Sessions: map[string]string{}, Receipts: map[string]ConversationReceipt{}, Queue: []ConversationDelivery{{ID: "start", From: "engine", To: start, Message: prompt}}, Prompt: prompt, FinishBy: step.FinishBy, MaxCalls: maxTurns, MaxMessages: maxMessages, Deadline: time.Now().UTC().Add(timeout)}
	}
	c := &conversationExecution{e: e, attempt: &attempt, state: attempt.Conversation, contract: contract}
	if err := c.save(ctx); err != nil {
		return result, err
	}
	ctx, cancel := context.WithDeadline(ctx, c.state.Deadline)
	defer cancel()
	for {
		// Reconstruct a committed active turn before enforcing the elapsed deadline:
		// consuming a durable receipt does not launch provider work.
		if c.state.Active != nil {
			turnID := step.ID + "/conversation/" + fmt.Sprint(c.state.Turns)
			retained, found, readErr := conversationJournalFor(e.Store).ConversationAttempt(context.WithoutCancel(ctx), ec.RunID, ec.JobID, turnID, ec.IterationNumber, logical)
			if readErr != nil {
				return result, errors.Join(errStepPersistence, readErr)
			}
			if found && retained.Status == RunStatusSucceeded {
				c.state.Active = nil
				c.state.Turns++
				if c.state.Finished {
					c.state.Queue = nil
				}
				if err := c.save(context.WithoutCancel(ctx)); err != nil {
					return result, err
				}
				continue
			}
		}
		if c.state.Finished && c.state.Active == nil {
			raw, err := json.Marshal(c.state.Result)
			if err != nil {
				return result, err
			}
			result.Outputs = map[string]any{"structured": cloneJSONValue(c.state.Result), "text": string(raw), "raw_json": string(raw), "valid": true, "turns": c.state.Calls, "messages": c.state.Messages}
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := e.checkEffectAdmission(ctx, ec.RunID); err != nil {
			return result, err
		}
		if err := c.failure(); err != nil {
			return result, err
		}
		if c.state.Active == nil {
			if len(c.state.Queue) == 0 {
				return result, c.fail(ctx, fmt.Errorf("conversation stalled: no runnable message or final result"), false)
			}
			if c.state.Calls >= c.state.MaxCalls {
				return result, c.fail(ctx, fmt.Errorf("conversation turn budget exhausted"), false)
			}
			delivery := c.state.Queue[0]
			c.state.Queue = c.state.Queue[1:]
			c.state.Active = &delivery
			c.state.Calls++
			if err := c.save(ctx); err != nil {
				return result, err
			}
		}
		active := *c.state.Active
		// Internal turn IDs cannot collide with user-defined step IDs.
		turnID := step.ID + "/conversation/" + fmt.Sprint(c.state.Turns)
		var turn Attempt
		var reused bool
		var err error
		if req.ordered {
			turn, reused, err = iterationJournal(e.Store).BeginInput(ctx, ec.RunID, ec.JobID, turnID, ec.IterationNumber, false, actionHash([]any{definition, active}), false, logical)
		} else {
			turn, reused, err = journal.Begin(ctx, ec.RunID, ec.JobID, turnID, ec.IterationNumber, false)
		}
		if err != nil {
			return result, err
		}
		if !reused {
			turnEC := ec
			turnEC.Attempt = &turn
			turnEC.Literal = true
			prompt := fmt.Sprintf("Shared task: %s\nYou are %s. Participants: %s. Only %s may finish. Use send(to, message) to communicate and finish(result) to complete.\nMessage from %s:\n%s", c.state.Prompt, active.To, strings.Join(c.state.Roles, ", "), c.state.FinishBy, active.From, active.Message)
			child := Step{ID: turnID, Uses: "agent/" + active.To, With: map[string]any{"prompt": prompt}}
			for _, key := range []string{"model", "workspace"} {
				if value, ok := args[key]; ok {
					child.With[key] = value
				}
			}
			ownerRun := logical
			if ownerRun == "" {
				ownerRun = ec.RunID
			}
			runner := &conversationAgent{base: e.Agents, conversation: c, role: active.To, session: c.state.Sessions[active.To], owner: actionHash([]any{ownerRun, ec.JobID, step.ID, ec.IterationNumber, active.To})[:32]}
			execution := *e
			execution.Agents = runner
			baseBinder := e.BindTools
			execution.BindTools = func(tc context.Context, inv ToolInvocation) (ToolBinding, error) {
				var base ToolBinding
				if baseBinder != nil {
					var err error
					base, err = baseBinder(tc, inv)
					if err != nil {
						return nil, err
					}
				}
				return &conversationTools{base: base, execution: c, role: active.To, turn: turnID}, nil
			}
			observed := StepExecution{ID: turnID, AttemptID: turn.ID, Status: RunStatusSucceeded, StartedAt: time.Now().UTC()}
			observed.Outputs, err = execution.executeStep(invocationContext(ctx, e.Store, &turn), child, turnEC, slots)
			observed.FinishedAt = time.Now().UTC()
			if err != nil {
				observed.Status, observed.Error, observed.FailureClass = executionFailureStatus(ctx, err), err.Error(), classifyFailure(err)
			}
			if saveErr := journal.Complete(context.WithoutCancel(ctx), turn, observed); saveErr != nil {
				err = errors.Join(err, errStepPersistence, saveErr)
			}
			if err != nil {
				return result, err
			}
		}
		c.state.Active = nil
		c.state.Turns++
		if c.state.Finished {
			c.state.Queue = nil
		}
		if err := c.save(ctx); err != nil {
			return result, err
		}
	}
}

type conversationAgent struct {
	base         AgentRunner
	conversation *conversationExecution
	role         string
	session      string
	owner        string
}

func (a *conversationAgent) SecretValues() map[string]string {
	if source, ok := a.base.(interface{ SecretValues() map[string]string }); ok {
		return source.SecretValues()
	}
	return nil
}
func (a *conversationAgent) UsageProvider(role, model string) (string, string) {
	if source, ok := a.base.(interface {
		UsageProvider(string, string) (string, string)
	}); ok {
		return source.UsageProvider(role, model)
	}
	return "", model
}
func (a *conversationAgent) RunAgent(ctx context.Context, req AgentRequest) (map[string]any, error) {
	req.SessionOwnerID = a.owner
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	retain := func(id string) {
		if id == "" {
			return
		}
		c := a.conversation
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return
		}
		if old := c.state.Sessions[a.role]; old != "" && old != id {
			c.err = c.fail(ctx, fmt.Errorf("participant session identity changed"), true)
		} else if c.state.Sessions[a.role] != id {
			c.state.Sessions[a.role] = id
			c.err = c.save(context.WithoutCancel(ctx))
		}
		if c.err != nil {
			cancel()
		}
	}
	result, err := a.base.(SessionRunner).RunTurn(ctx, SessionRequest{AgentRequest: req, SessionID: a.session}, func(event ProviderEvent) { retain(event.SessionID) })
	retain(result.SessionID)
	c := a.conversation
	c.mu.Lock()
	defer c.mu.Unlock()
	err = errors.Join(err, c.failure())
	if err == nil && c.state.Sessions[a.role] == "" {
		err = c.fail(ctx, fmt.Errorf("participant did not retain a session"), true)
	}
	if err == nil && result.Status != RunStatusSucceeded {
		err = fmt.Errorf("participant turn ended with status %s", result.Status)
	}
	if err == nil && !c.state.Finished && len(c.state.Queue) == 0 {
		err = c.fail(ctx, fmt.Errorf("conversation stalled: participant returned without send or finish"), false)
	}
	outputs, _ := cloneJSONValue(result.Outputs).(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	return outputs, err
}

type conversationTools struct {
	base      ToolBinding
	execution *conversationExecution
	role      string
	turn      string
}

func (b *conversationTools) Catalog() []ToolDefinition {
	var out []ToolDefinition
	if b.base != nil {
		out = append(out, b.base.Catalog()...)
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"enum": b.execution.state.Roles}, "message": map[string]any{"type": "string", "maxLength": 65536}}, "required": []string{"to", "message"}, "additionalProperties": false})
	finish, _ := json.Marshal(b.execution.contract.Schema)
	for _, t := range []ToolDefinition{{Server: conversationToolServer, Name: "send", Description: "Send a message to a declared peer. The controller authenticates the sender.", InputSchema: schema}, {Server: conversationToolServer, Name: "finish", Description: "Complete the conversation. Only the designated finisher may call this.", InputSchema: finish}} {
		t.ControllerValidation = true
		t.Fingerprint = actionHash(t.InputSchema)
		out = append(out, t)
	}
	return out
}
func (b *conversationTools) Call(ctx context.Context, req ToolRequest) (json.RawMessage, error) {
	if req.Server != conversationToolServer {
		if b.base == nil {
			return nil, fmt.Errorf("tool is unavailable")
		}
		return b.base.Call(ctx, req)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := b.execution
	if err := c.e.checkEffectAdmission(ctx, c.attempt.RunID); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.failure(); err != nil {
		return nil, err
	}
	if req.CallID == "" {
		return nil, fmt.Errorf("message tool call ID is required")
	}
	if len(req.Arguments) > 1<<20 {
		return nil, fmt.Errorf("conversation tool arguments exceed 1 MiB")
	}
	var value any
	if err := decodeJSONWithNumbers(req.Arguments, &value); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte(req.Tool+"\x00"), req.Arguments...))
	fingerprint := hex.EncodeToString(sum[:])
	key := b.turn + "/" + req.CallID
	if old, ok := c.state.Receipts[key]; ok {
		if old.Fingerprint != fingerprint {
			return nil, c.fail(ctx, fmt.Errorf("message tool request ID reused with changed arguments"), true)
		}
		return slices.Clone(old.Reply), nil
	}
	if c.state.Active == nil || c.state.Active.To != b.role || b.turn != c.attempt.StepID+"/conversation/"+fmt.Sprint(c.state.Turns) {
		return nil, fmt.Errorf("participant does not own the active turn")
	}
	if c.state.Sessions[b.role] == "" {
		return nil, fmt.Errorf("participant session must be retained before messaging")
	}
	if c.state.Finished {
		return nil, fmt.Errorf("conversation delivery is closed")
	}
	switch req.Tool {
	case "send":
		args, ok := value.(map[string]any)
		if !ok || len(args) != 2 {
			return nil, fmt.Errorf("send requires exactly to and message")
		}
		to, toOK := args["to"].(string)
		message, messageOK := args["message"].(string)
		if !toOK || !messageOK || to == b.role || !slices.Contains(c.state.Roles, to) || len(message) > 65536 {
			return nil, fmt.Errorf("invalid conversation recipient or message")
		}
		if c.state.Messages >= c.state.MaxMessages {
			return nil, c.fail(ctx, fmt.Errorf("conversation message budget exhausted"), false)
		}
		delivery := ConversationDelivery{ID: key, From: b.role, To: to, Message: message}
		c.state.Queue = append(c.state.Queue, delivery)
		c.state.Transcript = append(c.state.Transcript, delivery)
		c.state.Messages++
	case "finish":
		if b.role != c.state.FinishBy {
			return nil, fmt.Errorf("only %s may finish", c.state.FinishBy)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, c.fail(ctx, fmt.Errorf("invalid structured output: completion must be a JSON object"), false)
		}
		checked := ValidateAgentStructuredOutput(string(req.Arguments), c.contract)
		if !checked.Valid {
			return nil, c.fail(ctx, fmt.Errorf("invalid structured output: %s", checked.Error), false)
		}
		c.state.Finished = true
		c.state.Result = checked.Structured
	default:
		return nil, fmt.Errorf("unknown conversation tool %q", req.Tool)
	}
	reply := json.RawMessage(`{"accepted":true}`)
	c.state.Receipts[key] = ConversationReceipt{Fingerprint: fingerprint, Reply: reply}
	if err := c.save(ctx); err != nil {
		return nil, err
	}
	return reply, nil
}
