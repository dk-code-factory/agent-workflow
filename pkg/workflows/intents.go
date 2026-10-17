package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type ExternalIntent struct {
	Key     string         `json:"key"`
	RunID   string         `json:"run_id"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload"`
	Status  string         `json:"status"`
	Result  map[string]any `json:"result,omitempty"`
}

func (s *SQLiteStore) InitIntents(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS external_intents(key TEXT PRIMARY KEY,run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,kind TEXT NOT NULL,record BLOB NOT NULL)")
	return err
}
func (s *SQLiteStore) Intent(ctx context.Context, key string) (ExternalIntent, error) {
	var b []byte
	var intent ExternalIntent
	err := s.db.QueryRowContext(ctx, "SELECT record FROM external_intents WHERE key=?", key).Scan(&b)
	if err != nil {
		return intent, err
	}
	err = decodeJSONWithNumbers(b, &intent)
	return intent, err
}
func (s *SQLiteStore) PutIntent(ctx context.Context, intent ExternalIntent) error {
	b, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if intent.Status == "pending" {
		_, err = tx.ExecContext(ctx, "INSERT INTO external_intents VALUES(?,?,?,?)", intent.Key, intent.RunID, intent.Kind, b)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, "UPDATE external_intents SET record=? WHERE key=? AND run_id=?", b, intent.Key, intent.RunID)
		if err == nil {
			n, _ := res.RowsAffected()
			if n != 1 {
				err = fmt.Errorf("intent was not admitted")
			}
		}
	}
	if err != nil {
		return err
	}
	if err = addEvent(ctx, tx, intent.RunID, "action."+intent.Status, intent); err != nil {
		return err
	}
	return tx.Commit()
}
