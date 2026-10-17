package workflows

import (
	"database/sql"
	"fmt"
	"path/filepath"
)

// OpenSQLiteInspection never creates or migrates a workspace database.
func OpenSQLiteInspection(dir string) (*SQLiteStore, error) {
	path, err := filepath.Abs(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, err
	}
	dsn, err := sqliteDSN(path, true)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, Dir: dir}
	if err = s.RequireCurrentSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) RequireCurrentSchema() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("unsupported state schema version %d (maximum %d)", version, schemaVersion)
	}
	if version != schemaVersion {
		return fmt.Errorf("state schema version %d requires upgrade to %d; start the controller under exclusive workspace ownership", version, schemaVersion)
	}
	return nil
}
