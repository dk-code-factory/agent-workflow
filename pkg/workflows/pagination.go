package workflows

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type RunSummary struct {
	ID          string `json:"id"`
	OriginID    string `json:"origin_run_id"`
	Generation  int64  `json:"execution_generation"`
	WorkflowRef string `json:"workflow_ref"`
	Status      string `json:"status"`
	Updated     string `json:"updated"`
}

type WorkerSummary struct {
	ID       string `json:"id"`
	RunID    string `json:"run_id"`
	ParentID string `json:"parent_id,omitempty"`
	Role     string `json:"role"`
	Status   string `json:"status"`
}

type MessageSummary struct {
	ID       string `json:"id"`
	WorkerID string `json:"worker_id"`
	Sequence int    `json:"sequence"`
	Status   string `json:"status"`
}

type pageCursor struct {
	Scope string `json:"s"`
	After int64  `json:"a"`
}

func pageBounds(limit int, cursor, scope string) (int, int64, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return 0, 0, fmt.Errorf("limit must be between 1 and 1000")
	}
	if cursor == "" {
		return limit, 0, nil
	}
	if len(cursor) > 1024 {
		return 0, 0, fmt.Errorf("invalid continuation cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid continuation cursor")
	}
	var c pageCursor
	if err = json.Unmarshal(b, &c); err != nil || c.Scope != scope || c.After <= 0 {
		return 0, 0, fmt.Errorf("invalid continuation cursor")
	}
	return limit, c.After, nil
}

func nextCursor(scope string, after int64) string {
	b, _ := json.Marshal(pageCursor{scope, after})
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *SQLiteStore) ListPage(ctx context.Context, limit int, cursor string) (Page[RunSummary], error) {
	out := Page[RunSummary]{Items: []RunSummary{}}
	limit, after, err := pageBounds(limit, cursor, "runs")
	if err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rowid,id,origin_id,generation,COALESCE(json_extract(CAST(record AS TEXT),'$.workflow_ref'),''),status,updated FROM runs WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var row int64
		var r RunSummary
		if err = rows.Scan(&row, &r.ID, &r.OriginID, &r.Generation, &r.WorkflowRef, &r.Status, &r.Updated); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			out.NextCursor = nextCursor("runs", last)
			break
		}
		out.Items = append(out.Items, r)
		last = row
	}
	return out, rows.Err()
}

func (m *WorkerManager) ListPage(ctx context.Context, run string, limit int, cursor string) (Page[WorkerSummary], error) {
	out := Page[WorkerSummary]{Items: []WorkerSummary{}}
	scope := "workers:" + run
	limit, after, err := pageBounds(limit, cursor, scope)
	if err != nil {
		return out, err
	}
	rows, err := m.Store.db.QueryContext(ctx, "SELECT rowid,id,run_id,parent_id,role,status FROM workers WHERE run_id=? AND rowid>? ORDER BY rowid LIMIT ?", run, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var row int64
		var w WorkerSummary
		if err = rows.Scan(&row, &w.ID, &w.RunID, &w.ParentID, &w.Role, &w.Status); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			out.NextCursor = nextCursor(scope, last)
			break
		}
		out.Items = append(out.Items, w)
		last = row
	}
	return out, rows.Err()
}

func (m *WorkerManager) MessagesPage(ctx context.Context, run, worker string, limit int, cursor string) (Page[MessageSummary], error) {
	out := Page[MessageSummary]{Items: []MessageSummary{}}
	scope := "messages:" + run + ":" + worker
	limit, after, err := pageBounds(limit, cursor, scope)
	if err != nil {
		return out, err
	}
	// Validate ownership even for an empty mailbox.
	var owner string
	if err = m.Store.db.QueryRowContext(ctx, "SELECT run_id FROM workers WHERE id=? AND run_id=?", worker, run).Scan(&owner); err != nil {
		return out, err
	}
	rows, err := m.Store.db.QueryContext(ctx, "SELECT sequence,id,worker_id,status FROM messages WHERE worker_id=? AND sequence>? ORDER BY sequence LIMIT ?", worker, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var message MessageSummary
		if err = rows.Scan(&message.Sequence, &message.ID, &message.WorkerID, &message.Status); err != nil {
			return out, err
		}
		if len(out.Items) == limit {
			out.NextCursor = nextCursor(scope, last)
			break
		}
		out.Items = append(out.Items, message)
		last = int64(message.Sequence)
	}
	return out, rows.Err()
}
