package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const skillChildServer = "native-skill-child"

type SkillChildPermit struct {
	InvocationID string `json:"invocation_id"`
	CallID       string `json:"call_id"`
	Fingerprint  string `json:"fingerprint"`
}

// Authorization is deliberately not replayable. Even an identical call after
// a lost permit acknowledgement might already have dispatched its native child.
func (s *SQLiteStore) authorizeSkillChild(ctx context.Context, workerID, messageID string, observation SkillChildObservation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	var raw []byte
	var worker Worker
	if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=? AND status='running'", workerID).Scan(&raw); err != nil {
		return err
	}
	if err = decodeJSONWithNumbers(raw, &worker); err != nil {
		return err
	}
	if worker.Skill == nil || len(worker.Skill.Children) != 1 || !worker.SkillTurnUncertain || worker.SkillActiveMessageID != messageID || worker.SessionID != observation.SessionID || observation.Kind != "permit" {
		return fmt.Errorf("skill child permit is outside original active invocation/session")
	}
	if err = worker.SkillEvidence.InvocationComplete(worker.Skill); err != nil {
		return err
	}
	if len(worker.SkillChildPermits) != 0 {
		return fmt.Errorf("original skill child allowance is already consumed; reconcile original dispatch")
	}
	if err = worker.SkillEvidence.ObserveChild(worker.Skill, observation); err != nil {
		return err
	}
	if err = checkEffectAdmission(ctx, tx, worker.RunID); err != nil {
		return err
	}
	var generation int64
	if err = tx.QueryRowContext(ctx, `SELECT r.generation FROM runs r JOIN executions e ON e.run_id=r.id AND e.generation=r.generation WHERE r.id=? AND r.status='running' AND e.finalizing=0`, worker.RunID).Scan(&generation); err != nil {
		return err
	}
	var unresolved int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tool_calls WHERE run_id=? AND status IN ('dispatched','uncertain')", worker.RunID).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved != 0 {
		return ErrUncertainTool
	}
	fingerprint := actionHash([]any{worker.Skill.ID, worker.ID, messageID, observation})
	permit := SkillChildPermit{InvocationID: messageID, CallID: observation.ParentCallID, Fingerprint: fingerprint}
	worker.SkillChildPermits = append(worker.SkillChildPermits, permit)
	record := ToolCallRecord{RunID: worker.RunID, Generation: generation, InvocationID: messageID, CallID: permit.CallID, Server: skillChildServer, Tool: worker.Skill.Children[0].Type, ArgumentFingerprint: fingerprint, AuditArguments: observation, Status: "dispatched", AdmittedAt: time.Now().UTC()}
	raw, err = json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO tool_calls VALUES(?,?,?,?,?,?,?,?,?)", worker.RunID, generation, messageID, permit.CallID, record.Server, record.Tool, fingerprint, record.Status, raw); err != nil {
		return err
	}
	raw, err = json.Marshal(worker)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET record=? WHERE id=?", raw, workerID); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, worker.RunID, "worker.skill_child_permitted", map[string]any{"worker_id": workerID, "invocation_id": messageID, "call_id": permit.CallID, "fingerprint": fingerprint}); err != nil {
		return err
	}
	return tx.Commit()
}

func completeSkillChildTx(ctx context.Context, tx *sql.Tx, worker Worker, messageID string) error {
	if worker.Skill == nil || len(worker.Skill.Children) == 0 {
		return nil
	}
	if err := worker.SkillEvidence.Complete(worker.Skill); err != nil {
		return err
	}
	if len(worker.SkillChildPermits) != 1 {
		return fmt.Errorf("original native child durable permit is missing")
	}
	permit := worker.SkillChildPermits[0]
	// A subsequent completed continuation retains the original child row. It
	// must not manufacture another permit or another original completion.
	if permit.InvocationID != messageID {
		return nil
	}
	var raw []byte
	var usage UsageObservation
	if err := tx.QueryRowContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'", messageID).Scan(&raw); err != nil {
		return err
	}
	if err := decodeJSONWithNumbers(raw, &usage); err != nil {
		return err
	}
	if usage.CounterBasis != ClaudeChildUsageBasis || !usage.Complete || usage.CostIncomplete || usage.CounterReset {
		return fmt.Errorf("original native child accounting remains incomplete")
	}
	var record ToolCallRecord
	if err := tx.QueryRowContext(ctx, "SELECT record FROM tool_calls WHERE run_id=? AND invocation_id=? AND call_id=?", worker.RunID, messageID, permit.CallID).Scan(&raw); err != nil {
		return err
	}
	if err := decodeJSONWithNumbers(raw, &record); err != nil {
		return err
	}
	if record.Server != skillChildServer || record.ArgumentFingerprint != permit.Fingerprint || record.Tool != worker.Skill.Children[0].Type || record.Status != "dispatched" && record.Status != "uncertain" && record.Status != "completed" {
		return fmt.Errorf("original native child dispatch identity changed")
	}
	record.Status = "completed"
	record.Error, record.FailureClass = "", ""
	if record.FinishedAt.IsZero() {
		record.FinishedAt = time.Now().UTC()
	}
	record.Result, _ = json.Marshal(worker.SkillEvidence.Children)
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE tool_calls SET status='completed',record=? WHERE run_id=? AND invocation_id=? AND call_id=? AND fingerprint=?", raw, worker.RunID, messageID, permit.CallID, permit.Fingerprint)
	return err
}

// Reconcile only a lost local completion write, using the original full native
// terminal and child outputs. A failed turn or later unknown continuation is
// never retried or cleared using an older child's successful receipt.
func (s *SQLiteStore) reconcileSkillChildCompletion(ctx context.Context, workerID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var worker Worker
	if err = tx.QueryRowContext(ctx, "SELECT record FROM workers WHERE id=?", workerID).Scan(&raw); err != nil {
		return err
	}
	if err = decodeJSONWithNumbers(raw, &worker); err != nil {
		return err
	}
	if worker.Skill == nil || !worker.SkillTurnUncertain || len(worker.Skill.Children) != 1 || len(worker.SkillChildPermits) != 1 || worker.SkillActiveMessageID != worker.SkillChildPermits[0].InvocationID {
		return nil
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE id=? AND status='running'", worker.RunID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return nil
	}
	var message WorkerMessage
	if err = tx.QueryRowContext(ctx, "SELECT record FROM messages WHERE id=? AND worker_id=?", worker.SkillActiveMessageID, worker.ID).Scan(&raw); err != nil {
		return err
	}
	if err = decodeJSONWithNumbers(raw, &message); err != nil {
		return err
	}
	if message.Status != RunStatusRunning {
		return nil
	}
	var terminal map[string]any
	for _, observation := range worker.SkillEvidence.Children {
		if observation.Kind == "parent_terminal" {
			terminal = childRaw(observation)
		}
	}
	if terminal == nil {
		return nil
	}
	update, accountingErr := SkillChildUsage(terminal)
	if len(worker.SkillEvidence.UncertainAttempts) != 0 {
		update.Incomplete = true
	}
	identity := UsageIdentity{ID: message.ID, RunID: worker.RunID, WorkerID: worker.ID, Kind: "worker", TaskAttemptID: message.TaskAttemptID}
	if err = observeSkillUsageTx(ctx, tx, identity, update); err != nil {
		if errors.Is(err, ErrUsageCounterConflict) {
			return errors.Join(err, tx.Commit())
		}
		return err
	}
	if accountingErr != nil || worker.SkillEvidence.Complete(worker.Skill) != nil {
		// Retain known charges even when original child completion cannot be
		// established. The continuation guard still rejects this worker.
		return tx.Commit()
	}
	if err = completeSkillChildTx(ctx, tx, worker, message.ID); err != nil {
		return err
	}
	outputs := map[string]any{"text": terminal["result"], "response": terminal["result"], "native_child_result": terminal, "native_parent_usage": terminal["usage"], "native_model_usage": terminal["modelUsage"], "usage": update.Usage, "usage_counter_basis": update.CounterBasis, "cost_usd": update.CostUSD, "provider": update.Provider, "model": update.Model, "session_id": worker.SessionID, "provider_version": "claude-agent-sdk@0.3.322", "provider_status": "success"}
	if structured, ok := terminal["structured_output"]; ok {
		outputs["structured"] = structured
		data, _ := json.Marshal(structured)
		outputs["text"], outputs["response"] = string(data), string(data)
	}
	message.Result = SessionResult{SessionID: worker.SessionID, Status: RunStatusSucceeded, ProviderStatus: "success", ProviderVersion: "claude-agent-sdk@0.3.322", Outputs: outputs}
	encoded, _ := json.Marshal(message.Result)
	if worker.MaxOutput > 0 && len(encoded) > worker.MaxOutput {
		return fmt.Errorf("original native child recovered output exceeds retained limit")
	}
	message.Status, message.Error, message.FailureClass, message.UnsafeFailureClass = RunStatusSucceeded, "", "", ""
	message.FinishedAt = time.Now().UTC()
	worker.SkillTurnUncertain = false
	worker.SkillActiveMessageID = ""
	worker.Error, worker.FailureClass = "", ""
	worker.FailureAcknowledgement = nil
	if worker.Status != "closed" {
		worker.Status = "idle"
	}
	raw, _ = json.Marshal(worker)
	if _, err = tx.ExecContext(ctx, "UPDATE workers SET status=?,record=? WHERE id=?", worker.Status, raw, worker.ID); err != nil {
		return err
	}
	raw, _ = json.Marshal(message)
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET status=?,record=? WHERE id=?", message.Status, raw, message.ID); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, worker.RunID, "worker.skill_child_completion_reconciled", map[string]string{"worker_id": worker.ID, "message_id": message.ID, "call_id": worker.SkillChildPermits[0].CallID}); err != nil {
		return err
	}
	return tx.Commit()
}
