package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// TaskStateRequest addresses one logical operation in the admitted task group.
// Proofs and evidence identities are application assertions: the controller does
// not infer whether a domain-specific mutation happened or evidence is current.
type TaskStateRequest struct {
	Operation           string          `json:"operation"`
	Name                string          `json:"name,omitempty"`
	Task                string          `json:"task,omitempty"`
	Fingerprint         string          `json:"fingerprint,omitempty"`
	OperationID         string          `json:"operation_id,omitempty"`
	RequestID           string          `json:"request_id,omitempty"`
	EvidenceFingerprint string          `json:"evidence_fingerprint,omitempty"`
	ResultReference     string          `json:"result_reference,omitempty"`
	Outcome             string          `json:"outcome,omitempty"`
	Proof               json.RawMessage `json:"proof,omitempty"`
	Limit               int64           `json:"limit,omitempty"`
	Amount              int64           `json:"amount,omitempty"`
	Deadline            time.Time       `json:"deadline,omitempty"`
	Instruction         string          `json:"instruction,omitempty"`
	Source              string          `json:"source,omitempty"`
}

type TaskClaim struct {
	Task                string          `json:"task"`
	Fingerprint         string          `json:"fingerprint"`
	OperationID         string          `json:"operation_id"`
	State               string          `json:"state"`
	ResultReference     string          `json:"result_reference,omitempty"`
	EvidenceFingerprint string          `json:"evidence_fingerprint,omitempty"`
	OwnerRun            string          `json:"owner_run"`
	OwnerGeneration     int64           `json:"owner_generation"`
	Owner               string          `json:"-"`
	Proof               json.RawMessage `json:"proof,omitempty"`
}

type TaskBudget struct {
	Name  string `json:"name"`
	Limit int64  `json:"limit"`
	Used  int64  `json:"used"`
}

type TaskDeadline struct {
	Name             string    `json:"name"`
	OriginalDeadline time.Time `json:"original_deadline"`
	Deadline         time.Time `json:"deadline"`
	Extensions       int64     `json:"extensions"`
}

type TaskStateResponse struct {
	Claim    *TaskClaim    `json:"claim,omitempty"`
	Budget   *TaskBudget   `json:"budget,omitempty"`
	Deadline *TaskDeadline `json:"deadline,omitempty"`
	Dispatch bool          `json:"dispatch"`
	Reusable bool          `json:"reusable"`
	Closed   bool          `json:"closed,omitempty"`
}

type TaskStateInspection struct {
	Group     string             `json:"group"`
	Closed    bool               `json:"closed"`
	Claims    []TaskClaim        `json:"claims"`
	Budgets   []TaskBudget       `json:"budgets"`
	Deadlines []TaskDeadline     `json:"deadlines"`
	Requests  []TaskStateRequest `json:"requests"`
}

func ValidateTaskGroup(group string) error {
	if group == "" || len(group) > 128 {
		return fmt.Errorf("task group must contain 1–128 ASCII letters, digits, '.', '_' or '-'")
	}
	for _, c := range group {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return fmt.Errorf("invalid task group %q", group)
		}
	}
	return nil
}

func TaskStateReadOnly(operation string) bool {
	return operation == "get_claim" || operation == "get_budget" || operation == "get_deadline"
}

func taskIdentity(value, field string) error {
	if strings.TrimSpace(value) == "" || len(value) > 512 || strings.ContainsRune(value, 0) {
		return fmt.Errorf("task-state %s requires 1–512 non-NUL bytes", field)
	}
	return nil
}

func (s *SQLiteStore) linkTaskGroup(ctx context.Context, tx *sql.Tx, run *Run, snap Snapshot) error {
	raw, exists := snap.Metadata["task_group"]
	if !exists {
		return nil
	}
	group, ok := raw.(string)
	if !ok {
		return fmt.Errorf("task_group must be a string")
	}
	if err := ValidateTaskGroup(group); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO task_groups(id,closed) VALUES(?,0)", group); err != nil {
		return err
	}
	var closed bool
	if err := tx.QueryRowContext(ctx, "SELECT closed FROM task_groups WHERE id=?", group).Scan(&closed); err != nil {
		return err
	}
	if closed {
		return fmt.Errorf("task group %q is permanently closed", group)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO task_group_members(group_id,run_id) VALUES(?,?)", group, run.ID)
	return err
}

func (s *SQLiteStore) taskGroupTx(ctx context.Context, tx *sql.Tx) (string, executionFence, error) {
	f, ok := ctx.Value(fenceKey{}).(executionFence)
	if !ok || f.owner == "" {
		return "", f, fmt.Errorf("task state requires current execution ownership")
	}
	if err := checkFence(ctx, tx); err != nil {
		return "", f, err
	}
	var group string
	var finalizing bool
	err := tx.QueryRowContext(ctx, `SELECT m.group_id,e.finalizing FROM task_group_members m JOIN executions e ON e.run_id=m.run_id WHERE m.run_id=? AND e.generation=?`, f.id, f.generation).Scan(&group, &finalizing)
	if err != nil {
		return "", f, fmt.Errorf("execution has no admitted task group: %w", err)
	}
	if finalizing {
		return "", f, fmt.Errorf("task state owner is finalizing")
	}
	return group, f, nil
}

// TaskState commits logical task state before acknowledgement. A replayed claim
// never grants dispatch again; an unacknowledged claim needs reconciliation.
func (s *SQLiteStore) TaskState(ctx context.Context, req TaskStateRequest) (TaskStateResponse, error) {
	var out TaskStateResponse
	raw, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	if len(raw) > 64<<10 {
		return out, fmt.Errorf("task-state request exceeds 64 KiB")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	group, f, err := s.taskGroupTx(ctx, tx)
	if err != nil {
		return out, err
	}
	if !TaskStateReadOnly(req.Operation) {
		if err = checkEffectAdmission(ctx, tx, f.id); err != nil {
			return out, err
		}
		var closed bool
		if err = tx.QueryRowContext(ctx, "SELECT closed FROM task_groups WHERE id=?", group).Scan(&closed); err != nil {
			return out, err
		}
		if closed {
			if req.Operation == "close_group" {
				replay, e := taskRequest(ctx, tx, group, "close", req.RequestID, req)
				if e != nil {
					return out, e
				}
				if replay {
					out.Closed = true
					return out, tx.Commit()
				}
			}
			return out, fmt.Errorf("task group is permanently closed")
		}
	}
	switch req.Operation {
	case "claim", "get_claim", "complete", "reconcile":
		out, err = taskClaim(ctx, tx, group, f, req)
	case "budget", "reserve", "get_budget", "extend_budget":
		out, err = taskBudget(ctx, tx, group, req)
	case "deadline", "get_deadline", "extend_deadline":
		out, err = taskDeadline(ctx, tx, group, req)
	case "close_group":
		if err = taskContinuation(req); err != nil {
			break
		}
		var active int
		err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM task_claims WHERE group_id=? AND state IN ('claimed','unknown'))+(SELECT count(*) FROM task_group_members m JOIN runs r ON r.id=m.run_id WHERE m.group_id=? AND r.id!=? AND r.status IN ('running','queued','needs_attention','waiting','interrupted'))`, group, group, f.id).Scan(&active)
		if err != nil {
			break
		}
		if active != 0 {
			err = fmt.Errorf("task group still has unresolved claims or active members")
			break
		}
		_, err = taskRequest(ctx, tx, group, "close", req.RequestID, req)
		if err != nil {
			break
		}
		_, err = tx.ExecContext(ctx, "UPDATE task_groups SET closed=1 WHERE id=?", group)
		out.Closed = err == nil
	default:
		err = fmt.Errorf("unknown task-state operation %q", req.Operation)
	}
	if err != nil {
		return out, err
	}
	if !TaskStateReadOnly(req.Operation) {
		if err = addEvent(ctx, tx, f.id, "task-state."+req.Operation, map[string]string{"group": group}); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

func loadTaskClaim(ctx context.Context, tx *sql.Tx, group string, req TaskStateRequest) (*TaskClaim, error) {
	var c TaskClaim
	var proof []byte
	query := `SELECT task,fingerprint,operation_id,state,result_reference,evidence_fingerprint,owner_run,owner_generation,owner,proof FROM task_claims WHERE group_id=? AND operation_id=?`
	args := []any{group, req.OperationID}
	if req.OperationID == "" {
		query = `SELECT task,fingerprint,operation_id,state,result_reference,evidence_fingerprint,owner_run,owner_generation,owner,proof FROM task_claims WHERE group_id=? AND task=? AND fingerprint=? ORDER BY rowid DESC LIMIT 1`
		args = []any{group, req.Task, req.Fingerprint}
	}
	err := tx.QueryRowContext(ctx, query, args...).Scan(&c.Task, &c.Fingerprint, &c.OperationID, &c.State, &c.ResultReference, &c.EvidenceFingerprint, &c.OwnerRun, &c.OwnerGeneration, &c.Owner, &proof)
	c.Proof = proof
	return &c, err
}

func taskClaim(ctx context.Context, tx *sql.Tx, group string, f executionFence, req TaskStateRequest) (TaskStateResponse, error) {
	var out TaskStateResponse
	for field, value := range map[string]string{"task": req.Task, "fingerprint": req.Fingerprint} {
		if err := taskIdentity(value, field); err != nil {
			return out, err
		}
	}
	if req.Operation != "get_claim" {
		if err := taskIdentity(req.OperationID, "operation_id"); err != nil {
			return out, err
		}
	}
	c, err := loadTaskClaim(ctx, tx, group, req)
	if err == nil && (c.Task != req.Task || c.Fingerprint != req.Fingerprint) {
		return out, fmt.Errorf("task claim identity conflict")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if req.Operation == "claim" && errors.Is(err, sql.ErrNoRows) {
		existing, e := loadTaskClaim(ctx, tx, group, TaskStateRequest{Task: req.Task, Fingerprint: req.Fingerprint})
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
		if e == nil && existing.State != "no_effect" {
			c, err = existing, nil
		} else {
			c = &TaskClaim{Task: req.Task, Fingerprint: req.Fingerprint, OperationID: req.OperationID, State: "claimed", OwnerRun: f.id, OwnerGeneration: f.generation, Owner: f.owner}
			_, err = tx.ExecContext(ctx, `INSERT INTO task_claims(group_id,task,fingerprint,operation_id,state,result_reference,evidence_fingerprint,owner_run,owner_generation,owner,proof) VALUES(?,?,?,?,'claimed','','',?,?,?,NULL)`, group, c.Task, c.Fingerprint, c.OperationID, f.id, f.generation, f.owner)
			if err == nil {
				out.Dispatch = true
			}
		}
	}
	if err != nil {
		return out, err
	}
	if req.Operation == "complete" {
		if c.OwnerRun != f.id || c.OwnerGeneration != f.generation || c.Owner != f.owner {
			return out, fmt.Errorf("claim belongs to a different execution; reconcile before recovery")
		}
		if err = taskIdentity(req.ResultReference, "result_reference"); err != nil {
			return out, err
		}
		if err = taskIdentity(req.EvidenceFingerprint, "evidence_fingerprint"); err != nil {
			return out, err
		}
		if c.State == "completed" {
			if c.ResultReference != req.ResultReference || c.EvidenceFingerprint != req.EvidenceFingerprint {
				return out, fmt.Errorf("completed claim evidence conflict")
			}
		} else {
			if c.State != "claimed" {
				return out, fmt.Errorf("claim requires reconciliation before completion")
			}
			c.State, c.ResultReference, c.EvidenceFingerprint = "completed", req.ResultReference, req.EvidenceFingerprint
			_, err = tx.ExecContext(ctx, "UPDATE task_claims SET state='completed',result_reference=?,evidence_fingerprint=? WHERE group_id=? AND operation_id=?", c.ResultReference, c.EvidenceFingerprint, group, c.OperationID)
		}
	}
	if req.Operation == "reconcile" {
		if err = taskIdentity(req.RequestID, "request_id"); err != nil {
			return out, err
		}
		if req.Outcome != "unknown" && req.Outcome != "no_effect" && req.Outcome != "completed" {
			return out, fmt.Errorf("reconciliation outcome must be unknown, no_effect or completed")
		}
		if req.Outcome != "unknown" && (len(req.Proof) == 0 || string(req.Proof) == "null" || !json.Valid(req.Proof)) {
			return out, fmt.Errorf("authoritative reconciliation proof is required")
		}
		if req.Outcome == "completed" {
			if err = taskIdentity(req.ResultReference, "result_reference"); err != nil {
				return out, err
			}
			if err = taskIdentity(req.EvidenceFingerprint, "evidence_fingerprint"); err != nil {
				return out, err
			}
		}
		replay, e := taskRequest(ctx, tx, group, "reconcile", req.RequestID, req)
		if e != nil {
			return out, e
		}
		if !replay {
			if c.State == "completed" || c.State == "no_effect" {
				return out, fmt.Errorf("resolved claim cannot be reconciled again")
			}
			var live int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM runs WHERE id=? AND generation=? AND owner=? AND status='running'", c.OwnerRun, c.OwnerGeneration, c.Owner).Scan(&live); err != nil {
				return out, err
			}
			if live > 0 && (c.OwnerRun != f.id || c.OwnerGeneration != f.generation || c.Owner != f.owner) {
				return out, fmt.Errorf("claim's original execution is still active")
			}
			c.State, c.Proof = req.Outcome, req.Proof
			if req.Outcome == "completed" {
				c.ResultReference, c.EvidenceFingerprint = req.ResultReference, req.EvidenceFingerprint
			}
			_, err = tx.ExecContext(ctx, "UPDATE task_claims SET state=?,proof=?,result_reference=?,evidence_fingerprint=? WHERE group_id=? AND operation_id=?", c.State, []byte(c.Proof), c.ResultReference, c.EvidenceFingerprint, group, c.OperationID)
		}
	}
	out.Claim = c
	out.Reusable = c.State == "completed" && req.EvidenceFingerprint != "" && req.EvidenceFingerprint == c.EvidenceFingerprint
	return out, err
}

// taskRequest retains payloads, including continuation provenance. Replay with a
// changed payload is rejected even when the requested amount would still fit.
func taskRequest(ctx context.Context, tx *sql.Tx, group, kind, id string, req TaskStateRequest) (bool, error) {
	if err := taskIdentity(id, "request identity"); err != nil {
		return false, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM task_state_requests WHERE group_id=? AND kind=? AND id=?", group, kind, id).Scan(&previous)
	if err == nil {
		if string(raw) != string(previous) {
			return false, fmt.Errorf("task-state request identity conflict")
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO task_state_requests VALUES(?,?,?,?)", group, kind, id, raw)
	return false, err
}

func taskContinuation(req TaskStateRequest) error {
	for field, value := range map[string]string{"request_id": req.RequestID, "instruction": req.Instruction, "source": req.Source} {
		if err := taskIdentity(value, field); err != nil {
			return err
		}
	}
	return nil
}

func taskBudget(ctx context.Context, tx *sql.Tx, group string, req TaskStateRequest) (TaskStateResponse, error) {
	var out TaskStateResponse
	if err := taskIdentity(req.Name, "name"); err != nil {
		return out, err
	}
	var b TaskBudget
	b.Name = req.Name
	err := tx.QueryRowContext(ctx, "SELECT allowance,used FROM task_budgets WHERE group_id=? AND name=?", group, req.Name).Scan(&b.Limit, &b.Used)
	if req.Operation == "budget" {
		if req.Limit < 0 {
			return out, fmt.Errorf("initial budget limit must be nonnegative")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			b.Limit = req.Limit
			_, err = tx.ExecContext(ctx, "INSERT INTO task_budgets VALUES(?,?,?,?,?)", group, req.Name, req.Limit, req.Limit, 0)
		} else {
			var initial int64
			if err = tx.QueryRowContext(ctx, "SELECT initial_allowance FROM task_budgets WHERE group_id=? AND name=?", group, req.Name).Scan(&initial); err != nil {
				return out, err
			}
			if initial != req.Limit {
				return out, fmt.Errorf("initial budget limit changed")
			}
		}
	}
	if req.Operation == "reserve" {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		if req.Limit < 0 || req.Amount <= 0 {
			return out, fmt.Errorf("reservation needs nonnegative initial limit and positive amount")
		}
		replay, e := taskRequest(ctx, tx, group, "reservation", req.OperationID, req)
		if e != nil {
			return out, e
		}
		if errors.Is(err, sql.ErrNoRows) {
			b.Limit = req.Limit
			_, err = tx.ExecContext(ctx, "INSERT INTO task_budgets VALUES(?,?,?,?,?)", group, req.Name, req.Limit, req.Limit, 0)
			if err != nil {
				return out, err
			}
		} else {
			var initial int64
			if err = tx.QueryRowContext(ctx, "SELECT initial_allowance FROM task_budgets WHERE group_id=? AND name=?", group, req.Name).Scan(&initial); err != nil {
				return out, err
			}
			if req.Limit != initial {
				return out, fmt.Errorf("initial budget limit changed")
			}
		}
		if !replay {
			if req.Amount > b.Limit-b.Used {
				return out, fmt.Errorf("task budget exhausted")
			}
			b.Used += req.Amount
			_, err = tx.ExecContext(ctx, "UPDATE task_budgets SET used=? WHERE group_id=? AND name=?", b.Used, group, req.Name)
		}
	} else if req.Operation == "extend_budget" {
		if err != nil {
			return out, err
		}
		if err = taskContinuation(req); err != nil {
			return out, err
		}
		if req.Amount <= 0 {
			return out, fmt.Errorf("budget extension must be positive")
		}
		replay, e := taskRequest(ctx, tx, group, "continuation", req.RequestID, req)
		if e != nil {
			return out, e
		}
		if !replay {
			if req.Amount > math.MaxInt64-b.Limit {
				return out, fmt.Errorf("budget extension overflows")
			}
			b.Limit += req.Amount
			_, err = tx.ExecContext(ctx, "UPDATE task_budgets SET allowance=? WHERE group_id=? AND name=?", b.Limit, group, req.Name)
		}
	}
	out.Budget = &b
	return out, err
}

func taskDeadline(ctx context.Context, tx *sql.Tx, group string, req TaskStateRequest) (TaskStateResponse, error) {
	var out TaskStateResponse
	if err := taskIdentity(req.Name, "name"); err != nil {
		return out, err
	}
	var d TaskDeadline
	d.Name = req.Name
	var initial, current string
	err := tx.QueryRowContext(ctx, "SELECT original_deadline,deadline,extensions FROM task_deadlines WHERE group_id=? AND name=?", group, req.Name).Scan(&initial, &current, &d.Extensions)
	if req.Operation == "deadline" {
		if req.Deadline.IsZero() || req.Deadline.Year() < 1 || req.Deadline.Year() > 9999 {
			return out, fmt.Errorf("absolute deadline is required")
		}
		replay, e := taskRequest(ctx, tx, group, "deadline", req.OperationID, req)
		if e != nil {
			return out, e
		}
		if errors.Is(err, sql.ErrNoRows) {
			initial = req.Deadline.UTC().Format(time.RFC3339Nano)
			current = initial
			_, err = tx.ExecContext(ctx, "INSERT INTO task_deadlines VALUES(?,?,?,?,0)", group, req.Name, initial, current)
		} else if err == nil && !replay {
			return out, fmt.Errorf("deadline already exists; use get_deadline")
		}
	}
	if err != nil {
		return out, err
	}
	d.OriginalDeadline, err = time.Parse(time.RFC3339Nano, initial)
	if err != nil {
		return out, err
	}
	d.Deadline, err = time.Parse(time.RFC3339Nano, current)
	if err != nil {
		return out, err
	}
	if req.Operation == "extend_deadline" {
		if err = taskContinuation(req); err != nil {
			return out, err
		}
		if req.Amount <= 0 || req.Amount > math.MaxInt64/int64(time.Second) {
			return out, fmt.Errorf("deadline extension amount must be positive representable seconds")
		}
		replay, e := taskRequest(ctx, tx, group, "continuation", req.RequestID, req)
		if e != nil {
			return out, e
		}
		if !replay {
			if d.Extensions == math.MaxInt64 {
				return out, fmt.Errorf("too many deadline extensions")
			}
			next := d.Deadline.Add(time.Duration(req.Amount) * time.Second)
			if next.Year() > 9999 {
				return out, fmt.Errorf("deadline extension overflows")
			}
			d.Deadline = next
			d.Extensions++
			_, err = tx.ExecContext(ctx, "UPDATE task_deadlines SET deadline=?,extensions=? WHERE group_id=? AND name=?", d.Deadline.UTC().Format(time.RFC3339Nano), d.Extensions, group, req.Name)
		}
	}
	out.Deadline = &d
	return out, err
}

// InspectTaskState observes the full group ledger without an execution lease or
// writes. It remains available after interruption and includes provenance.
func (s *SQLiteStore) InspectTaskState(ctx context.Context, run string) (TaskStateInspection, error) {
	var out TaskStateInspection
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT g.id,g.closed FROM task_group_members m JOIN task_groups g ON g.id=m.group_id WHERE m.run_id=?", run).Scan(&out.Group, &out.Closed)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT task,fingerprint,operation_id,state,result_reference,evidence_fingerprint,owner_run,owner_generation,proof FROM task_claims WHERE group_id=? ORDER BY rowid`, out.Group)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c TaskClaim
		var proof []byte
		if err = rows.Scan(&c.Task, &c.Fingerprint, &c.OperationID, &c.State, &c.ResultReference, &c.EvidenceFingerprint, &c.OwnerRun, &c.OwnerGeneration, &proof); err != nil {
			break
		}
		c.Proof = proof
		out.Claims = append(out.Claims, c)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT name,allowance,used FROM task_budgets WHERE group_id=? ORDER BY name", out.Group)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var b TaskBudget
		if err = rows.Scan(&b.Name, &b.Limit, &b.Used); err != nil {
			break
		}
		out.Budgets = append(out.Budgets, b)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT name,original_deadline,deadline,extensions FROM task_deadlines WHERE group_id=? ORDER BY name", out.Group)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var d TaskDeadline
		var original, current string
		if err = rows.Scan(&d.Name, &original, &current, &d.Extensions); err != nil {
			break
		}
		d.OriginalDeadline, err = time.Parse(time.RFC3339Nano, original)
		if err != nil {
			break
		}
		d.Deadline, err = time.Parse(time.RFC3339Nano, current)
		if err != nil {
			break
		}
		out.Deadlines = append(out.Deadlines, d)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT record FROM task_state_requests WHERE group_id=? ORDER BY kind,id", out.Group)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		var req TaskStateRequest
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &req); err != nil {
			break
		}
		out.Requests = append(out.Requests, req)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}
