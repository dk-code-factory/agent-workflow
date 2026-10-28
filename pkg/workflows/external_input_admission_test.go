package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func TestInputContinuationBlockedOldestMakesBoundedProgress(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			e, s := durableExecutor(t, `jobs:
  decide:
    steps:
      - uses: agent/a
        continue-on-error: true
        with: {prompt: preparation}
      - uses: input/wait
        with: {prompt: q, evidence-id: e}
`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				if calls.Add(1) == 1 {
					return nil, errors.New("tolerated failure")
				}
				return map[string]any{}, nil
			}))
			var runs []*Run
			for i := 0; i < 2; i++ {
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				if err != nil || run.Status != RunStatusWaiting {
					t.Fatal(run, err)
				}
				answerGate(t, s, inputGate(t, s, run.ID))
				runs = append(runs, run)
			}
			if err := s.QueueInputContinuations(ctx, slots); err != nil {
				t.Fatal("blocked origin aborted pass", err)
			}
			blocked, _, err := s.Load(ctx, runs[0].ID)
			diagnostic := "explicit retry_uncertain acknowledgement required for uncertain attempts"
			if err != nil || blocked.Status != RunStatusNeedsAttention || blocked.Generation != runs[0].Generation || blocked.Error != diagnostic {
				t.Fatal("missing authoritative blocked diagnosis", blocked, err)
			}
			control, err := s.InputControl(ctx, blocked.ID)
			if err != nil || !control.Continuation || control.Canceled || control.Paused {
				t.Fatal("blocked intent changed", control, err)
			}
			gate := inputGate(t, s, blocked.ID)
			if gate.State != "accepted" || gate.Answer == nil || len(gate.History) != 2 {
				t.Fatal("blocked answer/history changed", gate)
			}
			later, _, err := s.Load(ctx, runs[1].ID)
			want := RunStatusWaiting
			if slots == 2 {
				want = RunStatusQueued
			}
			if err != nil || later.Status != want {
				t.Fatal("per-pass work or later admission changed", later, err)
			}
			if err = s.QueueInputContinuations(ctx, slots); err != nil {
				t.Fatal(err)
			}
			later, _, err = s.Load(ctx, later.ID)
			if err != nil || later.Status != RunStatusQueued || later.Generation != runs[1].Generation+1 {
				t.Fatal("oldest origin starved next pass", later, err)
			}
			if _, err = s.QueueResume(ctx, blocked.ID, "", false, slots); err == nil || err.Error() != diagnostic {
				t.Fatal("explicit retry guard weakened", err)
			}
			done, err := e.Resume(ctx, later.ID, nil, false)
			if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 2 {
				t.Fatal("unrelated continuation replayed preparation", done, err, calls.Load())
			}
			done, err = e.Resume(ctx, blocked.ID, nil, true)
			if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 3 {
				t.Fatal("acknowledged retry did not retain answer", done, err, calls.Load())
			}
		})
	}
}

func TestInputContinuationOnlyKnownGuardsPublishAttention(t *testing.T) {
	for _, guard := range []string{"restore", "worker", "tool", "unknown record", "storage publication", "global cleanup", "paused", "canceled", "new generation"} {
		t.Run(guard, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
			run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err != nil {
				t.Fatal(err)
			}
			answerGate(t, s, inputGate(t, s, run.ID))
			diagnostic := ""
			switch guard {
			case "restore":
				run.FailureClass = "restore_uncertainty"
				if err = s.Save(ctx, run); err != nil {
					t.Fatal(err)
				}
				diagnostic = "restored effects require explicit retry_uncertain acknowledgement"
			case "worker":
				if _, err = s.db.Exec(`INSERT INTO workers VALUES('worker',?,'','review','idle','{}')`, run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec(`INSERT INTO messages VALUES('message','worker','request',1,'failed','{}')`); err != nil {
					t.Fatal(err)
				}
				diagnostic = "explicit retry_uncertain acknowledgement required for uncertain worker turns"
			case "tool":
				if _, err = s.db.Exec(`INSERT INTO tool_calls VALUES(?,1,'invocation','call','fixture','tool','fingerprint','uncertain','{}')`, run.ID); err != nil {
					t.Fatal(err)
				}
				diagnostic = "explicit retry_uncertain acknowledgement required for uncertain tool effects"
			case "unknown record":
				if _, err = s.db.Exec(`UPDATE runs SET record='invalid-json' WHERE id=?`, run.ID); err != nil {
					t.Fatal(err)
				}
			case "storage publication":
				run.FailureClass = "restore_uncertainty"
				if err = s.Save(ctx, run); err != nil {
					t.Fatal(err)
				}
				if _, err = s.db.Exec(`CREATE TRIGGER fail_attention BEFORE UPDATE ON runs WHEN NEW.status='needs_attention' BEGIN SELECT RAISE(ABORT,'storage publication fault'); END`); err != nil {
					t.Fatal(err)
				}
			case "global cleanup":
				if _, err = s.db.Exec(`INSERT INTO invocations VALUES('uncertain-invocation',?,1,'uncertain','{}',0)`, run.ID); err != nil {
					t.Fatal(err)
				}
			case "paused":
				if err = s.InterruptInput(ctx, run.ID, run.Generation); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				if err = s.RequestCancel(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
			case "new generation":
				if _, err = s.QueueResume(ctx, run.ID, "manual", false, 1); err != nil {
					t.Fatal(err)
				}
			}
			beforeEvents, eventErr := s.Events(ctx, run.ID, 0)
			if eventErr != nil {
				t.Fatal(eventErr)
			}
			err = s.QueueInputContinuations(ctx, 1)
			if guard == "paused" || guard == "canceled" || guard == "new generation" {
				if _, automaticErr := s.queueResume(ctx, run.ID, "", false, 1, true); automaticErr != nil {
					t.Fatal("automatic revalidation changed inactive/new owner", automaticErr)
				}
			}
			shouldError := guard == "unknown record" || guard == "storage publication" || guard == "global cleanup"
			if (err != nil) != shouldError {
				t.Fatal("unrecognized outcome classification", err)
			}
			if guard == "storage publication" && !strings.Contains(err.Error(), "storage publication fault") {
				t.Fatal("publication failure hidden", err)
			}
			if shouldError {
				afterEvents, eventErr := s.Events(ctx, run.ID, 0)
				if eventErr != nil || len(beforeEvents) != len(afterEvents) {
					t.Fatal("failed admission published an event", eventErr, len(beforeEvents), len(afterEvents))
				}
			}
			var status string
			var generation int64
			if scanErr := s.db.QueryRow(`SELECT status,generation FROM runs WHERE id=?`, run.ID).Scan(&status, &generation); scanErr != nil {
				t.Fatal(scanErr)
			}
			want := RunStatusWaiting
			if diagnostic != "" {
				want = RunStatusNeedsAttention
			}
			if guard == "paused" {
				want = RunStatusInterrupted
			}
			if guard == "canceled" {
				want = RunStatusCanceled
			}
			if guard == "new generation" {
				want = RunStatusQueued
			}
			wantGeneration := run.Generation
			if guard == "new generation" {
				wantGeneration++
			}
			if status != want || generation != wantGeneration {
				t.Fatal("guard changed generation/status", status, generation)
			}
			if diagnostic != "" {
				stored, _, loadErr := s.Load(ctx, run.ID)
				if loadErr != nil || stored.Error != diagnostic {
					t.Fatal(stored, loadErr)
				}
				if _, manualErr := s.QueueResume(ctx, run.ID, "", false, 1); manualErr == nil || manualErr.Error() != diagnostic {
					t.Fatal("parked origin lost acknowledgement guard", manualErr)
				}
				if guard == "tool" {
					var status string
					if toolErr := s.db.QueryRow("SELECT status FROM tool_calls WHERE run_id=?", run.ID).Scan(&status); toolErr != nil || status != "uncertain" {
						t.Fatal("parking inferred tool outcome", status, toolErr)
					}
				}
			}
			gate := inputGate(t, s, run.ID)
			if guard == "canceled" {
				if gate.State != "canceled" {
					t.Fatal(gate)
				}
			} else if gate.State != "accepted" || len(gate.History) != 2 {
				t.Fatal("guard consumed/discarded answer", gate)
			}
			control, controlErr := s.InputControl(ctx, run.ID)
			if controlErr != nil || (guard != "canceled" && guard != "new generation" && !control.Continuation) {
				t.Fatal("guard lost durable intent", control, controlErr)
			}
		})
	}
}

func seedZeroIntentWaits(t *testing.T, s *SQLiteStore, count int) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	queries := []string{
		`INSERT INTO runs(id,status,record,snapshot,updated,origin_id,generation,owner,admitted) VALUES(?,'waiting',?,'{"version":1}','fixture',?,1,'',?)`,
		`INSERT INTO executions(run_id,generation,retry_uncertain,admitted,status,record) VALUES(?,1,0,?,'waiting',?)`,
		`INSERT INTO input_controls(run_id) VALUES(?)`,
		`INSERT INTO input_gates(id,origin_id,run_id,state,record) VALUES(?,?,?,'pending',?)`,
	}
	statements := make([]*sql.Stmt, len(queries))
	for i, query := range queries {
		statements[i], err = tx.PrepareContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer statements[i].Close()
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("unanswered-%05d", i)
		r := Run{ID: id, OriginID: id, Version: 1, Generation: 1, Status: RunStatusWaiting, Jobs: map[string]JobExecution{}}
		raw, _ := json.Marshal(r)
		if _, err = statements[0].ExecContext(ctx, id, raw, id, i); err != nil {
			t.Fatal(err)
		}
		if _, err = statements[1].ExecContext(ctx, id, i, raw); err != nil {
			t.Fatal(err)
		}
		if _, err = statements[2].ExecContext(ctx, id); err != nil {
			t.Fatal(err)
		}
		g := ExternalInputGate{ID: actionHash([]any{id, "question", "input", 1}), OriginID: id, RunID: id, Generation: 1, JobID: "question", StepID: "input", Iteration: 1, EvidenceID: "e", Prompt: "q", State: "pending"}
		g.Fingerprint = inputFingerprint(g)
		gate, _ := json.Marshal(g)
		if _, err = statements[3].ExecContext(ctx, g.ID, id, id, gate); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestInputContinuationZeroIntentBacklogSkipsDiscovery(t *testing.T) {
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedZeroIntentWaits(t, s, 10000)
	// With no continuation intent, a free queue must not touch the gate candidate
	// source at all. Only the eligibility index and originating-run lookup apply.
	if _, err = s.db.Exec(`ALTER TABLE input_gates RENAME TO unavailable_gates`); err != nil {
		t.Fatal(err)
	}
	if err = s.QueueInputContinuations(context.Background(), 128); err != nil {
		t.Fatal("zero-intent backlog reached discovery", err)
	}
	var queued int
	if err = s.db.QueryRow(`SELECT count(*) FROM runs WHERE status='queued'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatal(queued, err)
	}
}

func TestInputContinuationPartialAnswersRemainWaiting(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs:
  a: {steps: [{uses: input/wait, with: {prompt: a, evidence-id: e}}]}
  b: {steps: [{uses: input/wait, with: {prompt: b, evidence-id: e}}]}
`, nil)
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	gates, err := s.Inputs(ctx, run.ID)
	if err != nil || len(gates) != 2 {
		t.Fatal(gates, err)
	}
	answerGate(t, s, gates[0])
	if err = s.QueueInputContinuations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.Load(ctx, run.ID)
	if err != nil || current.Status != RunStatusWaiting || current.Generation != run.Generation {
		t.Fatal("partial answer admitted", current, err)
	}
	control, err := s.InputControl(ctx, run.ID)
	if err != nil || !control.Continuation {
		t.Fatal(control, err)
	}
	answerGate(t, s, gates[1])
	if err = s.QueueInputContinuations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	current, _, err = s.Load(ctx, run.ID)
	if err != nil || current.Status != RunStatusQueued || current.Generation != run.Generation+1 {
		t.Fatal(current, err)
	}
}

func inputContinuationIndexPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='input_controls_continuations'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func TestInputContinuationEligibilityIndexMigration(t *testing.T) {
	ctx := context.Background()
	t.Run("fresh index and zero-intent plan", func(t *testing.T) {
		s, err := OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if !inputContinuationIndexPresent(t, s.db) {
			t.Fatal("fresh v10 lacks eligibility index")
		}
		seedZeroIntentWaits(t, s, 10000)
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM input_controls c CROSS JOIN runs r ON r.id=c.run_id WHERE c.continuation=1 AND c.paused=0 AND c.canceled=0 AND r.status='waiting' AND r.owner='')`)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Log(details)
		joined := strings.Join(details, "\n")
		if !strings.Contains(joined, "SCAN c USING INDEX input_controls_continuations") || !strings.Contains(joined, "SEARCH r USING INDEX sqlite_autoindex_runs_1") {
			t.Fatal("probe does not start with eligible controls", details)
		}
	})
	t.Run("existing v10 opens without mutation then owned repair", func(t *testing.T) {
		e, s := durableExecutor(t, `jobs: {ask: {steps: [{uses: input/wait, with: {prompt: q, evidence-id: e}}]}}`, nil)
		run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
		if err != nil {
			t.Fatal(err)
		}
		answerGate(t, s, inputGate(t, s, run.ID))
		if _, err = s.db.Exec(`DROP INDEX IF EXISTS input_controls_continuations`); err != nil {
			t.Fatal(err)
		}
		dir := s.Dir
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		view, err := OpenSQLiteInspection(dir)
		if err != nil {
			t.Fatal(err)
		}
		if inputContinuationIndexPresent(t, view.db) {
			t.Fatal("inspection repaired schema")
		}
		if inputGate(t, view, run.ID).State != "accepted" {
			t.Fatal("inspection lost answer")
		}
		view.Close()
		reopened, err := OpenSQLite(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if inputContinuationIndexPresent(t, reopened.db) {
			t.Fatal("ordinary open repaired schema")
		}
		if err = reopened.QueueInputContinuations(ctx, 1); err != nil {
			t.Fatal("pre-index v10 lost functional admission", err)
		}
		compatible, _, err := reopened.Load(ctx, run.ID)
		if err != nil || compatible.Status != RunStatusQueued || compatible.Generation != run.Generation+1 || inputContinuationIndexPresent(t, reopened.db) {
			t.Fatal("pre-index admission mutated schema/failed", compatible, err)
		}
		if err = reopened.Migrate(ctx, nil); err == nil {
			t.Fatal("index repair lacked ownership")
		}
		lock, err := ownership.Own(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err = reopened.Migrate(ctx, lock); err != nil {
			t.Fatal(err)
		}
		if !inputContinuationIndexPresent(t, reopened.db) {
			t.Fatal("owned migration omitted index")
		}
		paths, err := filepath.Glob(filepath.Join(dir, "state.pre-v10-input-continuations*.db"))
		if err != nil || len(paths) != 1 {
			t.Fatal("missing retained repair backup", paths, err)
		}
		prior, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		backup, err := openBackupSource(ctx, paths[0])
		if err != nil {
			t.Fatal(err)
		}
		if inputContinuationIndexPresent(t, backup) {
			t.Fatal("backup is not pre-repair")
		}
		backup.Close()
		if err = reopened.Migrate(ctx, lock); err != nil {
			t.Fatal(err)
		}
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = RestoreDatabase(ctx, dir, paths[0], lock); err != nil {
			t.Fatal(err)
		}
		restored, err := OpenSQLite(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		if inputContinuationIndexPresent(t, restored.db) {
			t.Fatal("restored old index unexpectedly created")
		}
		if err = restored.Migrate(ctx, lock); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(paths[0])
		if err != nil || string(after) != string(prior) {
			t.Fatal("repair overwrote earlier backup", err)
		}
		paths, err = filepath.Glob(filepath.Join(dir, "state.pre-v10-input-continuations*.db"))
		if err != nil || len(paths) != 2 {
			t.Fatal("restored repair lacks fresh backup", paths, err)
		}
		gate := inputGate(t, restored, run.ID)
		if gate.State != "accepted" || gate.Answer == nil || len(gate.History) != 2 {
			t.Fatal("repair/restore changed accepted history", gate)
		}
	})
	t.Run("v9 upgrade retains backup", func(t *testing.T) {
		s, err := OpenSQLite(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err = s.db.Exec(`DROP TABLE input_gates; DROP TABLE input_controls; PRAGMA user_version=9`); err != nil {
			t.Fatal(err)
		}
		lock, err := ownership.Own(s.Dir)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err = s.Migrate(ctx, lock); err != nil {
			t.Fatal(err)
		}
		if !inputContinuationIndexPresent(t, s.db) {
			t.Fatal("v9 upgrade lacks eligibility index")
		}
		backup, err := openBackupSource(ctx, filepath.Join(s.Dir, "state.pre-v10.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer backup.Close()
		var version int
		if err = backup.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
			t.Fatal(version, err)
		}
	})
}
