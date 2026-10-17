package workflows

import (
	"context"
	"time"
)

func (s *SQLiteStore) ProbeStorage(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "UPDATE runs SET updated=updated WHERE id='' ")
	return err
}

type StoreMetrics struct {
	QueueDepth      int            `json:"queue_depth"`
	QueueAgeSeconds float64        `json:"queue_age_seconds"`
	Runs            map[string]int `json:"runs"`
	Workers         map[string]int `json:"workers"`
	Failures        int            `json:"failures"`
	CleanupBacklog  int            `json:"cleanup_backlog"`
}

func (s *SQLiteStore) Metrics(ctx context.Context) (StoreMetrics, error) {
	out := StoreMetrics{Runs: map[string]int{}, Workers: map[string]int{}}
	group := func(table string, target map[string]int) error {
		rows, err := s.db.QueryContext(ctx, "SELECT status,count(*) FROM "+table+" GROUP BY status")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var count int
			if err = rows.Scan(&status, &count); err != nil {
				return err
			}
			target[status] = count
		}
		return rows.Err()
	}
	if err := group("runs", out.Runs); err != nil {
		return out, err
	}
	if err := group("workers", out.Workers); err != nil {
		return out, err
	}
	out.QueueDepth = out.Runs[RunStatusQueued]
	if out.QueueDepth > 0 {
		var admitted int64
		var started string
		if err := s.db.QueryRowContext(ctx, "SELECT admitted,COALESCE(json_extract(CAST(record AS TEXT),'$.started_at'),'') FROM runs WHERE status='queued' ORDER BY admitted,id LIMIT 1").Scan(&admitted, &started); err != nil {
			return out, err
		}
		when := time.Unix(0, admitted)
		if admitted < 946684800000000000 {
			when, _ = time.Parse(time.RFC3339Nano, started)
		}
		if !when.IsZero() {
			out.QueueAgeSeconds = time.Since(when).Seconds()
			if out.QueueAgeSeconds < 0 {
				out.QueueAgeSeconds = 0
			}
		}
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM attempts WHERE status='failed'").Scan(&out.Failures); err != nil {
		return out, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM invocations i JOIN runs r ON r.id=i.run_id WHERE i.state IN ('intent','launching','created','running','uncertain') AND (i.state='uncertain' OR r.status!='running')").Scan(&out.CleanupBacklog); err != nil {
		return out, err
	}
	return out, nil
}
