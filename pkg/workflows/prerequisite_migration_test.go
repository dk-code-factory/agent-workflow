package workflows

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

// The helper and worker-wait prerequisites assigned different meanings to v11
// while they were developed separately. Preserve both upgrade paths and backups.
func TestPrerequisiteSchemaLineagesMigrate(t *testing.T) {
	for _, lineage := range []string{"main-v11", "helper-v11", "helper-v12"} {
		t.Run(lineage, func(t *testing.T) {
			s := taskStateStore(t)
			statement := dropTaskStateFixtureSchema
			version := 11
			switch lineage {
			case "main-v11":
				statement += "DROP TABLE run_helper_assets; DROP TABLE helper_assets;"
			case "helper-v11":
				statement += "DROP TABLE worker_waits;"
			case "helper-v12":
				statement = "DROP TABLE worker_waits;"
				version = 12
			}
			statement += fmt.Sprintf("PRAGMA user_version=%d", version)
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			lock, err := ownership.Own(s.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = s.Migrate(t.Context(), lock); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"worker_waits", "helper_assets", "run_helper_assets", "task_groups", "task_claims", "task_budgets", "task_deadlines"} {
				var found int
				if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&found); err != nil || found != 1 {
					t.Fatalf("missing %s after migration: %d %v", table, found, err)
				}
			}
			before, err := openBackupSource(t.Context(), filepath.Join(s.Dir, fmt.Sprintf("state.pre-v%d.db", schemaVersion)))
			if err != nil {
				t.Fatal(err)
			}
			defer before.Close()
			var savedVersion int
			if err = before.QueryRow("PRAGMA user_version").Scan(&savedVersion); err != nil || savedVersion != version {
				t.Fatalf("retained version %d, want %d: %v", savedVersion, version, err)
			}
		})
	}
}
