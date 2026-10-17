package workflows

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReplacementSQLiteConnectionRetainsDurabilityAndForeignKeys(t *testing.T) {
	// URI metacharacters must remain part of the filename, not DSN options.
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "state?#fixture"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for attempt := 0; attempt < 2; attempt++ {
		for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 5000} {
			var got int
			if err := store.db.QueryRowContext(context.Background(), "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
				t.Fatalf("connection %d %s=%d want %d: %v", attempt, pragma, got, want, err)
			}
		}
		// Evict the idle connection so the next query opens a replacement.
		store.db.SetMaxIdleConns(0)
		store.db.SetMaxIdleConns(1)
	}
}
