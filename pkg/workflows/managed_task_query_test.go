package workflows

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"modernc.org/sqlite"
)

var managedTerminalPayloadBytes atomic.Int64

func init() {
	sqlite.MustRegisterScalarFunction("managed_terminal_payload_bytes", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch value := args[0].(type) {
		case string:
			managedTerminalPayloadBytes.Add(int64(len(value)))
		case []byte:
			managedTerminalPayloadBytes.Add(int64(len(value)))
		}
		return args[0], nil
	})
}

// Retained comparison for deterministic byte-work evidence, not a timing bound.
const managedTaskMarkerFirstQuery = `SELECT u.observation_key,u.record,v.record,(SELECT status FROM attempts WHERE id=v.id),a.run_id
 FROM usage_invocations v INDEXED BY usage_origin CROSS JOIN usage_observations u CROSS JOIN attempts a
 WHERE v.origin_id IN (` + managedTaskCoordinateOriginQuery + `)
 AND u.invocation_id=v.id AND u.observation_key IN (?,?)
 AND a.id IN (v.id,json_extract(CAST(u.record AS TEXT),'$.managed_sdk_attempt.original_admission.task.parent_id'),json_extract(CAST(u.record AS TEXT),'$.managed_codex_admission.admission.task.parent_id'))
 AND a.job_id=? AND a.step_id=? AND a.iteration=?`

func TestManagedConversationAdmissionDoesNotParseUnrelatedTerminals(t *testing.T) {
	s, ctx, run, _, turn := managedTaskFixture(t, "claude-sdk")
	var raw, identityRaw []byte
	if err := s.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, turn.ID, managedSDKAttemptKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var marker managedSDKAttemptRow
	if err := decodeJSONWithNumbers(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, turn.ID).Scan(&identityRaw); err != nil {
		t.Fatal(err)
	}
	var identity UsageIdentity
	if err := decodeJSONWithNumbers(identityRaw, &identity); err != nil {
		t.Fatal(err)
	}
	marker.Attempt.State = "settled"
	marker.Attempt.Terminal = &SessionResult{Status: RunStatusSucceeded, SessionID: "session", ProviderStatus: "success", ProviderVersion: "claude-agent-sdk@0.3.322", Outputs: map[string]any{"text": strings.Repeat("x", 1<<20), "cost_usd": float64(0), "usage": map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}}
	marker.Attempt.SessionID = "session"
	marker.Attempt.TerminalSHA256, _ = managedResultDigest(*marker.Attempt.Terminal)
	identity.SessionID = "session"
	identity.Finished = true
	if _, err := s.db.Exec(`DELETE FROM usage_observations WHERE invocation_id=?`, turn.ID); err != nil {
		t.Fatal(err)
	}
	for i := range 64 {
		identity.ID = fmt.Sprintf("completed-%d", i)
		marker.Attempt.InvocationID = identity.ID
		marker.Attempt.Admission.Task.Attempt.ID = identity.ID
		raw, _ = json.Marshal(marker)
		completed := turn
		completed.ID, completed.StepID, completed.Status = identity.ID, fmt.Sprintf("conversation/conversation/%d", i+1), RunStatusSucceeded
		completedRaw, _ := json.Marshal(completed)
		if _, err := s.db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)`, completed.ID, completed.RunID, completed.JobID, completed.StepID, completed.Iteration, completed.Number, completed.Status, completedRaw); err != nil {
			t.Fatal(err)
		}
		identityRaw, _ = json.Marshal(identity)
		if _, err := s.db.Exec(`INSERT INTO usage_invocations VALUES(?,?,?)`, identity.ID, run.ID, identityRaw); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO usage_observations VALUES(?,?,?)`, identity.ID, managedSDKAttemptKey, raw); err != nil {
			t.Fatal(err)
		}
	}
	retained := func() [32]byte {
		t.Helper()
		rows, err := s.db.Query(`SELECT record FROM usage_observations ORDER BY invocation_id,observation_key`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		h := sha256.New()
		for rows.Next() {
			var record []byte
			if err = rows.Scan(&record); err != nil {
				t.Fatal(err)
			}
			_, _ = h.Write(record)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return sum
	}
	before := retained()
	args := []any{run.ID, run.ID, managedSDKAttemptKey, managedCodexAdmissionKey, "job", "later-command", 1}
	managedTerminalPayloadBytes.Store(0)
	old := strings.ReplaceAll(managedTaskMarkerFirstQuery, "CAST(u.record AS TEXT)", "managed_terminal_payload_bytes(CAST(u.record AS TEXT))")
	rows, err := s.db.Query(old, args...)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		rows.Close()
		t.Fatal("unrelated old coordinate matched")
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	bytes := managedTerminalPayloadBytes.Load()
	if bytes < 64<<20 {
		t.Fatal("control did not visit supported retained payload workload", bytes)
	}
	t.Logf("marker-first JSON input bytes per admission=%d; 500 admissions=%d", bytes, bytes*500)
	rows, err = s.db.Query(`EXPLAIN `+managedTaskCoordinateQuery, args...)
	if err != nil {
		t.Fatal(err)
	}
	functions := 0
	for rows.Next() {
		var address, p1, p2, p3, p5 int
		var opcode string
		var p4, comment any
		if err = rows.Scan(&address, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if opcode == "Function" && strings.Contains(fmt.Sprint(p4), "json_extract") {
			functions++
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if functions != 0 {
		t.Fatal("ordinary admission still executes retained JSON extraction", functions)
	}
	if _, _, err = s.Begin(ctx, run.ID, "job", "later-command", 1, false); err != nil {
		t.Fatal(err)
	}
	if after := retained(); before != after {
		t.Fatal("admission rewrote original terminal evidence", before, after)
	}
	t.Logf("ordinary admission VDBE JSON extraction instructions=%d; retained SHA256=%x", functions, before)
}

func TestManagedConversationParentIndexMigration(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%v", provider, malformed), func(t *testing.T) {
				s, ctx, run, parent, turn := managedTaskFixture(t, provider)
				key, parentPath := managedSDKAttemptKey, "$.managed_sdk_attempt.original_admission.task.parent_id"
				if provider == "codex-app-server" {
					key, parentPath = managedCodexAdmissionKey, "$.managed_codex_admission.admission.task.parent_id"
				}
				if _, err := s.db.Exec(`DROP INDEX usage_managed_sdk_parent; DROP INDEX usage_managed_codex_parent; PRAGMA user_version=13`); err != nil {
					t.Fatal(err)
				}
				if malformed {
					if _, err := s.db.Exec(`UPDATE usage_observations SET record='{' WHERE invocation_id=? AND observation_key=?`, turn.ID, key); err != nil {
						t.Fatal(err)
					}
				}
				var before []byte
				if err := s.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, turn.ID, key).Scan(&before); err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if inspection, err := OpenSQLiteInspection(s.Dir); err == nil {
					inspection.Close()
					t.Fatal("read-only inspection accepted an old schema without explicit upgrade")
				}
				fresh, err := OpenSQLite(s.Dir)
				if err != nil {
					t.Fatal(err)
				}
				defer fresh.Close()
				assertSchema := func(version, indexes int) {
					t.Helper()
					var v, n int
					if err := fresh.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
						t.Fatal(err)
					}
					if err := fresh.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('usage_managed_sdk_parent','usage_managed_codex_parent')`).Scan(&n); err != nil {
						t.Fatal(err)
					}
					if v != version || n != indexes {
						t.Fatal("unexpected schema", v, n, version, indexes)
					}
				}
				assertSchema(13, 0) // Inspection does not silently migrate an old store.
				if err = fresh.Migrate(ctx, nil); err == nil {
					t.Fatal("unowned migration accepted")
				}
				owner, err := ownership.Own(fresh.Dir)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				err = fresh.Migrate(ctx, owner)
				if malformed {
					if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
						t.Fatal("malformed private marker silently omitted", err)
					}
					assertSchema(13, 0)
				} else {
					if err != nil {
						t.Fatal(err)
					}
					assertSchema(14, 2)
					for _, a := range []Attempt{turn, parent} {
						if _, _, err = fresh.BeginInput(ctx, run.ID, a.JobID, a.StepID, a.Iteration, true, "changed", true, ""); !errors.Is(err, ErrActionIntegrity) {
							t.Fatal("upgraded original lost direct/parent hold", a.ID, err)
						}
					}
					// A retained row matching direct and parent branches is returned
					// once; the marker and its original admission are then restored.
					if _, err = fresh.db.Exec(`UPDATE usage_observations SET record=json_set(CAST(record AS TEXT),?,?) WHERE invocation_id=? AND observation_key=?`, parentPath, turn.ID, turn.ID, key); err != nil {
						t.Fatal(err)
					}
					rows, err := fresh.db.Query(managedTaskCoordinateQuery, run.ID, run.ID, managedSDKAttemptKey, managedCodexAdmissionKey, turn.JobID, turn.StepID, turn.Iteration)
					if err != nil {
						t.Fatal(err)
					}
					count := 0
					for rows.Next() {
						count++
					}
					rowErr := rows.Err()
					rows.Close()
					if rowErr != nil || count != 1 {
						t.Fatal("direct/parent branches not deduplicated", count, rowErr)
					}
					if _, err = fresh.db.Exec(`UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, before, turn.ID, key); err != nil {
						t.Fatal(err)
					}
				}
				var after []byte
				if err = fresh.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, turn.ID, key).Scan(&after); err != nil || string(before) != string(after) {
					t.Fatal("migration rewrote original marker", err)
				}
				backup, err := openBackupSource(ctx, filepath.Join(fresh.Dir, "state.pre-v14.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer backup.Close()
				var version int
				if err = backup.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 13 {
					t.Fatal("prior schema not retained", version, err)
				}
				if err = backup.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, turn.ID, key).Scan(&after); err != nil || string(before) != string(after) {
					t.Fatal("backup lost original evidence", err)
				}
			})
		}
	}
}
