package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

func agentRole(Step) string { return "codex" }

type AgentTaskDispatch struct {
	Reference string         `json:"agent"`
	Operation string         `json:"operation"`
	Input     AgentTaskInput `json:"input"`
}
type AgentTaskInput struct {
	Workspace  string               `json:"workspace,omitempty"`
	Prompt     string               `json:"prompt"`
	Input      any                  `json:"input,omitempty"`
	Output     *AgentOutputContract `json:"output,omitempty"`
	Model      string               `json:"model,omitempty"`
	Definition *AgentDefinition     `json:"definition,omitempty"`
}

func (e *Executor) executeAgentTask(ctx context.Context, step Step, ec expressionContext, request AgentRequest) (map[string]any, error) {
	if e.Workers == nil || ec.Attempt == nil {
		return nil, fmt.Errorf("agent/run requires controller-owned durable workers")
	}
	owner := nativeString(ec.Metadata, "origin_run_id")
	if owner == "" {
		owner = ec.RunID
	}
	logical := nativeString(ec.Metadata, "iteration_logical_run_id")
	if logical == "" {
		logical = ec.RunID
	}
	coordinate := []any{owner, logical, ec.JobID, step.ID, ec.IterationNumber}
	reference := "agent_" + actionHash(coordinate)[:32]
	args, err := renderMap(step.With, ec)
	if err != nil {
		return nil, err
	}
	if step.Definition == nil {
		reference, _ = args["agent"].(string)
		if !strings.HasPrefix(reference, "agent_") {
			return nil, fmt.Errorf("with.agent expression must return an admitted agent reference")
		}
		worker, err := e.Workers.Get(ctx, owner, reference)
		if err != nil {
			return nil, err
		}
		if worker.Definition == nil || worker.Role != "codex" || worker.SessionID == "" {
			return nil, fmt.Errorf("%w: invalid agent session binding", ErrActionIntegrity)
		}
		if request.Workspace != "" && request.Workspace != worker.TaskWorkspace {
			return nil, fmt.Errorf("%w: agent workspace binding changed", ErrActionIntegrity)
		}
		request.Workspace = worker.TaskWorkspace
		request.Definition = worker.Definition
	}
	if request.Model == "" && request.Definition != nil && request.Definition.Model != "" {
		request.Model = request.Definition.Model
	}
	operation := actionHash([]any{coordinate, ec.Attempt.InputFingerprint, request.RepairOnly, ec.Attempt.RepairCount})
	dispatch := &AgentTaskDispatch{Reference: reference, Operation: operation, Input: AgentTaskInput{Workspace: request.Workspace, Prompt: request.Prompt, Input: request.Input, Output: request.Output, Model: request.Model, Definition: request.Definition}}
	encoded, encodeErr := json.Marshal(dispatch.Input)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if Redact(string(encoded), e.redactions(ec.Secrets)) != string(encoded) {
		return nil, fmt.Errorf("agent task inputs must use credential references, not secret values")
	}
	if ec.Attempt.AgentTask != nil && ec.Attempt.AgentTask.Operation == operation && actionHash(ec.Attempt.AgentTask) != actionHash(dispatch) {
		return nil, fmt.Errorf("%w: task dispatch inputs changed", ErrActionIntegrity)
	}
	ec.Attempt.AgentTask = dispatch
	if err = e.Store.(Journal).RecordAttempt(ctx, *ec.Attempt); err != nil {
		return nil, errors.Join(errStepPersistence, err)
	}
	e.Workers.registerTaskSecrets(owner, e.redactions(ec.Secrets))
	fence, _ := ctx.Value(fenceKey{}).(executionFence)
	worker, err := e.Workers.Spawn(ctx, WorkerSpec{RunID: owner, Role: "codex", StableID: reference, TaskWorkspace: request.Workspace, Owner: fence.owner, Definition: request.Definition})
	if err != nil {
		return nil, err
	}
	// An unchanged dispatch receipt is readable even after the producer closed.
	var outcome WorkerMessage
	operationID := operation
	taskRequest := request
	for recovered := 0; recovered <= e.Workers.Limits.MaxTurns; recovered++ {
		existing, lookupErr := e.Workers.taskMessage(ctx, worker.ID, operationID)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, lookupErr
		}
		if errors.Is(lookupErr, sql.ErrNoRows) && (worker.Status == "closed" || worker.Status == RunStatusNeedsAttention) {
			if err = e.Workers.reopenAgentTask(ctx, owner, worker.ID); err != nil {
				return nil, err
			}
			worker, err = e.Workers.Get(ctx, owner, worker.ID)
			if err != nil {
				return nil, err
			}
		}
		message := existing
		if errors.Is(lookupErr, sql.ErrNoRows) {
			message, err = e.Workers.sendTask(ctx, owner, worker.ID, operationID, taskRequest)
			if err != nil {
				return nil, err
			}
		}
		outcome, err = e.Workers.WaitMessage(ctx, WorkerWaitRequest{RunID: owner, OperationID: operationID, WorkerID: worker.ID, MessageID: message.ID, Deadline: message.EnqueuedAt.Add(worker.TurnDuration)})
		if err != nil && ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			joined := e.Workers.joinCanceledTask(cleanup, owner, worker.ID, message.ID)
			if joined != nil {
				cancel()
				return nil, errors.Join(process.ErrCleanup, joined)
			}
			retained, lookup := e.Workers.taskMessage(cleanup, worker.ID, operationID)
			if lookup == nil {
				outcome = retained
			}
			if lookup == nil && retained.Status == RunStatusFailed {
				outcome, err = e.Workers.WaitMessage(cleanup, WorkerWaitRequest{RunID: owner, OperationID: operationID, WorkerID: worker.ID, MessageID: message.ID, Deadline: message.EnqueuedAt.Add(worker.TurnDuration)})
			}
			cancel()
		}
		if err != nil {
			outputs, _ := cloneJSONValue(outcome.Result.Outputs).(map[string]any)
			if outputs == nil {
				outputs = map[string]any{}
			}
			delete(outputs, "session_id")
			outputs["agent"] = reference
			return outputs, err
		}
		if outcome.Status == RunStatusSucceeded || ec.Attempt.Number <= 1 || errors.Is(lookupErr, sql.ErrNoRows) {
			break
		}
		if outcome.UnsafeFailureClass != "" && outcome.UnsafeFailureClass != "canceled" {
			break
		}
		if err = e.Workers.reopenAgentTask(ctx, owner, worker.ID); err != nil {
			return nil, err
		}
		worker, err = e.Workers.Get(ctx, owner, worker.ID)
		if err != nil {
			return nil, err
		}
		operationID = actionHash([]any{outcome.ID, "explicit-session-continuation"})
		taskRequest.Prompt = request.Prompt
	}

	outputs, _ := cloneJSONValue(outcome.Result.Outputs).(map[string]any)
	if outputs == nil {
		outputs = map[string]any{}
	}
	delete(outputs, "session_id")
	outputs["agent"] = reference
	outputs["queue_time_ms"] = outcome.QueueTimeMS
	if outcome.Status != RunStatusSucceeded {
		if outcome.Status == RunStatusFailed && (outcome.UnsafeFailureClass == "" || outcome.UnsafeFailureClass == "canceled") && acknowledgeableFailure(outcome.FailureClass) {
			if _, ackErr := e.Workers.acknowledgeFailure(context.WithoutCancel(ctx), WorkerFailureRequest{RunID: owner, WorkerID: reference, MessageID: outcome.ID, Owner: fence.owner, Generation: fence.generation, RequestID: actionHash([]any{outcome.RequestID, "task-result-observed"}), Reason: "pipeline observed the durable task error", Evidence: actionHash(outcome.Result)}, operationID); ackErr != nil {
				return outputs, ackErr
			}
		}
		status := outcome.Status
		if outcome.FailureClass == "needs_attention" {
			status = RunStatusNeedsAttention
		}
		return outputs, lifecycleError(StepExecution{Status: status, Error: outcome.Error, FailureClass: outcome.FailureClass, UnsafeFailureClass: outcome.UnsafeFailureClass})
	}
	return outputs, nil
}

func (e *Executor) settleAgentTask(ctx context.Context, ec expressionContext, life *StepLifecycle) error {
	if e.Workers == nil || ec.Attempt == nil || ec.Attempt.AgentTask == nil {
		return nil
	}
	dispatch := ec.Attempt.AgentTask
	owner := nativeString(ec.Metadata, "origin_run_id")
	if owner == "" {
		owner = ec.RunID
	}
	messages, err := e.Workers.Messages(ctx, owner, dispatch.Reference)
	if err != nil {
		return err
	}
	for _, msg := range messages {
		if msg.RequestID != dispatch.Operation || msg.Status != RunStatusFailed || msg.FailureAcknowledgement != nil {
			continue
		}
		fence, _ := ctx.Value(fenceKey{}).(executionFence)
		evidence, _ := json.Marshal(life)
		proof := actionHash(string(evidence))
		_, err = e.Workers.AcknowledgeFailure(ctx, WorkerFailureRequest{RunID: owner, WorkerID: dispatch.Reference, MessageID: msg.ID, Owner: fence.owner, Generation: fence.generation, RequestID: actionHash([]any{dispatch.Operation, "recovered"}), Reason: "pipeline onError accepted a verified replacement", Evidence: proof})
		return err
	}
	return nil
}

// Reopen only a known original session after owned work and tool effects have
// settled. This changes no grants, state owner, definition, or remaining turns.
func (m *WorkerManager) reopenAgentTask(ctx context.Context, run, id string) error {
	m.mu.Lock()
	done := m.done[id]
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := m.Store.CheckToolEffects(ctx, run); err != nil {
		return err
	}
	if err := m.Store.CheckInvocationCleanup(ctx, run); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[id] != nil {
		return fmt.Errorf("%w: session still has an active owner", ErrActionIntegrity)
	}
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return fmt.Errorf("%w: %v", ErrActionIntegrity, err)
	}
	if err = checkEffectAdmission(ctx, tx, run); err != nil {
		return err
	}
	var allowed int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.status='running' AND e.finalizing=0`, run).Scan(&allowed); err != nil {
		return err
	}
	if allowed != 1 {
		return fmt.Errorf("%w: originating execution is inactive", ErrActionIntegrity)
	}
	var raw []byte
	var worker Worker
	if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND run_id=?", id, run).Scan(&raw); err != nil {
		return err
	}
	if err = decodeJSONWithNumbers(raw, &worker); err != nil {
		return err
	}
	if worker.Definition == nil || worker.Role != "codex" || worker.SessionID == "" {
		return fmt.Errorf("%w: original agent session binding is unknown", ErrActionIntegrity)
	}
	if worker.Status != "closed" && worker.Status != RunStatusNeedsAttention {
		return nil
	}
	var pending int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE worker_id=? AND status IN ('queued','running')", id).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return fmt.Errorf("%w: agent has unresolved mailbox work", ErrActionIntegrity)
	}
	if worker.Remaining <= 0 {
		return fmt.Errorf("agent turn allowance exhausted")
	}
	worker.Status = "idle"
	if worker.SessionOwnerID == "" {
		worker.SessionOwnerID = worker.ID
	}
	raw, err = json.Marshal(worker)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", worker.Status, raw, id); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, run, "agent.session_reopened", map[string]any{"agent": id, "remaining_turns": worker.Remaining}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.notify()
	m.notifyWaiters()
	return nil
}

func (m *WorkerManager) taskMessage(ctx context.Context, worker, operation string) (WorkerMessage, error) {
	var raw []byte
	var message WorkerMessage
	err := m.Store.db.QueryRowContext(ctx, "SELECT record FROM messages WHERE worker_id=? AND request_id=?", worker, operation).Scan(&raw)
	if err == nil {
		err = decodeJSONWithNumbers(raw, &message)
	}
	return message, err
}

func (m *WorkerManager) registerTaskSecrets(run string, values map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.taskSecrets == nil {
		m.taskSecrets = map[string]map[string]string{}
	}
	if m.taskSecrets[run] == nil {
		m.taskSecrets[run] = map[string]string{}
	}
	for k, v := range values {
		m.taskSecrets[run][actionHash([]any{k, v})] = v
	}
}
func (m *WorkerManager) taskRedactions(run string, values map[string]string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range values {
		out[k] = v
	}
	for k, v := range m.taskSecrets[run] {
		out["task:"+k] = v
	}
	return out
}

// Interrupt only the exact active turn. Queued messages retain their evidence
// for origin cleanup; cancellation must not cancel another session owner.
func (m *WorkerManager) joinCanceledTask(ctx context.Context, run, worker, message string) error {
	m.mu.Lock()
	var active string
	err := m.Store.db.QueryRowContext(ctx, "SELECT id FROM messages WHERE worker_id=? AND status='running'", worker).Scan(&active)
	done := m.done[worker]
	if err == nil && active == message && m.activeRuns[worker] == run {
		if cancel := m.active[worker]; cancel != nil {
			cancel()
		}
	} else {
		done = nil
	}
	m.mu.Unlock()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Ordered pipelines may disposition an explicitly continued native task error
// only after its exact ordinary failure has an authoritative observation receipt.
// Unsafe outcomes and unrelated effects retain the ordered iteration barrier.
func (e *Executor) observedNativeTaskFailure(ctx context.Context, step Step, result StepExecution, ec expressionContext) bool {
	if step.Uses != "agent/run" || e.Workers == nil || result.UnsafeFailureClass != "" || !acknowledgeableFailure(result.FailureClass) {
		return false
	}
	switch result.FailureClass {
	case "provider_failure", "usage_exhausted", "limit":
	default:
		return false
	}
	if result.Lifecycle != nil {
		for _, handlers := range result.Lifecycle.Handlers {
			for _, handler := range handlers {
				if handler.Status != RunStatusSucceeded && handler.Status != RunStatusSkipped {
					return false
				}
			}
		}
	}
	owner := nativeString(ec.Metadata, "origin_run_id")
	if owner == "" {
		owner = ec.RunID
	}
	reference := nativeString(result.Outputs, "agent")
	if reference == "" {
		return false
	}
	messages, err := e.Workers.Messages(ctx, owner, reference)
	if err != nil {
		return false
	}
	for _, message := range messages {
		ack := message.FailureAcknowledgement
		if message.TaskAttemptID == result.AttemptID && message.Status == RunStatusFailed && message.UnsafeFailureClass == "" && ack != nil && ack.MessageID == message.ID && ack.Generation == message.Generation && ack.RequestID != "" {
			return true
		}
	}
	return false
}
