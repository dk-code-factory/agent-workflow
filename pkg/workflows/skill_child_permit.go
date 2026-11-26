package workflows

import (
	"context"
	"encoding/json"
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
