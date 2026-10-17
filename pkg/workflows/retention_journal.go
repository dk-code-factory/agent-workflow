package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

type Removal struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	State string `json:"state"`
}
type Retention struct {
	OriginID  string    `json:"origin_run_id"`
	Root      string    `json:"workspace"`
	State     string    `json:"state"`
	Removals  []Removal `json:"removals"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *SQLiteStore) Retention(ctx context.Context, id string) (Retention, error) {
	var raw []byte
	var record Retention
	err := s.db.QueryRowContext(ctx, "SELECT record FROM retention_groups WHERE origin_id=?", id).Scan(&raw)
	if err != nil {
		return record, err
	}
	err = decodeJSONWithNumbers(raw, &record)
	return record, err
}
func (s *SQLiteStore) BeginRetention(ctx context.Context, record Retention, owner *ownership.Lock) (Retention, error) {
	if !owner.Holds(s.Dir) {
		return record, fmt.Errorf("retention requires exclusive workspace ownership")
	}
	if existing, err := s.Retention(ctx, record.OriginID); err == nil {
		if existing.Root != record.Root {
			return record, fmt.Errorf("retention workspace identity changed")
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return record, err
	}
	if _, err := s.OriginRuns(ctx, record.OriginID); err != nil {
		return record, err
	}
	var active int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM workers WHERE run_id=? AND status='running'", record.OriginID).Scan(&active); err != nil {
		return record, err
	}
	if active > 0 {
		return record, fmt.Errorf("retention requires stopped workers")
	}
	record.State = "planned"
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO retention_groups VALUES(?,?,?)", record.OriginID, record.State, raw)
	return record, err
}
func (s *SQLiteStore) SaveRetention(ctx context.Context, record Retention, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("retention requires exclusive workspace ownership")
	}
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE retention_groups SET state=?,record=? WHERE origin_id=?", record.State, raw, record.OriginID)
	return err
}
func (s *SQLiteStore) FinishRetention(ctx context.Context, record Retention, owner *ownership.Lock) error {
	if !owner.Holds(s.Dir) {
		return fmt.Errorf("retention requires exclusive workspace ownership")
	}
	for _, removal := range record.Removals {
		if removal.State != "removed" {
			return fmt.Errorf("filesystem removal is incomplete")
		}
	}
	runs, err := s.OriginRuns(ctx, record.OriginID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, run := range runs {
		if _, err = tx.ExecContext(ctx, "DELETE FROM runs WHERE id=? AND status NOT IN ('running','queued','needs_attention')", run.ID); err != nil {
			return err
		}
	}
	record.State = "complete"
	record.UpdatedAt = time.Now().UTC()
	raw, _ := json.Marshal(record)
	if _, err = tx.ExecContext(ctx, "UPDATE retention_groups SET state='complete',record=? WHERE origin_id=?", raw, record.OriginID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) InvocationDirectories(ctx context.Context, origin string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM invocations WHERE run_id=? ORDER BY rowid", origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("invalid invocation ownership identity")
		}
		out = append(out, filepath.Join(s.Dir, "invocations", id))
	}
	return out, rows.Err()
}
