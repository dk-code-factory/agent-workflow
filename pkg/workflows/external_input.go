package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const DurableExternalInputV1 = "durable-external-input-v1"
const RunStatusWaiting = "waiting"
const RunStatusInterrupted = "interrupted"

var ErrExternalInput = errors.New("external input conflict")

// ErrExternalInputPaused identifies a durable pause that prevented consumption
// of an accepted answer. It wraps ErrExternalInput. Journals may wrap this signal;
// joining a genuine failure preserves that failure instead of suspending it.
var ErrExternalInputPaused = fmt.Errorf("%w: execution paused", ErrExternalInput)

// ErrExternalInputCanceled identifies a durable Cancel that alone prevented
// answer consumption. It wraps ErrExternalInput; journals may wrap the signal,
// while joined genuine failures must remain observable.
var ErrExternalInputCanceled = fmt.Errorf("%w: execution canceled", ErrExternalInput)

// ExternalInputAnswer is data interpreted and validated by the host application.
// AnswerID is unique across the journal, including canceled and consumed gates.
type ExternalInputAnswer struct {
	AnswerID   string          `json:"answer_id"`
	EvidenceID string          `json:"evidence_id"`
	Value      json.RawMessage `json:"value"`
}
type ExternalInputObservation struct {
	State      string    `json:"state"`
	Time       time.Time `json:"time"`
	Generation int64     `json:"execution_generation"`
}

// ExternalInputGate retains the immutable request and its complete lifecycle.
// RunID identifies the physical child; OriginID is the controller handle.
type ExternalInputGate struct {
	ID          string                     `json:"gate_id"`
	OriginID    string                     `json:"origin_run_id"`
	RunID       string                     `json:"run_id"`
	Generation  int64                      `json:"execution_generation"`
	JobID       string                     `json:"job_id"`
	StepID      string                     `json:"step_id"`
	Iteration   int                        `json:"iteration"`
	EvidenceID  string                     `json:"evidence_id"`
	Prompt      string                     `json:"prompt"`
	Payload     json.RawMessage            `json:"payload,omitempty"`
	Fingerprint string                     `json:"fingerprint"`
	State       string                     `json:"state"`
	Answer      *ExternalInputAnswer       `json:"answer,omitempty"`
	History     []ExternalInputObservation `json:"history"`
}
type ExternalInputControl struct {
	Paused       bool `json:"paused"`
	Canceled     bool `json:"canceled"`
	Continuation bool `json:"continuation_requested"`
}

// ExternalInputJournal is optional for existing stores. Workflows containing
// input/wait require it before admission. Writes must fence the current owner;
// acceptance and continuation intent must commit in one transaction. New effect
// admissions must reject durable Cancel; completed results/checkpoints and
// cleanup persistence remain writable after cancellation.
type ExternalInputJournal interface {
	Journal
	EnsureInputGate(context.Context, ExternalInputGate) (ExternalInputGate, error)
	Inputs(context.Context, string) ([]ExternalInputGate, error)
	AcceptInput(context.Context, string, string, ExternalInputAnswer) (ExternalInputGate, error)
	// ConsumeInput returns ErrExternalInputPaused or ErrExternalInputCanceled
	// when a durable pause or Cancel alone prevents consumption. Pause preserves
	// accepted state; Cancel preserves retained answer/history while invalidating
	// its gate. Return genuine failures too if control alone did not explain denial.
	ConsumeInput(context.Context, string) (ExternalInputGate, error)
	InputControl(context.Context, string) (ExternalInputControl, error)
	// QueueResume controls the root handle: unanswered gates remain suspended
	// without a new generation; safe admission atomically clears pause and creates
	// one execution after cancellation and retry-acknowledgement guards. Empty
	// request identity and zero queue limit support embedded Executor.Resume.
	QueueResume(context.Context, string, string, bool, int) (*Run, error)
}

func externalInputJournal(store RunStore) ExternalInputJournal {
	if r, ok := store.(redactedStore); ok {
		return externalInputJournal(r.Journal)
	}
	j, _ := store.(ExternalInputJournal)
	return j
}
func hasInputWait(all map[string]*Workflow) bool {
	for _, w := range all {
		for _, j := range w.Jobs {
			for _, s := range j.Steps {
				if s.Uses == "input/wait" {
					return true
				}
			}
		}
	}
	return false
}
func inputFingerprint(g ExternalInputGate) string {
	return actionHash([]any{g.ID, g.OriginID, g.RunID, g.JobID, g.StepID, g.Iteration, g.EvidenceID, g.Prompt, g.Payload})
}
func inputObservation(g *ExternalInputGate, state string, generation int64) {
	g.State = state
	g.History = append(g.History, ExternalInputObservation{state, time.Now().UTC(), generation})
}
func writeInput(ctx context.Context, tx *sql.Tx, g ExternalInputGate) error {
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	var answerID any
	if g.Answer != nil {
		answerID = g.Answer.AnswerID
	}
	_, err = tx.ExecContext(ctx, `UPDATE input_gates SET state=?,answer_id=?,record=? WHERE id=?`, g.State, answerID, b, g.ID)
	if err != nil {
		return err
	}
	return addEvent(ctx, tx, g.OriginID, "input."+g.State, g)
}
func loadInput(ctx context.Context, tx *sql.Tx, id string) (g ExternalInputGate, err error) {
	var b []byte
	err = tx.QueryRowContext(ctx, "SELECT record FROM input_gates WHERE id=?", id).Scan(&b)
	if err == nil {
		err = decodeJSONWithNumbers(b, &g)
	}
	if err == nil && g.Fingerprint != inputFingerprint(g) {
		err = fmt.Errorf("%w: retained input request changed", ErrActionIntegrity)
	}
	return
}
func inputControlTx(ctx context.Context, tx *sql.Tx, origin string) (c ExternalInputControl, err error) {
	err = tx.QueryRowContext(ctx, "SELECT paused,canceled,continuation FROM input_controls WHERE run_id=?", origin).Scan(&c.Paused, &c.Canceled, &c.Continuation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
func (s *SQLiteStore) InputControl(ctx context.Context, origin string) (c ExternalInputControl, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT paused,canceled,continuation FROM input_controls WHERE run_id=?", origin).Scan(&c.Paused, &c.Canceled, &c.Continuation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
func (s *SQLiteStore) EnsureInputGate(ctx context.Context, g ExternalInputGate) (ExternalInputGate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return g, err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return g, err
	}
	var origin string
	if err = tx.QueryRowContext(ctx, "SELECT origin_id FROM runs WHERE id=?", g.RunID).Scan(&origin); err != nil {
		return g, err
	}
	if origin != g.OriginID || g.ID != actionHash([]any{g.RunID, g.JobID, g.StepID, g.Iteration}) {
		return g, fmt.Errorf("%w: invalid gate identity", ErrExternalInput)
	}
	c, err := inputControlTx(ctx, tx, origin)
	if err != nil {
		return g, err
	}
	var canceled int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM cancellations WHERE run_id=?", origin).Scan(&canceled); err != nil {
		return g, err
	}
	if c.Canceled || canceled > 0 {
		return g, ErrExternalInputCanceled
	}
	if strings.TrimSpace(g.Prompt) == "" || strings.TrimSpace(g.EvidenceID) == "" {
		return g, fmt.Errorf("input/wait requires nonempty prompt and evidence-id")
	}
	g.Fingerprint = inputFingerprint(g)
	old, err := loadInput(ctx, tx, g.ID)
	if err == nil {
		if old.Fingerprint != g.Fingerprint {
			return g, fmt.Errorf("%w: input request/evidence changed; start a new run", ErrActionIntegrity)
		}
		if old.State == "canceled" || old.State == "replaced" {
			return g, fmt.Errorf("%w: gate %s", ErrExternalInput, old.State)
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return g, err
	}
	inputObservation(&g, "pending", g.Generation)
	b, err := json.Marshal(g)
	if err != nil {
		return g, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO input_controls(run_id) VALUES(?) ON CONFLICT DO NOTHING", origin); err != nil {
		return g, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO input_gates(id,origin_id,run_id,state,record) VALUES(?,?,?,?,?)", g.ID, origin, g.RunID, g.State, b); err != nil {
		return g, err
	}
	if err = addEvent(ctx, tx, origin, "input.pending", g); err != nil {
		return g, err
	}
	return g, tx.Commit()
}

// Resolve a physical child through its immutable origin before filtering its
// gates. Both root and child inspection use the existing origin index.
const inputInspectionQuery = `SELECT record FROM input_gates WHERE origin_id=COALESCE((SELECT origin_id FROM runs WHERE id=?),?) AND (origin_id=? OR run_id=?) ORDER BY rowid`

func (s *SQLiteStore) Inputs(ctx context.Context, run string) ([]ExternalInputGate, error) {
	rows, err := s.db.QueryContext(ctx, inputInspectionQuery, run, run, run, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalInputGate{}
	for rows.Next() {
		var g ExternalInputGate
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = decodeJSONWithNumbers(b, &g); err != nil {
			return nil, err
		}
		if g.Fingerprint != inputFingerprint(g) {
			return nil, fmt.Errorf("%w: input request changed", ErrActionIntegrity)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) AcceptInput(ctx context.Context, origin, id string, a ExternalInputAnswer) (ExternalInputGate, error) {
	var g ExternalInputGate
	if strings.TrimSpace(a.AnswerID) == "" || len(a.AnswerID) > 128 || strings.TrimSpace(a.EvidenceID) == "" || !json.Valid(a.Value) || strings.TrimSpace(string(a.Value)) == "null" {
		return g, fmt.Errorf("%w: answer_id, evidence_id and non-null JSON value are required", ErrExternalInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return g, err
	}
	defer tx.Rollback()
	g, err = loadInput(ctx, tx, id)
	if err != nil {
		return g, err
	}
	c, err := inputControlTx(ctx, tx, origin)
	if err != nil {
		return g, err
	}
	var canceled int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM cancellations WHERE run_id=?", origin).Scan(&canceled); err != nil {
		return g, err
	}
	if g.OriginID != origin || g.State != "pending" || g.EvidenceID != a.EvidenceID || c.Canceled || canceled > 0 {
		return g, fmt.Errorf("%w: duplicate, stale, or canceled gate/evidence", ErrExternalInput)
	}
	var used int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM input_gates WHERE answer_id=?", a.AnswerID).Scan(&used); err != nil {
		return g, err
	}
	if used > 0 {
		return g, fmt.Errorf("%w: answer identity already accepted", ErrExternalInput)
	}
	g.Answer = &a
	inputObservation(&g, "accepted", g.Generation)
	if err = writeInput(ctx, tx, g); err != nil {
		return g, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE input_controls SET continuation=1 WHERE run_id=?", origin); err != nil {
		return g, err
	}
	return g, tx.Commit()
}
func (s *SQLiteStore) ConsumeInput(ctx context.Context, id string) (ExternalInputGate, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExternalInputGate{}, err
	}
	defer tx.Rollback()
	if err = checkFence(ctx, tx); err != nil {
		return ExternalInputGate{}, err
	}
	g, err := loadInput(ctx, tx, id)
	if err != nil {
		return g, err
	}
	c, err := inputControlTx(ctx, tx, g.OriginID)
	if err != nil {
		return g, err
	}
	if c.Canceled {
		return g, ErrExternalInputCanceled
	}
	if c.Paused {
		return g, ErrExternalInputPaused
	}
	if g.State == "consumed" {
		return g, nil
	}
	if g.State != "accepted" || g.Answer == nil {
		return g, fmt.Errorf("%w: gate has no accepted answer", ErrExternalInput)
	}
	var generation int64
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM runs WHERE id=?", g.OriginID).Scan(&generation); err != nil {
		return g, err
	}
	inputObservation(&g, "consumed", generation)
	if err = writeInput(ctx, tx, g); err != nil {
		return g, err
	}
	return g, tx.Commit()
}

// InterruptInput preserves all gates/answers and fences automatic continuation.
// The controller stops and joins any active execution after this commit.
func (s *SQLiteStore) InterruptInput(ctx context.Context, origin string, generation int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, actualOrigin string
	var current int64
	if err = tx.QueryRowContext(ctx, "SELECT status,origin_id,generation FROM runs WHERE id=?", origin).Scan(&status, &actualOrigin, &current); err != nil {
		return err
	}
	if actualOrigin != origin || generation != current {
		return ErrStaleExecution
	}
	if status != RunStatusRunning && status != RunStatusQueued && status != RunStatusWaiting && status != RunStatusInterrupted && status != RunStatusNeedsAttention {
		return fmt.Errorf("%w: run is terminal", ErrExternalInput)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO input_controls(run_id,paused) VALUES(?,1) ON CONFLICT(run_id) DO UPDATE SET paused=1", origin); err != nil {
		return err
	}
	if status != RunStatusRunning {
		var b []byte
		var run Run
		if err = tx.QueryRowContext(ctx, "SELECT record FROM runs WHERE id=?", origin).Scan(&b); err != nil {
			return err
		}
		if err = decodeJSONWithNumbers(b, &run); err != nil {
			return err
		}
		run.Status = RunStatusInterrupted
		run.FinishedAt = time.Time{}
		b, err = json.Marshal(run)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE runs SET status=?,record=? WHERE id=?", run.Status, b, origin); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE executions SET status=?,record=? WHERE run_id=? AND generation=?", run.Status, b, origin, current); err != nil {
			return err
		}
	}
	if err = addEvent(ctx, tx, origin, "run.interrupted", map[string]any{"execution_generation": current}); err != nil {
		return err
	}
	return tx.Commit()
}

// QueueInputContinuations admits durable continuation intents when no controller
// owns an execution. Controllers use QueueInputContinuationsExcept during cleanup.
func (s *SQLiteStore) QueueInputContinuations(ctx context.Context, maxQueued int) error {
	return s.QueueInputContinuationsExcept(ctx, maxQueued)
}

// The predicate matches the additive v10 partial index. Gate completeness is
// checked during discovery/admission; partially answered origins can still cost
// work proportional to their eligible-intent history.
const inputContinuationIntentProbe = `SELECT EXISTS(SELECT 1 FROM input_controls c CROSS JOIN runs r ON r.id=c.run_id WHERE c.continuation=1 AND c.paused=0 AND c.canceled=0 AND r.status='waiting' AND r.owner=''`

// QueueInputContinuationsExcept excludes active controller owners until cleanup
// finishes, even after the executor has published waiting and released its lease.
func (s *SQLiteStore) QueueInputContinuationsExcept(ctx context.Context, maxQueued int, active ...string) error {
	// A full queue must not discover waiting origins while controller controls
	// are blocked behind its ownership mutex. Zero retains unlimited admission.
	available := -1
	if maxQueued > 0 {
		queued, err := s.QueueDepth(ctx)
		if err != nil {
			return err
		}
		available = maxQueued - queued
		if available <= 0 {
			return nil
		}
	}
	args := make([]any, 0, len(active)+1)
	exclusions := ""
	if len(active) > 0 {
		exclusions = " AND r.id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(active)), ",") + ")"
		for _, id := range active {
			args = append(args, id)
		}
	}
	// Start from the sparse eligibility index, not accumulated zero-intent waits.
	// CROSS JOIN keeps eligible controls outermost even with an empty index. Older
	// v10 stores without this additive index remain functional until owned Migrate.
	var eligible bool
	probe := inputContinuationIntentProbe + exclusions + ")"
	if err := s.db.QueryRowContext(ctx, probe, args...).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	query := `SELECT r.id FROM runs r JOIN input_controls c ON c.run_id=r.id WHERE r.status='waiting' AND r.owner='' AND c.continuation=1 AND c.paused=0 AND c.canceled=0 AND NOT EXISTS(SELECT 1 FROM input_gates g WHERE g.origin_id=r.id AND g.state IN ('pending','canceled','replaced'))`
	query += exclusions + " ORDER BY r.admitted,r.id LIMIT ?"
	args = append(args, available)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = s.queueResume(ctx, id, "", false, maxQueued, true); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) executeInputWait(ctx context.Context, step Step, ec expressionContext) (StepExecution, error) {
	result := StepExecution{ID: step.ID, Status: RunStatusWaiting}
	args, err := renderMap(step.With, ec)
	if err != nil {
		return result, err
	}
	raw, err := e.actionIdentityJSON(mustInputJSON(args))
	if err != nil {
		return result, err
	}
	if err = decodeJSONWithNumbers(raw, &args); err != nil {
		return result, err
	}
	prompt, pok := args["prompt"].(string)
	evidence, eok := args["evidence-id"].(string)
	if !pok || !eok || strings.TrimSpace(prompt) == "" || strings.TrimSpace(evidence) == "" {
		return result, fmt.Errorf("input/wait requires string prompt and evidence-id")
	}
	origin := nativeString(ec.Metadata, "origin_run_id")
	if origin == "" {
		origin = ec.RunID
	}
	generation := int64(1)
	if f, ok := ctx.Value(fenceKey{}).(executionFence); ok {
		generation = f.generation
	}
	g := ExternalInputGate{ID: actionHash([]any{ec.RunID, ec.JobID, step.ID, ec.IterationNumber}), OriginID: origin, RunID: ec.RunID, Generation: generation, JobID: ec.JobID, StepID: step.ID, Iteration: ec.IterationNumber, EvidenceID: evidence, Prompt: prompt}
	if p, ok := args["payload"]; ok {
		g.Payload, err = actionJSON(p)
		if err != nil {
			return result, err
		}
	}
	j := externalInputJournal(e.Store)
	if j == nil {
		return result, fmt.Errorf("input/wait requires %s", DurableExternalInputV1)
	}
	g, err = j.EnsureInputGate(ctx, g)
	if err != nil {
		return result, err
	}
	result.Outputs = map[string]any{"gate_id": g.ID, "evidence_id": g.EvidenceID}
	if g.State == "pending" {
		return result, nil
	}
	// All retained application evidence is validated before consumption, even for
	// an answer accepted during the current owner's final cleanup.
	if err = e.validateActionRecovery(ctx, origin, Snapshot{ActionContracts: e.actionContracts}); err != nil {
		return result, err
	}
	g, err = j.ConsumeInput(ctx, g.ID)
	if onlyError(err, ErrExternalInputPaused) || onlyError(err, ErrExternalInputCanceled) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	// Cancel may commit after consumption but before the journal returns it.
	// Preserve the consumed checkpoint while stopping further effect admission.
	if err = e.checkEffectAdmission(ctx, origin); onlyError(err, ErrExternalInputCanceled) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	var value any
	if g.Answer == nil {
		return result, fmt.Errorf("%w: accepted answer missing", ErrActionIntegrity)
	}
	if err = decodeJSONWithNumbers(g.Answer.Value, &value); err != nil {
		return result, err
	}
	result.Status = RunStatusSucceeded
	result.Outputs["answer"] = value
	result.Outputs["answer_id"] = g.Answer.AnswerID
	return result, nil
}
func mustInputJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (e *Executor) validateInputRecovery(ctx context.Context, origin string) error {
	j := externalInputJournal(e.Store)
	if j == nil {
		return nil
	}
	gates, err := j.Inputs(ctx, origin)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, g := range gates {
		if g.State == "canceled" || g.State == "replaced" {
			return fmt.Errorf("%w: gate %s is %s; start a new run", ErrActionIntegrity, g.ID, g.State)
		}
		if seen[g.RunID] {
			continue
		}
		seen[g.RunID] = true
		_, snap, err := j.Load(ctx, g.RunID)
		if err != nil {
			return err
		}
		for ref, w := range snap.Definitions {
			if actionHash(w) != snap.Hashes[ref] {
				return fmt.Errorf("%w: input workflow snapshot changed", ErrActionIntegrity)
			}
		}
	}
	return nil
}

// recoverInputWaits runs only under exclusive workspace ownership, after
// generic owner-loss recovery. Safe lifecycle cancellation also retains its
// gate, while explicit Cancel and uncertain physical effects remain blocked.
func (s *SQLiteStore) recoverInputWaits(ctx context.Context) error {
	runs, err := s.listRecords(ctx, `SELECT r.record,r.generation,r.owner FROM runs r WHERE r.status IN ('needs_attention','canceled') AND EXISTS(SELECT 1 FROM input_gates g WHERE g.origin_id=r.id) AND r.origin_id=r.id`)
	if err != nil {
		return err
	}
	for _, r := range runs {
		ownerLost := r.Status == RunStatusNeedsAttention && r.FailureClass == "owner_loss"
		lifecycleCanceled := r.Status == RunStatusCanceled && r.FailureClass == "canceled"
		if !ownerLost && !lifecycleCanceled {
			continue
		}
		if err = s.CheckInvocationCleanup(ctx, r.ID); err != nil {
			continue
		}
		if err = s.CheckToolEffects(ctx, r.ID); err != nil {
			continue
		}
		var uncertain int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE run_id IN (SELECT id FROM runs WHERE origin_id=?) AND status NOT IN ('succeeded','skipped') AND NOT EXISTS(SELECT 1 FROM action_attempts a WHERE a.attempt_id=attempts.id)`, r.ID).Scan(&uncertain); err != nil {
			return err
		}
		if uncertain > 0 {
			continue
		}
		if err = s.recoverInputWait(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) recoverInputWait(ctx context.Context, r Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := inputControlTx(ctx, tx, r.ID)
	if err != nil {
		return err
	}
	var cancellations int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM cancellations WHERE run_id=?", r.ID).Scan(&cancellations); err != nil {
		return err
	}
	if c.Canceled || cancellations > 0 {
		return nil
	}
	r.Status = RunStatusWaiting
	if c.Paused {
		r.Status = RunStatusInterrupted
	}
	r.Error, r.FailureClass = "", ""
	r.FinishedAt = time.Time{}
	if err = saveRunTx(ctx, tx, &r); err != nil {
		return err
	}
	// A crash following consumption can leave intent cleared by queue admission.
	// Reconstruct it atomically with the recoverable suspension checkpoint.
	if _, err = tx.ExecContext(ctx, "UPDATE input_controls SET continuation=1 WHERE run_id=? AND EXISTS(SELECT 1 FROM input_gates WHERE origin_id=? AND state IN ('accepted','consumed'))", r.ID, r.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Executor) childInputWait(ctx context.Context, child *Run, req RunRequest) (bool, error) {
	if child.Status == RunStatusWaiting {
		return true, nil
	}
	j := externalInputJournal(e.Store)
	if j == nil {
		return false, nil
	}
	origin := nativeString(req.Metadata, "origin_run_id")
	if origin == "" {
		origin = req.runID
	}
	gates, err := j.Inputs(ctx, origin)
	if err != nil {
		return false, err
	}
	for _, g := range gates {
		if g.RunID == child.ID {
			return true, nil
		}
		_, snap, err := j.Load(ctx, g.RunID)
		if err != nil {
			return false, err
		}
		ancestors, _ := snap.Metadata["iteration_ancestors"].([]any)
		for _, ancestor := range ancestors {
			if ancestor == child.ID {
				return true, nil
			}
		}
	}
	return false, nil
}
