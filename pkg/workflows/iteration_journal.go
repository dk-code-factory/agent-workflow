package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// IterationPlan is immutable. Fingerprint binds order, payloads, resolved call
// inputs, the admitted definition closure, roles, and action contracts.
type IterationPlan struct {
	RunID       string              `json:"run_id"`
	JobID       string              `json:"job_id"`
	Fingerprint string              `json:"fingerprint"`
	Items       []IterationPlanItem `json:"items"`
}
type IterationPlanItem struct {
	Key    string         `json:"key"`
	Number int            `json:"number"`
	Item   any            `json:"item"`
	Inputs map[string]any `json:"inputs"`
}

// IterationItemResult is an observation of one physical child. Terminal results
// are append-only; explicit resume appends a fresh attempt for incomplete items.
type IterationItemResult struct {
	Fingerprint     string                  `json:"fingerprint"`
	PlanFingerprint string                  `json:"plan_fingerprint"`
	Jobs            map[string]JobExecution `json:"jobs,omitempty"`
	Key             string                  `json:"key"`
	Number          int                     `json:"number"`
	Attempt         int                     `json:"attempt"`
	ChildID         string                  `json:"child_id,omitempty"`
	Status          string                  `json:"status"`
	Outputs         map[string]any          `json:"outputs,omitempty"`
	FailureClass    string                  `json:"failure_class,omitempty"`
	Error           string                  `json:"error,omitempty"`
}

// IterationJournal is required only for workflows containing for-each. Plan and
// attempt writes must fence the originating owner and commit before dispatch or
// advancing. Each attempt has one admission and one immutable result. A separately appended
// recovered observation may close an interrupted child after a lost terminal write.
type IterationJournal interface {
	Journal
	CheckIteration(context.Context) error
	BeginInput(context.Context, string, string, string, int, bool, string, bool) (Attempt, bool, error)
	PinIteration(context.Context, IterationPlan) error
	IterationPlan(context.Context, string, string) (IterationPlan, error)
	IterationAttempts(context.Context, string, string) ([]IterationItemResult, error)
	AppendIteration(context.Context, string, string, IterationItemResult) error
}

func iterationJournal(store RunStore) IterationJournal {
	if r, ok := store.(redactedStore); ok {
		return iterationJournal(r.Journal)
	}
	j, _ := store.(IterationJournal)
	return j
}
func (s *SQLiteStore) IterationPlan(ctx context.Context, run, job string) (p IterationPlan, err error) {
	var b []byte
	err = s.db.QueryRowContext(ctx, "SELECT record FROM iteration_plans WHERE run_id=? AND job_id=?", run, job).Scan(&b)
	if err == nil {
		err = decodeJSONWithNumbers(b, &p)
	}
	return
}
func (s *SQLiteStore) PinIteration(ctx context.Context, p IterationPlan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	var previous []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM iteration_plans WHERE run_id=? AND job_id=?", p.RunID, p.JobID).Scan(&previous)
	if err == nil {
		var old IterationPlan
		if err = decodeJSONWithNumbers(previous, &old); err != nil {
			return err
		}
		if actionHash(old) != actionHash(p) {
			return fmt.Errorf("%w: ordered iteration plan changed for %s", ErrActionIntegrity, p.JobID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if p.Fingerprint == "" {
		return fmt.Errorf("iteration fingerprint is required")
	}
	keys := map[string]bool{}
	for i, item := range p.Items {
		if item.Key == "" || keys[item.Key] || item.Number != i+1 {
			return fmt.Errorf("invalid ordered item identity")
		}
		keys[item.Key] = true
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO iteration_plans VALUES(?,?,?)", p.RunID, p.JobID, b); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, p.RunID, "iteration.pinned", p); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SQLiteStore) IterationAttempts(ctx context.Context, run, job string) ([]IterationItemResult, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM iteration_attempts WHERE run_id=? AND job_id=? ORDER BY rowid", run, job)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IterationItemResult{}
	for rows.Next() {
		var b []byte
		var r IterationItemResult
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &r); err != nil {
			return nil, err
		}
		if r.Fingerprint != iterationResultHash(r) {
			return nil, fmt.Errorf("%w: iteration result fingerprint mismatch", ErrActionIntegrity)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) AppendIteration(ctx context.Context, run, job string, r IterationItemResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	var b []byte
	var p IterationPlan
	if err = tx.QueryRowContext(ctx, "SELECT record FROM iteration_plans WHERE run_id=? AND job_id=?", run, job).Scan(&b); err != nil {
		return err
	}
	if err = decodeJSONWithNumbers(b, &p); err != nil {
		return err
	}
	if r.Fingerprint != iterationResultHash(r) || r.PlanFingerprint != p.Fingerprint {
		return fmt.Errorf("%w: iteration attempt fingerprint mismatch", ErrActionIntegrity)
	}
	if r.Number < 1 || r.Number > len(p.Items) || p.Items[r.Number-1].Key != r.Key || r.Attempt < 1 || r.ChildID != iterationChildID(run, job, r.Key, r.Attempt) {
		return fmt.Errorf("invalid iteration attempt identity")
	}
	phase := "result"
	if r.Status == "pending" {
		phase = "admitted"
	} else if r.Status != "completed" && r.Status != "exhausted" && r.Status != "provider_failure" && r.Status != "interrupted" {
		return fmt.Errorf("invalid item outcome %q", r.Status)
	}
	if phase == "admitted" {
		var count, last int
		if err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(MAX(number),0) FROM iteration_attempts WHERE run_id=? AND job_id=? AND item_key=? AND phase='admitted'", run, job, r.Key).Scan(&count, &last); err != nil {
			return err
		}
		if last > 0 {
			var previous []byte
			if err = tx.QueryRowContext(ctx, "SELECT record FROM iteration_attempts WHERE run_id=? AND job_id=? AND item_key=? AND number=? ORDER BY rowid DESC LIMIT 1", run, job, r.Key, last).Scan(&previous); err != nil {
				return err
			}
			var saved IterationItemResult
			if err = decodeJSONWithNumbers(previous, &saved); err != nil {
				return err
			}
			if saved.Status == "completed" {
				return fmt.Errorf("completed iteration item is immutable")
			}
		}
		if r.Attempt != last+1 || count != last {
			return fmt.Errorf("iteration attempt sequence changed")
		}
	} else {
		var admitted IterationItemResult
		if err = tx.QueryRowContext(ctx, "SELECT record FROM iteration_attempts WHERE run_id=? AND job_id=? AND item_key=? AND number=? AND phase='admitted'", run, job, r.Key, r.Attempt).Scan(&b); err != nil {
			return err
		}
		if err = decodeJSONWithNumbers(b, &admitted); err != nil {
			return err
		}
		if admitted.ChildID != r.ChildID {
			return fmt.Errorf("iteration child changed")
		}
	}
	if phase == "result" && r.Status == "completed" {
		var previous []byte
		queryErr := tx.QueryRowContext(ctx, "SELECT record FROM iteration_attempts WHERE run_id=? AND job_id=? AND item_key=? AND number=? AND phase='result'", run, job, r.Key, r.Attempt).Scan(&previous)
		if queryErr == nil {
			var saved IterationItemResult
			if err = decodeJSONWithNumbers(previous, &saved); err != nil {
				return err
			}
			if saved.Status != "interrupted" {
				return fmt.Errorf("terminal iteration result is immutable")
			}
			phase = "recovered"
		} else if !errors.Is(queryErr, sql.ErrNoRows) {
			return queryErr
		}
	}
	b, err = json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO iteration_attempts VALUES(?,?,?,?,?,?)", run, job, r.Key, r.Attempt, phase, b); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, run, "iteration."+r.Status, r); err != nil {
		return err
	}
	return tx.Commit()
}

func iterationResultHash(r IterationItemResult) string { r.Fingerprint = ""; return actionHash(r) }

func (s *SQLiteStore) CheckIteration(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return checkFence(ctx, tx)
}
