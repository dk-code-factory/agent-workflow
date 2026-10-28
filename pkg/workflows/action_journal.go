package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ActionAttempt retains admission and every reconciliation observation. A lost
// completion leaves State admitted; neither a handler error nor retry-uncertain
// proves that an external mutation had no effect.
type ActionAttempt struct {
	Attempt             Attempt                     `json:"attempt"`
	Context             ActionContext               `json:"context"`
	Contract            ActionContract              `json:"contract"`
	Input               json.RawMessage             `json:"input"`
	Bindings            json.RawMessage             `json:"bindings"`
	InputFingerprint    string                      `json:"input_fingerprint"`
	BindingsFingerprint string                      `json:"bindings_fingerprint"`
	State               string                      `json:"state"`
	Result              ActionValue                 `json:"result"`
	Error               string                      `json:"error,omitempty"`
	Reconciliations     []ActionRecoveryObservation `json:"reconciliations,omitempty"`
	Revision            int                         `json:"revision"`
}

type ActionRecoveryObservation struct {
	Generation int64           `json:"generation"`
	Time       time.Time       `json:"time"`
	Outcome    string          `json:"outcome"`
	Proof      json.RawMessage `json:"proof,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// ActionJournal is an optional Journal extension. Admission and completion must
// atomically update both the action record and its linked step attempt, fence
// the originating execution owner, and retain reconciliation history.
type ActionJournal interface {
	Journal
	ActionAttempts(context.Context, string) ([]ActionAttempt, error)
	AdmitAction(context.Context, ActionAttempt) error
	CheckpointAction(context.Context, ActionAttempt) error
}

func actionJournal(store RunStore) ActionJournal {
	if r, ok := store.(redactedStore); ok {
		return actionJournal(r.Journal)
	}
	j, _ := store.(ActionJournal)
	return j
}

func (s *SQLiteStore) ActionAttempts(ctx context.Context, run string) ([]ActionAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.record FROM action_attempts a JOIN attempts t ON t.id=a.attempt_id JOIN runs r ON r.id=t.run_id WHERE r.id=? OR r.origin_id=? ORDER BY a.rowid`, run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionAttempt
	for rows.Next() {
		var b []byte
		var a ActionAttempt
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) AdmitAction(ctx context.Context, a ActionAttempt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	if err = checkEffectAdmission(ctx, tx, a.Attempt.RunID); err != nil {
		return err
	}
	b, err := json.Marshal(a.Attempt)
	if err != nil {
		return err
	}
	t := a.Attempt
	if a.State != "admitted" || a.Revision != 1 || t.ID != a.Context.AttemptID {
		return fmt.Errorf("invalid action admission")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)", t.ID, t.RunID, t.JobID, t.StepID, t.Iteration, t.Number, t.Status, b); err != nil {
		return err
	}
	b, err = json.Marshal(a)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO action_attempts(attempt_id,revision,record) VALUES(?,?,?)", t.ID, a.Revision, b); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, t.RunID, "action.admitted", a); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) CheckpointAction(ctx context.Context, a ActionAttempt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE action_attempts SET record=?,revision=? WHERE attempt_id=? AND revision=?", b, a.Revision, a.Attempt.ID, a.Revision-1)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("action attempt %s checkpoint changed", a.Attempt.ID)
	}
	b, err = json.Marshal(a.Attempt)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE attempts SET record=?,status=? WHERE id=?", b, a.Attempt.Status, a.Attempt.ID); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, a.Attempt.RunID, "action."+a.State, a); err != nil {
		return err
	}
	return tx.Commit()
}
