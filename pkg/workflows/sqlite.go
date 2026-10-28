package workflows

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	_ "modernc.org/sqlite"
)

const RunStatusNeedsAttention = "needs_attention"

// Snapshot is the admitted definition closure and nonsecret execution context.
// Credentials are resolved again on resume; values never belong here.
type Snapshot struct {
	ActionContracts map[string]ActionContract `json:"application_actions,omitempty"`
	Execution       runtimeexec.Policy        `json:"execution,omitempty"`
	Metadata        map[string]any            `json:"metadata,omitempty"`
	Version         int                       `json:"version"`
	Definitions     map[string]*Workflow      `json:"definitions"`
	Hashes          map[string]string         `json:"hashes"`
	Inputs          map[string]any            `json:"inputs,omitempty"`
	Event           map[string]any            `json:"event,omitempty"`
	SecretRefs      map[string]string         `json:"secret_refs,omitempty"`
	RequestID       string                    `json:"request_id,omitempty"`
	RequiredSecrets []string                  `json:"required_secrets,omitempty"`
	Roles           json.RawMessage           `json:"roles,omitempty"`
	Parallel        int                       `json:"parallel"`
}

type Attempt struct {
	InputFingerprint string           `json:"input_fingerprint,omitempty"`
	Phase            string           `json:"phase,omitempty"`
	TaskAttemptID    string           `json:"task_attempt_id,omitempty"`
	TaskOutputs      map[string]any   `json:"task_outputs,omitempty"`
	RepairCount      int              `json:"repair_count,omitempty"`
	RepairOutputs    []map[string]any `json:"repair_outputs,omitempty"`
	ID               string           `json:"id"`
	RunID            string           `json:"run_id"`
	JobID            string           `json:"job_id"`
	StepID           string           `json:"step_id"`
	Iteration        int              `json:"iteration"`
	Number           int              `json:"number"`
	Status           string           `json:"status"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       time.Time        `json:"finished_at,omitempty"`
	Result           StepExecution    `json:"result"`
}

type Event struct {
	Version  int             `json:"version"`
	Sequence int64           `json:"sequence"`
	RunID    string          `json:"run_id"`
	Time     time.Time       `json:"time"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// Journal supplements the compatibility checkpoint interface with durable
// admission and completion. Completion is committed before a dependent starts.
type Journal interface {
	RunStore
	Admit(context.Context, *Run, Snapshot) error
	Load(context.Context, string) (*Run, Snapshot, error)
	Begin(context.Context, string, string, string, int, bool) (Attempt, bool, error)
	Complete(context.Context, Attempt, StepExecution) error
	Claim(context.Context, *Run, string) error
	RecordAttempt(context.Context, Attempt) error
}

type SQLiteStore struct {
	db  *sql.DB
	Dir string
}

func OpenSQLite(dir string) (*SQLiteStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Create privately before SQLite opens it (also protects WAL/SHM via directory).
	path := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	dsn, err := sqliteDSN(path, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db, Dir: dir}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version > schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("unsupported state schema version %d (maximum %d)", version, schemaVersion)
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Only empty stores are initialized here. Upgrades require an explicit
	// Migrate call under exclusive workspace ownership; inspection never upgrades.
	if version == 0 {
		lock, lockErr := ownership.Own(dir)
		if lockErr != nil {
			_ = db.Close()
			return nil, lockErr
		}
		err = s.Migrate(context.Background(), lock)
		_ = lock.Close()
		if err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *SQLiteStore) Close() error { return s.db.Close() }
func newID() string { // crypto/rand is shared with the executor's admission helper.
	return randomID()
}
func snapshotFor(all map[string]*Workflow, req RunRequest, roles json.RawMessage, parallel int) (Snapshot, error) {
	s := Snapshot{RequestID: req.RequestID, Metadata: req.Metadata, Version: 1, Definitions: all, Hashes: map[string]string{}, Inputs: req.Inputs, Event: req.Event, SecretRefs: req.SecretRefs, Roles: roles, Parallel: parallel}
	s.Execution = req.Execution
	s.ActionContracts = req.actionContracts
	for k, w := range all {
		b, err := json.Marshal(w)
		if err != nil {
			return s, err
		}
		h := sha256.Sum256(b)
		s.Hashes[k] = hex.EncodeToString(h[:])
	}
	for k := range req.Secrets {
		s.RequiredSecrets = append(s.RequiredSecrets, k)
	}
	return s, nil
}
func addEvent(ctx context.Context, tx *sql.Tx, id, kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(run_id,time,type,data) VALUES(?,?,?,?)", id, time.Now().UTC().Format(time.RFC3339Nano), kind, data)
	return err
}
func (s *SQLiteStore) Admit(ctx context.Context, r *Run, snap Snapshot) error {
	if r.Generation == 0 {
		r.Generation = 1
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO runs(id,status,record,snapshot,updated,origin_id,generation,owner,admitted) VALUES(?,?,?,?,?,?,?,?,?)", r.ID, r.Status, b, p, time.Now().UTC().Format(time.RFC3339Nano), originID(r, snap), r.Generation, r.Owner, time.Now().UTC().UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO executions(run_id,generation,retry_uncertain,admitted,status,record) VALUES(?,?,0,?,?,?)", r.ID, r.Generation, time.Now().UTC().UnixNano(), r.Status, b); err != nil {
		return err
	}
	if snap.RequestID != "" {
		if _, err = tx.ExecContext(ctx, "INSERT INTO admission_requests VALUES(?,?)", snap.RequestID, r.ID); err != nil {
			return err
		}
	}
	if err = addEvent(ctx, tx, r.ID, "run.admitted", map[string]any{"workflow_ref": r.WorkflowRef}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SQLiteStore) Save(ctx context.Context, r *Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	// Publish final outcomes under the same transaction as input controls. A
	// control acknowledged after the executor's last observation must still win.
	final := *r
	if final.Status != RunStatusRunning && final.Status != RunStatusQueued && final.Status != RunStatusNeedsAttention {
		var origin string
		if err = tx.QueryRowContext(ctx, "SELECT origin_id FROM runs WHERE id=?", r.ID).Scan(&origin); err != nil {
			return err
		}
		control, controlErr := inputControlTx(ctx, tx, origin)
		if controlErr != nil {
			return controlErr
		}
		if control.Canceled {
			final.Status = RunStatusCanceled
			final.FinishedAt = time.Now().UTC()
		} else if origin == r.ID && control.Paused && (final.FailureClass == "" || final.FailureClass == "canceled") {
			final.Status = RunStatusInterrupted
			final.Error, final.FailureClass = "", ""
			final.FinishedAt = time.Time{}
		}
	}
	if err = saveRunTx(ctx, tx, &final); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	*r = final
	return nil
}

// saveRunTx lets recovery publish its outcome and continuation intent together.
func saveRunTx(ctx context.Context, tx *sql.Tx, r *Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	owner := r.Owner
	if r.Status != RunStatusRunning && r.Status != RunStatusQueued {
		owner = ""
	}
	res, err := tx.ExecContext(ctx, "UPDATE runs SET status=?,record=?,updated=?,owner=? WHERE id=? AND generation=? AND owner=?", r.Status, b, time.Now().UTC().Format(time.RFC3339Nano), owner, r.ID, r.Generation, r.Owner)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("run %s is absent or execution ownership changed", r.ID)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE executions SET status=?,record=? WHERE run_id=? AND generation=?", r.Status, b, r.ID, r.Generation); err != nil {
		return err
	}
	if err = addEvent(ctx, tx, r.ID, "run."+r.Status, r); err != nil {
		return err
	}
	r.Owner = owner
	return nil
}
func (s *SQLiteStore) Load(ctx context.Context, id string) (*Run, Snapshot, error) {
	var b, p []byte
	var r Run
	var snap Snapshot
	if err := s.db.QueryRowContext(ctx, "SELECT record,snapshot,generation,owner FROM runs WHERE id=?", id).Scan(&b, &p, &r.Generation, &r.Owner); err != nil {
		return nil, snap, err
	}
	if err := decodeJSONWithNumbers(b, &r); err != nil {
		return nil, snap, err
	}
	if err := decodeJSONWithNumbers(p, &snap); err != nil {
		return nil, snap, err
	}
	r.OriginID = nativeString(snap.Metadata, "origin_run_id")
	if r.Version > 1 {
		return nil, snap, fmt.Errorf("unsupported result version %d", r.Version)
	}
	if snap.Version != 1 {
		return nil, snap, fmt.Errorf("unsupported snapshot version %d", snap.Version)
	}
	return &r, snap, nil
}
func (s *SQLiteStore) Begin(ctx context.Context, run, job, step string, iteration int, retry bool) (Attempt, bool, error) {
	return s.BeginInput(ctx, run, job, step, iteration, retry, "", false, "")
}
func (s *SQLiteStore) BeginInput(ctx context.Context, run, job, step string, iteration int, retry bool, fingerprint string, replaySafe bool, logical string) (Attempt, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, false, err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return Attempt{}, false, err
	}
	var b []byte
	var last Attempt
	if logical == "" {
		err = tx.QueryRowContext(ctx, "SELECT record FROM attempts WHERE run_id=? AND job_id=? AND step_id=? AND iteration=? ORDER BY number DESC LIMIT 1", run, job, step, iteration).Scan(&b)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT a.record FROM attempts a JOIN runs r ON r.id=a.run_id WHERE json_extract(CAST(r.snapshot AS TEXT),'$.metadata.iteration_logical_run_id')=? AND a.job_id=? AND a.step_id=? AND a.iteration=? ORDER BY a.rowid DESC LIMIT 1`, logical, job, step, iteration).Scan(&b)
	}
	if err == nil {
		if err = decodeJSONWithNumbers(b, &last); err != nil {
			return last, false, err
		}
		if last.Status == RunStatusSucceeded || last.Status == RunStatusSkipped {
			if fingerprint == "" || last.InputFingerprint == fingerprint {
				return last, true, nil
			}
			if !replaySafe {
				return last, false, fmt.Errorf("%w: completed effect input changed for %s/%s", ErrActionIntegrity, job, step)
			}
		}
		if logical != "" && last.RunID != run && !replaySafe {
			return last, false, fmt.Errorf("%w: incomplete item effect %s/%s requires application reconciliation before fresh physical replay", ErrActionIntegrity, job, step)
		}
		if !retry && !schemaPhase(last.Phase) && last.Status != RunStatusSucceeded && last.Status != RunStatusSkipped {
			return last, false, fmt.Errorf("step %s/%s has %s attempt %s; explicit --retry-uncertain is required", job, step, last.Status, last.ID)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return last, false, err
	}
	a := Attempt{InputFingerprint: fingerprint, ID: newID(), RunID: run, JobID: job, StepID: step, Iteration: iteration, Number: last.Number + 1, Status: RunStatusRunning, StartedAt: time.Now().UTC()}
	if schemaPhase(last.Phase) {
		a.Phase = last.Phase
		a.TaskAttemptID = last.TaskAttemptID
		a.TaskOutputs = last.TaskOutputs
		a.RepairCount = last.RepairCount
		a.RepairOutputs = last.RepairOutputs
	}
	b, err = json.Marshal(a)
	if err != nil {
		return a, false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)", a.ID, run, job, step, iteration, a.Number, a.Status, b); err != nil {
		return a, false, err
	}
	if err = addEvent(ctx, tx, run, "step.admitted", a); err != nil {
		return a, false, err
	}
	return a, false, tx.Commit()
}
func (s *SQLiteStore) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	a.Result = result
	a.Status = result.Status
	a.FinishedAt = time.Now().UTC()
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	for _, name := range []string{"stdout", "stderr"} {
		if output, ok := result.Outputs[name].(string); ok {
			h := sha256.Sum256([]byte(output))
			artifactID := a.ID + "-" + name
			if _, err = tx.ExecContext(ctx, "INSERT INTO artifacts VALUES(?,?,?,?,?,?)", artifactID, a.RunID, a.ID, name, hex.EncodeToString(h[:]), []byte(output)); err != nil {
				return err
			}
		}
	}
	res, err := tx.ExecContext(ctx, "UPDATE attempts SET status=?,record=? WHERE id=? AND status='running'", a.Status, b, a.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("attempt %s is no longer running", a.ID)
	}
	if err = addEvent(ctx, tx, a.RunID, "step."+a.Status, a); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SQLiteStore) Attempts(ctx context.Context, id string) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT record FROM attempts WHERE run_id IN (SELECT id FROM runs WHERE id=? OR origin_id=?) ORDER BY rowid", id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var b []byte
		var a Attempt
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) List(ctx context.Context) ([]Run, error) {
	return s.listRecords(ctx, "SELECT record,generation,owner FROM runs ORDER BY updated DESC")
}
func (s *SQLiteStore) listRecords(ctx context.Context, query string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var b []byte
		var r Run
		if err = rows.Scan(&b, &r.Generation, &r.Owner); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) Events(ctx context.Context, id string, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT sequence,run_id,time,type,data FROM events WHERE run_id IN (SELECT id FROM runs WHERE id=? OR origin_id=?) AND sequence>? ORDER BY sequence LIMIT 1000", id, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e := Event{Version: 1, RunID: id}
		var stamp string
		if err = rows.Scan(&e.Sequence, &e.RunID, &stamp, &e.Type, &e.Data); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, stamp)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Recover is called only while holding exclusive workspace ownership. It never
// replays effects or claims a process is dead based on a reused PID.
func (s *SQLiteStore) Recover(ctx context.Context) error {
	if err := s.RecoverToolCalls(ctx); err != nil {
		return err
	}
	runs, err := s.listRecords(ctx, "SELECT record,generation,owner FROM runs WHERE status='running'")
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Status == RunStatusRunning {
			r.Status = RunStatusNeedsAttention
			r.FailureClass = "owner_loss"
			canceled, err := s.CancellationRequested(ctx, r.ID)
			if err != nil {
				return err
			}
			if canceled {
				var pending int
				if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM invocations WHERE run_id=? AND state!='cleaned'", r.ID).Scan(&pending); err != nil {
					return err
				}
				if pending == 0 {
					r.Status = RunStatusCanceled
				}
			}
			r.Error = "execution owner stopped; inspect workspace and attempts before explicit resume"
			if err = s.Save(ctx, &r); err != nil {
				return err
			}
		}
	}
	return s.recoverInputWaits(ctx)
}
func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	if err := s.CheckInvocationCleanup(ctx, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, retentionDeleteRun+" AND NOT EXISTS(SELECT 1 FROM runs origin WHERE origin.id=runs.origin_id AND origin.status IN ('waiting','interrupted'))", id, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("run is absent or still needs recovery")
	}
	return nil
}

// Claim atomically transfers a suspended/queued run into execution. A second
// contender cannot launch effects even if it loaded the same prior checkpoint.
func (s *SQLiteStore) Claim(ctx context.Context, r *Run, previous string) error {
	oldOwner := r.Owner
	r.Owner = newID()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE runs SET status=?,record=?,updated=?,owner=? WHERE id=? AND status=? AND generation=? AND owner=?", r.Status, b, time.Now().UTC().Format(time.RFC3339Nano), r.Owner, r.ID, previous, r.Generation, oldOwner)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("run %s was claimed by another execution owner", r.ID)
	}
	if err = addEvent(ctx, tx, r.ID, "run.claimed", map[string]string{"previous_status": previous}); err != nil {
		return err
	}
	return tx.Commit()
}

type Artifact struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Size      int    `json:"size"`
}

func (s *SQLiteStore) Artifacts(ctx context.Context, run string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,run_id,attempt_id,name,sha256,length(data) FROM artifacts WHERE run_id IN (SELECT id FROM runs WHERE id=? OR origin_id=?) ORDER BY rowid", run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		a := Artifact{RunID: run}
		if err = rows.Scan(&a.ID, &a.RunID, &a.AttemptID, &a.Name, &a.SHA256, &a.Size); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) Artifact(ctx context.Context, run, id string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT data FROM artifacts WHERE run_id IN (SELECT id FROM runs WHERE id=? OR origin_id=?) AND id=?", run, run, id).Scan(&data)
	return data, err
}

func (s *SQLiteStore) AdmissionRequest(ctx context.Context, requestID string) (*Run, Snapshot, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT run_id FROM admission_requests WHERE request_id=?", requestID).Scan(&id); err != nil {
		return nil, Snapshot{}, err
	}
	return s.Load(ctx, id)
}

var ErrStaleExecution = errors.New("execution generation changed")

func (s *SQLiteStore) RequestCancel(ctx context.Context, id string) error {
	return s.RequestCancelGeneration(ctx, id, 0)
}

// RequestCancelGeneration fences delayed requests against a later resume. Zero
// selects the current generation for compatibility callers.
func (s *SQLiteStore) RequestCancelGeneration(ctx context.Context, id string, generation int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM runs WHERE id=?", id).Scan(&current); err != nil {
		return err
	}
	if generation != 0 && current != generation {
		return ErrStaleExecution
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO cancellations(run_id,requested_at) VALUES(?,?) ON CONFLICT DO NOTHING", id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE executions SET cancellation_requested=1 WHERE run_id=? AND generation=(SELECT generation FROM runs WHERE id=?)", id, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO input_controls(run_id,canceled) VALUES(?,1) ON CONFLICT(run_id) DO UPDATE SET canceled=1,continuation=0", id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM input_gates WHERE origin_id=? AND state IN ('pending','accepted')", id)
	if err != nil {
		return err
	}
	var gates []string
	for rows.Next() {
		var gate string
		if err = rows.Scan(&gate); err != nil {
			rows.Close()
			return err
		}
		gates = append(gates, gate)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, gate := range gates {
		g, loadErr := loadInput(ctx, tx, gate)
		if loadErr != nil {
			return loadErr
		}
		inputObservation(&g, "canceled", current)
		if err = writeInput(ctx, tx, g); err != nil {
			return err
		}
	}
	// Suspended children have no active effects to stop. Retire them with their
	// gates so retention cannot remain blocked by an unreachable waiting child.
	rows, err = tx.QueryContext(ctx, "SELECT record,generation,owner FROM runs WHERE origin_id=? AND status IN ('waiting','interrupted')", id)
	if err != nil {
		return err
	}
	var suspended []Run
	for rows.Next() {
		var r Run
		var b []byte
		if err = rows.Scan(&b, &r.Generation, &r.Owner); err == nil {
			err = decodeJSONWithNumbers(b, &r)
		}
		if err != nil {
			rows.Close()
			return err
		}
		suspended = append(suspended, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range suspended {
		r.Status = RunStatusCanceled
		r.FinishedAt = time.Now().UTC()
		if err = saveRunTx(ctx, tx, &r); err != nil {
			return err
		}
	}
	if err = addEvent(ctx, tx, id, "run.cancellation_requested", map[string]string{"id": id}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *SQLiteStore) CancellationRequested(ctx context.Context, id string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM cancellations WHERE run_id=?", id).Scan(&count)
	return count > 0, err
}

func schemaPhase(phase string) bool {
	return phase == "task_completed" || phase == "repair_admitted" || phase == "repair_completed"
}
func (s *SQLiteStore) RecordAttempt(ctx context.Context, a Attempt) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE attempts SET record=? WHERE id=? AND run_id=? AND status='running'", b, a.ID, a.RunID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("attempt %s is no longer running", a.ID)
	}
	if err = addEvent(ctx, tx, a.RunID, "step."+a.Phase, a); err != nil {
		return err
	}
	return tx.Commit()
}

// Configure connection-local safety settings on every driver connection,
// including replacements after an I/O error or an idle-connection eviction.
func sqliteDSN(path string, readOnly bool) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Add("_pragma", "synchronous(FULL)")
	}
	u := url.URL{Scheme: "file", Path: absolute, RawQuery: query.Encode()}
	return u.String(), nil
}
