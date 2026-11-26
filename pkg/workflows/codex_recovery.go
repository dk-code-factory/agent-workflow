package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// ErrManagedCodexProcessLost describes verified loss of the owned native
// process. It is not a cancellation, source rejection or unknown tool receipt.
// Recovery additionally requires a private sealed row and completed cleanup.
var ErrManagedCodexProcessLost = errors.New("owned managed Codex process stopped before its reply")

type managedCodexBatchIdentity struct {
	ThreadID     string   `json:"threadId"`
	TurnID       string   `json:"turnId"`
	InvocationID string   `json:"invocationId"`
	PolicyID     string   `json:"policyId"`
	BuildID      string   `json:"buildId"`
	BatchID      string   `json:"batchId"`
	ResponseID   string   `json:"responseId"`
	CallIDs      []string `json:"callIds"`
}

type managedCodexRecovery struct {
	Admission      *managedCodexAdmission `json:"original_admission"`
	LastGeneration int64                  `json:"last_generation,omitempty"`
	LastOwner      string                 `json:"last_owner,omitempty"`
	Batch          json.RawMessage        `json:"batch"`
	WitnessSHA256  string                 `json:"witness_sha256"`
	Generation     int64                  `json:"original_generation"`
	Owner          string                 `json:"original_owner"`
	State          string                 `json:"state"`
	Terminal       *SessionResult         `json:"terminal,omitempty"`
	TerminalSHA256 string                 `json:"terminal_sha256,omitempty"`
}

type managedCodexRecoveryContext struct{ value managedCodexRecovery }
type managedCodexRecoveryKey struct{}
type managedCodexTerminalKey struct{}
type managedCodexTerminalRecorder func(context.Context, SessionResult, string, string) error

// ManagedCodexRecoveryFrom returns only a private original-message recovery
// installed by the engine after current-owner admission. A provider cannot
// select a batch or install this context through request fields.
func ManagedCodexRecoveryFrom(ctx context.Context) json.RawMessage {
	value, ok := ctx.Value(managedCodexRecoveryKey{}).(managedCodexRecoveryContext)
	if !ok {
		return nil
	}
	return append(json.RawMessage(nil), value.value.Batch...)
}

func decodeManagedCodexBatch(raw []byte) (managedCodexBatchIdentity, error) {
	var b managedCodexBatchIdentity
	if len(raw) > 64<<10 {
		return b, fmt.Errorf("managed recovery identity exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b); err != nil {
		return b, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return b, fmt.Errorf("extra managed recovery identity")
	}
	if b.ThreadID == "" || b.TurnID == "" || b.InvocationID == "" || b.PolicyID == "" || b.BuildID == "" || b.BatchID == "" || b.ResponseID == "" || len(b.CallIDs) > 128 || !sort.StringsAreSorted(b.CallIDs) {
		return b, fmt.Errorf("managed recovery original identity is incomplete")
	}
	for i, id := range b.CallIDs {
		if id == "" || len(id) > 256 || i > 0 && id == b.CallIDs[i-1] {
			return b, fmt.Errorf("managed recovery original calls are invalid")
		}
	}
	return b, nil
}
func managedCodexRecoveryRow(raw []byte) (map[string]json.RawMessage, *managedCodexRecovery, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	var r *managedCodexRecovery
	if value := fields["managed_codex_recovery"]; len(value) > 0 {
		if err := json.Unmarshal(value, &r); err != nil {
			return nil, nil, err
		}
	}
	return fields, r, nil
}
func putManagedCodexRecovery(ctx context.Context, tx *sql.Tx, inv, key string, fields map[string]json.RawMessage, r *managedCodexRecovery) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	fields["managed_codex_recovery"] = raw
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, inv, key)
	return err
}
func managedRecoveryProof(raw []byte, b managedCodexBatchIdentity, digest string) error {
	var row managedCodexUsageRecord
	if err := decodeJSONWithNumbers(raw, &row); err != nil {
		return err
	}
	p := row.Proof
	if p == nil || p.ResponseID != b.ResponseID || p.Digest != digest || p.SessionID != b.ThreadID || p.TurnID != b.TurnID || p.PolicyID != b.PolicyID || p.BuildID != b.BuildID || !bytes.Equal(row.Batch, mustManagedRecoveryJSON(b)) || row.Scope != "invocation" || row.CounterReset {
		return fmt.Errorf("managed recovery differs from original usage witness")
	}
	return nil
}

// SealManagedCodexBatch is called only after the adapter validates the full
// native seal and commits its exact original usage. It durably binds recovery
// before the first broker dispatch. Unknown extension JSON is preserved.
func SealManagedCodexBatch(ctx context.Context, run, inv, digest string, rawBatch json.RawMessage) error {
	if err := CheckManagedCodexAuthority(ctx, run, inv); err != nil {
		return err
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
	if err != nil {
		return err
	}
	batch, err := decodeManagedCodexBatch(rawBatch)
	if err != nil {
		return err
	}
	if batch.InvocationID != inv || len(batch.CallIDs) == 0 {
		return fmt.Errorf("managed recovery seal has no original calls")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return err
	}
	if err = checkEffectAdmission(ctx, tx, run); err != nil {
		return err
	}
	key := "codex-response:" + batch.ResponseID
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, key).Scan(&raw); err != nil {
		return err
	}
	if err = managedRecoveryProof(raw, batch, digest); err != nil {
		return err
	}
	fields, prior, err := managedCodexRecoveryRow(raw)
	if err != nil {
		return err
	}
	admission, err := managedCaptureAdmission(ctx, tx, binding, inv)
	if err != nil {
		return err
	}
	value := &managedCodexRecovery{Admission: admission, Batch: append(json.RawMessage(nil), rawBatch...), WitnessSHA256: digest, Generation: binding.Generation, Owner: binding.Owner, State: "sealed"}
	if prior != nil {
		if prior.State != "sealed" || !bytes.Equal(prior.Batch, rawBatch) || prior.WitnessSHA256 != digest {
			return fmt.Errorf("managed recovery original seal changed")
		}
		if prior.Generation != binding.Generation || prior.Owner != binding.Owner {
			original, ok := ctx.Value(managedCodexRecoveryKey{}).(managedCodexRecoveryContext)
			if !ok || original.value.Generation != prior.Generation || original.value.Owner != prior.Owner || !bytes.Equal(original.value.Batch, prior.Batch) {
				return fmt.Errorf("new execution lacks original managed recovery authority")
			}
		}
		value = prior
	}
	// A later fully sealed native response proves the previous broker batch's
	// completion barrier; the native journal independently verifies it on load.
	rows, err := tx.QueryContext(ctx, `SELECT observation_key,record FROM usage_observations WHERE invocation_id=?`, inv)
	if err != nil {
		return err
	}
	type older struct {
		key    string
		fields map[string]json.RawMessage
		value  *managedCodexRecovery
	}
	var previous []older
	for rows.Next() {
		var otherKey string
		var otherRaw []byte
		if err = rows.Scan(&otherKey, &otherRaw); err != nil {
			break
		}
		if otherKey == key {
			continue
		}
		f, r, e := managedCodexRecoveryRow(otherRaw)
		if e != nil {
			err = e
			break
		}
		if r != nil && r.State == "sealed" {
			r.State = "settled"
			previous = append(previous, older{otherKey, f, r})
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, old := range previous {
		if err = putManagedCodexRecovery(ctx, tx, inv, old.key, old.fields, old.value); err != nil {
			return err
		}
	}
	if err = putManagedCodexRecovery(ctx, tx, inv, key, fields, value); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckpointManagedCodexTerminal uses a private engine closure with original
// message, redaction, limits and current ownership. No provider event can
// install that closure or supply a different message's authority.
func CheckpointManagedCodexTerminal(ctx context.Context, result SessionResult, responseID, digest string) error {
	record, ok := ctx.Value(managedCodexTerminalKey{}).(managedCodexTerminalRecorder)
	if !ok {
		return fmt.Errorf("managed terminal lacks its original engine checkpoint")
	}
	return record(ctx, result, responseID, digest)
}
func managedResultDigest(result SessionResult) (string, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<20 {
		return "", fmt.Errorf("managed terminal checkpoint exceeds bound")
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

// These reads are deliberately conservative. Detection is not admission: the
// scheduler additionally requires a newly owned execution and original running
// mailbox identity, unexpired original deadline, cancellation and cleanup gates.
func managedRecoveryForInvocation(ctx context.Context, tx *sql.Tx, inv string) (*managedCodexRecovery, error) {
	rows, err := tx.QueryContext(ctx, `SELECT observation_key,record FROM usage_observations WHERE invocation_id=?`, inv)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var selected *managedCodexRecovery
	var selectedProof *managedCodexUsageProof
	var bookkeeping *managedCodexAttemptAdmission
	unproven := false
	count := 0
	for rows.Next() {
		count++
		if count > 4097 {
			return nil, fmt.Errorf("managed recovery observation bound exceeded")
		}
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		if key == managedCodexAdmissionKey {
			_, bookkeeping, err = parseManagedCodexAdmission(raw)
			if err != nil {
				return nil, err
			}
			continue
		}
		var observation managedCodexUsageRecord
		if err = decodeJSONWithNumbers(raw, &observation); err != nil {
			return nil, err
		}
		if observation.Proof == nil {
			unproven = true
		}
		_, r, err := managedCodexRecoveryRow(raw)
		if err != nil {
			return nil, err
		}
		if r == nil || r.State == "settled" || r.State == "completed" {
			continue
		}
		if r.State != "sealed" && r.State != "terminal" {
			return nil, fmt.Errorf("managed original recovery is held")
		}
		if selected != nil {
			return nil, fmt.Errorf("ambiguous original managed recovery")
		}
		b, err := decodeManagedCodexBatch(r.Batch)
		if err != nil {
			return nil, err
		}
		if b.InvocationID != inv || r.Generation <= 0 || r.Owner == "" {
			return nil, fmt.Errorf("managed recovery original admission missing")
		}
		if err = managedRecoveryProof(raw, b, r.WitnessSHA256); err != nil {
			return nil, err
		}
		if r.State == "terminal" {
			if r.Terminal == nil {
				return nil, fmt.Errorf("managed terminal result absent")
			}
			digest, err := managedResultDigest(*r.Terminal)
			if err != nil || digest != r.TerminalSHA256 || r.Terminal.Status != RunStatusSucceeded || r.Terminal.SessionID != b.ThreadID {
				return nil, fmt.Errorf("managed terminal result differs from checkpoint")
			}
		}
		selected = r
		selectedProof = observation.Proof
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if selected != nil {
		if unproven {
			return nil, fmt.Errorf("managed recovery has another unknown observation")
		}
		var run string
		if err = tx.QueryRowContext(ctx, `SELECT origin_id FROM usage_invocations WHERE id=?`, inv).Scan(&run); err != nil {
			return nil, err
		}
		if err = validateManagedCodexBookkeeping(bookkeeping, run, inv, selectedProof); err != nil {
			return nil, err
		}
	}
	return selected, nil
}

func managedOriginalDeadline(w Worker, msg WorkerMessage) time.Time {
	if w.TurnDuration <= 0 || msg.StartedAt.IsZero() {
		return time.Time{}
	}
	return msg.StartedAt.Add(w.TurnDuration)
}

func mustManagedRecoveryJSON(value managedCodexBatchIdentity) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func (m *WorkerManager) managedTerminalRecorder(w Worker, msg WorkerMessage, secrets map[string]string) managedCodexTerminalRecorder {
	return func(ctx context.Context, result SessionResult, responseID, digest string) error {
		if err := CheckManagedCodexAuthority(ctx, w.RunID, msg.ID); err != nil {
			return err
		}
		if result.Status != RunStatusSucceeded || result.ProviderStatus != "completed" || result.Outputs["usage_incomplete"] != false {
			return fmt.Errorf("managed checkpoint lacks terminal success")
		}
		totals, err := ManagedCodexUsageTotals(ctx, w.RunID, msg.ID, responseID, digest)
		if err != nil {
			return err
		}
		usage, ok := result.Outputs["usage"].(map[string]any)
		if !ok {
			return fmt.Errorf("managed terminal accounting is missing")
		}
		observed := NormalizeUsage(UsageUpdate{Usage: usage})
		expected := NormalizeUsage(UsageUpdate{Usage: totals})
		left, _ := json.Marshal([]any{observed.InputTokens, observed.CachedInputTokens, observed.OutputTokens})
		right, _ := json.Marshal([]any{expected.InputTokens, expected.CachedInputTokens, expected.OutputTokens})
		if !bytes.Equal(left, right) {
			return fmt.Errorf("managed terminal total differs from original observations")
		}
		result.Outputs, _ = redactValue(result.Outputs, secrets).(map[string]any)
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		limit := w.MaxOutput
		if limit == 0 {
			limit = m.Limits.MaxOutput
		}
		if limit <= 0 || len(encoded) > limit {
			return fmt.Errorf("managed terminal exceeds original output limit")
		}
		resultDigest, err := managedResultDigest(result)
		if err != nil {
			return err
		}
		_, binding, err := managedCodexUsageOwner(ctx, w.RunID, msg.ID)
		if err != nil {
			return err
		}
		tx, err := m.Store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
			return err
		}
		if err = checkEffectAdmission(ctx, tx, w.RunID); err != nil {
			return err
		}
		var messageRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT record FROM messages WHERE id=? AND worker_id=? AND status='running'`, msg.ID, w.ID).Scan(&messageRaw); err != nil {
			return err
		}
		var original WorkerMessage
		if err = decodeJSONWithNumbers(messageRaw, &original); err != nil {
			return err
		}
		if original.Generation != binding.Generation || original.StartedAt != msg.StartedAt {
			return fmt.Errorf("managed terminal original admission changed")
		}
		key := "codex-response:" + responseID
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, msg.ID, key).Scan(&raw); err != nil {
			return err
		}
		var row managedCodexUsageRecord
		if err = decodeJSONWithNumbers(raw, &row); err != nil {
			return err
		}
		batch, err := decodeManagedCodexBatch(row.Batch)
		if err != nil {
			return err
		}
		if len(batch.CallIDs) != 0 || batch.InvocationID != msg.ID || batch.ThreadID != result.SessionID || !strings.Contains(result.ProviderVersion, "agent-workflow-managed-v1/"+batch.BuildID) {
			return fmt.Errorf("managed terminal is not the original no-tool response")
		}
		if err = managedRecoveryProof(raw, batch, digest); err != nil {
			return err
		}
		fields, prior, err := managedCodexRecoveryRow(raw)
		if err != nil {
			return err
		}
		if prior != nil && (prior.State != "terminal" || prior.TerminalSHA256 != resultDigest) {
			return fmt.Errorf("managed terminal checkpoint conflicts")
		}
		admission, err := managedCaptureAdmission(ctx, tx, binding, msg.ID)
		if err != nil {
			return err
		}
		value := &managedCodexRecovery{Admission: admission, Batch: row.Batch, WitnessSHA256: digest, Generation: binding.Generation, Owner: binding.Owner, State: "terminal", Terminal: &result, TerminalSHA256: resultDigest}
		if err = settleManagedCodexRows(ctx, tx, msg.ID, key); err != nil {
			return err
		}
		if err = putManagedCodexRecovery(ctx, tx, msg.ID, key, fields, value); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func settleManagedCodexRows(ctx context.Context, tx *sql.Tx, inv, except string) error {
	rows, err := tx.QueryContext(ctx, `SELECT observation_key,record FROM usage_observations WHERE invocation_id=?`, inv)
	if err != nil {
		return err
	}
	type change struct {
		key    string
		fields map[string]json.RawMessage
		value  *managedCodexRecovery
	}
	var changes []change
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			break
		}
		if key == except {
			continue
		}
		fields, r, e := managedCodexRecoveryRow(raw)
		if e != nil {
			err = e
			break
		}
		if r == nil || r.State == "settled" || r.State == "completed" {
			continue
		}
		if r.State != "sealed" {
			err = fmt.Errorf("managed original checkpoint has uncertain prior work")
			break
		}
		r.State = "settled"
		changes = append(changes, change{key, fields, r})
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	for _, value := range changes {
		if err = putManagedCodexRecovery(ctx, tx, inv, value.key, value.fields, value.value); err != nil {
			return err
		}
	}
	return nil
}
func managedCheckpointExists(ctx context.Context, store *SQLiteStore, inv string) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, err := managedRecoveryForInvocation(ctx, tx, inv)
	if err != nil {
		return false, err
	}
	return r != nil && r.State == "terminal", tx.Commit()
}

// Called in the same transaction that publishes the original worker/message
// result. A failed commit leaves the terminal checkpoint intact for readback;
// no provider is needed to reconcile a lost completion acknowledgement.
func finishManagedCodexCheckpoint(ctx context.Context, tx *sql.Tx, id UsageIdentity, result SessionResult) error {
	r, err := managedRecoveryForInvocation(ctx, tx, id.ID)
	if err != nil {
		return err
	}
	if r == nil || r.State != "terminal" {
		return fmt.Errorf("managed original terminal checkpoint absent")
	}
	digest, err := managedResultDigest(result)
	if err != nil || digest != r.TerminalSHA256 {
		return fmt.Errorf("managed original terminal result changed before publication")
	}
	if err = observeUsageTx(ctx, tx, id, usageUpdate(result.Outputs, true)); err != nil {
		return err
	}
	b, err := decodeManagedCodexBatch(r.Batch)
	if err != nil {
		return err
	}
	key := "codex-response:" + b.ResponseID
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, id.ID, key).Scan(&raw); err != nil {
		return err
	}
	fields, _, err := managedCodexRecoveryRow(raw)
	if err != nil {
		return err
	}
	r.State = "completed"
	if err = putManagedCodexRecovery(ctx, tx, id.ID, key, fields, r); err != nil {
		return err
	}
	admissionFields, admission, err := readManagedCodexAdmission(ctx, tx, id.ID)
	if err != nil {
		return err
	}
	admission.State = "completed"
	return writeManagedCodexAdmission(ctx, tx, id.ID, admissionFields, admission)
}

type managedCodexAdmission struct {
	Task             *managedTaskAdmission `json:"task,omitempty"`
	WorkerID         string                `json:"worker_id"`
	MessageID        string                `json:"message_id"`
	StartedAt        time.Time             `json:"started_at"`
	BoundarySHA256   string                `json:"boundary_sha256"`
	RemainingCeiling int                   `json:"remaining_ceiling"`
}

func managedBoundaryDigest(w Worker, msg WorkerMessage) string {
	// Mutable outcomes, session readback, generation and decreasing remaining
	// allowances are checked separately; every admitted boundary stays fixed.
	raw, _ := json.Marshal([]any{w.ID, w.RunID, w.ParentID, w.CreatorInvocation, w.Role, w.Definition, w.TaskWorkspace, w.SessionOwnerID, w.MaxWorkers, w.MaxDepth, w.MaxQueued, w.TurnDuration, w.MaxOutput, w.Access, w.Tools, w.ToolGrants, w.TaskTurns, w.Depth, msg.ID, msg.WorkerID, msg.RequestID, msg.Sequence, msg.TaskAttemptID, msg.Prompt, msg.Input, msg.Output, msg.Model, msg.RepairOnly})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func managedCaptureAdmission(ctx context.Context, tx *sql.Tx, binding runtimeexec.Binding, inv string) (*managedCodexAdmission, error) {
	if binding.Kind != "worker" {
		return managedCaptureTaskAdmission(ctx, tx, binding, inv)
	}
	var workerRaw, messageRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT w.record,m.record FROM workers w JOIN messages m ON m.worker_id=w.id WHERE w.run_id=? AND m.id=? AND w.status='running' AND m.status='running'`, binding.RunID, inv).Scan(&workerRaw, &messageRaw); err != nil {
		return nil, err
	}
	var w Worker
	var msg WorkerMessage
	if err := decodeJSONWithNumbers(workerRaw, &w); err != nil {
		return nil, err
	}
	if err := decodeJSONWithNumbers(messageRaw, &msg); err != nil {
		return nil, err
	}
	if msg.Generation != binding.Generation || msg.ID != inv || msg.WorkerID != w.ID || msg.StartedAt.IsZero() || w.Remaining < 0 {
		return nil, fmt.Errorf("managed original worker admission missing")
	}
	return &managedCodexAdmission{WorkerID: w.ID, MessageID: msg.ID, StartedAt: msg.StartedAt, BoundarySHA256: managedBoundaryDigest(w, msg), RemainingCeiling: w.Remaining}, nil
}
func managedAdmissionMatches(r *managedCodexRecovery, w Worker, msg WorkerMessage) bool {
	a := r.Admission
	if a == nil || a.WorkerID != w.ID || a.MessageID != msg.ID || !a.StartedAt.Equal(msg.StartedAt) || a.BoundarySHA256 != managedBoundaryDigest(w, msg) || w.Remaining < 0 || w.Remaining > a.RemainingCeiling {
		return false
	}
	deadline := managedOriginalDeadline(w, msg)
	return !deadline.IsZero() && time.Now().Before(deadline)
}

type managedCodexCandidate struct {
	worker   Worker
	message  WorkerMessage
	recovery *managedCodexRecovery
	sdk      *managedSDKAttempt
	run      Run
}
type managedCodexResumeContext struct {
	message  WorkerMessage
	recovery *managedCodexRecovery
	sdk      *managedSDKAttempt
}
type managedCodexResumeKey struct{}

func managedReadCandidate(ctx context.Context, tx *sql.Tx, w Worker) (*managedCodexCandidate, error) {
	var raw []byte
	var generation int64
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT record,generation,owner FROM runs WHERE id=? AND status='running'`, w.RunID).Scan(&raw, &generation, &owner); err != nil {
		return nil, err
	}
	var run Run
	if err := decodeJSONWithNumbers(raw, &run); err != nil {
		return nil, err
	}
	run.Generation = generation
	run.Owner = owner
	if run.Owner == "" || w.Status != RunStatusNeedsAttention {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT record FROM messages WHERE worker_id=? AND status='running' ORDER BY sequence LIMIT 2`, w.ID)
	if err != nil {
		return nil, err
	}
	var messages []WorkerMessage
	for rows.Next() {
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var msg WorkerMessage
		if err = decodeJSONWithNumbers(raw, &msg); err != nil {
			break
		}
		messages = append(messages, msg)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if len(messages) != 1 {
		return nil, nil
	}
	msg := messages[0]
	var identity UsageIdentity
	if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_invocations WHERE id=? AND origin_id=?`, msg.ID, w.RunID).Scan(&raw); err != nil {
		return nil, err
	}
	if err = decodeJSONWithNumbers(raw, &identity); err != nil {
		return nil, err
	}
	if identity.ID != msg.ID || identity.RunID != w.RunID {
		return nil, fmt.Errorf("managed original usage scope changed")
	}
	if identity.Provider != "codex-app-server" {
		var codexProof int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND (observation_key=? OR json_type(CAST(record AS TEXT),'$.managed_codex_witness')='object')`, msg.ID, managedCodexAdmissionKey).Scan(&codexProof); err != nil {
			return nil, err
		}
		if codexProof > 0 {
			return nil, fmt.Errorf("managed Codex original provider identity changed")
		}
		if identity.Provider == "claude-sdk" {
			return managedSDKReadTerminalCandidate(ctx, tx, w, msg, run, identity)
		}
		if identity.Provider == "" || identity.Provider == "unknown" {
			return nil, fmt.Errorf("managed original provider identity is unknown")
		}
		return nil, nil
	}

	if identity.Finished || identity.WorkerID != w.ID {
		return nil, nil
	}
	r, err := managedRecoveryForInvocation(ctx, tx, msg.ID)
	if err != nil {
		return nil, err
	}
	if r == nil || !managedAdmissionMatches(r, w, msg) {
		return nil, nil
	}
	previousGeneration, previousOwner := r.Generation, r.Owner
	if r.LastGeneration != 0 {
		previousGeneration, previousOwner = r.LastGeneration, r.LastOwner
	}
	if previousGeneration <= 0 || previousOwner == "" || run.Generation <= previousGeneration || run.Owner == previousOwner || msg.Generation != previousGeneration {
		return nil, nil
	}
	var canceled bool
	if err = tx.QueryRowContext(ctx, `SELECT record,cancellation_requested FROM executions WHERE run_id=? AND generation=?`, run.ID, previousGeneration).Scan(&raw, &canceled); err != nil {
		return nil, err
	}
	var previous Run
	if err = decodeJSONWithNumbers(raw, &previous); err != nil {
		return nil, err
	}
	// Only exclusive workspace recovery establishes owner loss. A user-canceled
	// or failed execution is never silently converted into a process-loss replay.
	if canceled || previous.Status != RunStatusNeedsAttention || previous.FailureClass != "owner_loss" {
		return nil, nil
	}
	if err = checkEffectAdmission(ctx, tx, run.ID); err != nil {
		return nil, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM invocations WHERE run_id=? AND state!='cleaned'`, run.ID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending != 0 {
		return nil, nil
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_calls WHERE run_id=? AND invocation_id=? AND status NOT IN ('completed','denied')`, run.ID, msg.ID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending != 0 {
		return nil, nil
	}
	b, err := decodeManagedCodexBatch(r.Batch)
	if err != nil {
		return nil, err
	}
	if b.InvocationID != msg.ID || identity.SessionID != b.ThreadID || !msg.RepairOnly && w.SessionID != b.ThreadID {
		return nil, nil
	}
	return &managedCodexCandidate{worker: w, message: msg, recovery: r, run: run}, nil
}
func (m *WorkerManager) managedCodexCandidates(ctx context.Context) ([]managedCodexCandidate, error) {
	// The cursor only schedules discovery. Every original is independently
	// validated again before admission; held rows must not pin the first page.
	m.recoveryMu.Lock()
	defer m.recoveryMu.Unlock()
	rows, err := m.Store.db.QueryContext(ctx, `SELECT w.rowid,w.id,w.run_id,w.record FROM workers w JOIN runs r ON r.id=w.run_id WHERE w.rowid>? AND w.status=? AND r.status='running' AND EXISTS(SELECT 1 FROM messages m JOIN usage_observations u ON u.invocation_id=m.id WHERE m.worker_id=w.id AND m.status='running' AND (json_type(CAST(u.record AS TEXT),'$.managed_codex_recovery')='object' OR (u.observation_key='managed-sdk-attempt' AND json_extract(CAST(u.record AS TEXT),'$.managed_sdk_attempt.state')='terminal'))) ORDER BY w.rowid LIMIT 100`, m.recoveryAfter, RunStatusNeedsAttention)
	if err != nil {
		return nil, err
	}
	var workers []Worker
	var lastRow int64
	var scanned int
	for rows.Next() {
		var raw []byte
		var id, run string
		if err = rows.Scan(&lastRow, &id, &run, &raw); err != nil {
			break
		}
		scanned++
		var w Worker
		if decodeJSONWithNumbers(raw, &w) != nil || w.ID != id || w.RunID != run {
			slog.Error("managed original worker record remains held", "worker_id", id)
			continue
		}
		workers = append(workers, w)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	var result []managedCodexCandidate
	for _, w := range workers {
		tx, err := m.Store.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		candidate, err := managedReadCandidate(ctx, tx, w)
		tx.Rollback()
		if err != nil {
			// This original stays held on every read/integrity/admission error.
			// It cannot suppress independently fenced workers in other runs.
			// Query/transaction creation failures above remain global failures.
			slog.Error("managed original recovery remains held", "worker_id", w.ID)
			continue
		}
		if candidate != nil {
			result = append(result, *candidate)
		}
	}
	if scanned == 100 {
		m.recoveryAfter = lastRow
	} else {
		m.recoveryAfter = 0
	}
	return result, nil
}
func (m *WorkerManager) admitManagedCodexRecovery(ctx context.Context, w Worker) (*managedCodexCandidate, error) {
	tx, err := m.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM workers WHERE id=? AND status=?`, w.ID, RunStatusNeedsAttention).Scan(&raw); err != nil {
		return nil, err
	}
	if err = decodeJSONWithNumbers(raw, &w); err != nil {
		return nil, err
	}
	candidate, err := managedReadCandidate(ctx, tx, w)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, fmt.Errorf("managed original recovery admission changed")
	}
	if candidate.sdk != nil {
		fields, a, err := readManagedSDKAttempt(ctx, tx, candidate.message.ID)
		if err != nil {
			return nil, err
		}
		a.LastGeneration = candidate.run.Generation
		a.LastOwner = candidate.run.Owner
		if err = putManagedSDKAttempt(ctx, tx, candidate.message.ID, fields, a); err != nil {
			return nil, err
		}
		candidate.sdk = a
	} else {
		r := candidate.recovery
		r.LastGeneration = candidate.run.Generation
		r.LastOwner = candidate.run.Owner
		b, err := decodeManagedCodexBatch(r.Batch)
		if err != nil {
			return nil, err
		}
		key := "codex-response:" + b.ResponseID
		if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, candidate.message.ID, key).Scan(&raw); err != nil {
			return nil, err
		}
		fields, _, err := managedCodexRecoveryRow(raw)
		if err != nil {
			return nil, err
		}
		if err = putManagedCodexRecovery(ctx, tx, candidate.message.ID, key, fields, r); err != nil {
			return nil, err
		}
	}
	candidate.message.Generation = candidate.run.Generation
	candidate.message.Error = ""
	candidate.worker.Status = RunStatusRunning
	candidate.worker.Error = ""
	raw, _ = json.Marshal(candidate.message)
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET record=? WHERE id=? AND status='running'`, raw, candidate.message.ID); err != nil {
		return nil, err
	}
	raw, _ = json.Marshal(candidate.worker)
	if _, err = tx.ExecContext(ctx, `UPDATE workers SET status=?,record=? WHERE id=? AND status=?`, RunStatusRunning, raw, w.ID, RunStatusNeedsAttention); err != nil {
		return nil, err
	}
	if err = addEvent(ctx, tx, w.RunID, "worker.managed_original_recovery", map[string]any{"worker_id": w.ID, "message_id": candidate.message.ID, "execution_generation": candidate.run.Generation, "original_started_at": candidate.message.StartedAt}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return candidate, nil
}

func (m *WorkerManager) retainManagedCodexProcessLoss(ctx context.Context, w Worker, msg WorkerMessage, turnErr error) (bool, error) {
	if ctx.Err() != nil || !errors.Is(turnErr, ErrManagedCodexProcessLost) || errors.Is(turnErr, ErrUncertainTool) {
		return false, nil
	}
	// PID observations classify the adapter error only. Existing durable runtime
	// cleanup receipts independently establish that all owned work was joined.
	if err := m.Store.CheckInvocationCleanup(context.WithoutCancel(ctx), w.RunID); err != nil {
		return false, err
	}
	_, binding, err := managedCodexUsageOwner(ctx, w.RunID, msg.ID)
	if err != nil {
		return false, err
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := m.Store.db.BeginTx(commitCtx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(commitCtx, tx, binding); err != nil {
		return false, err
	}
	if err = checkEffectAdmission(commitCtx, tx, w.RunID); err != nil {
		return false, err
	}
	r, err := managedRecoveryForInvocation(commitCtx, tx, msg.ID)
	if err != nil {
		return false, err
	}
	if r == nil || !managedAdmissionMatches(r, w, msg) {
		return false, nil
	}
	var pending int
	if err = tx.QueryRowContext(commitCtx, `SELECT count(*) FROM tool_calls WHERE run_id=? AND invocation_id=? AND status NOT IN ('completed','denied')`, w.RunID, msg.ID).Scan(&pending); err != nil {
		return false, err
	}
	if pending != 0 {
		return false, nil
	}
	var raw []byte
	if err = tx.QueryRowContext(commitCtx, `SELECT record FROM workers WHERE id=? AND status='running'`, w.ID).Scan(&raw); err != nil {
		return false, err
	}
	var current Worker
	if err = decodeJSONWithNumbers(raw, &current); err != nil {
		return false, err
	}
	if !managedAdmissionMatches(r, current, msg) {
		return false, fmt.Errorf("managed lost process admission changed")
	}
	current.Status = RunStatusNeedsAttention
	current.Error = "owned managed provider process stopped; original sealed invocation retained"
	current.FailureClass = "managed_process_loss"
	current.FailureGeneration = binding.Generation
	raw, _ = json.Marshal(current)
	if _, err = tx.ExecContext(commitCtx, `UPDATE workers SET status=?,record=? WHERE id=?`, current.Status, raw, w.ID); err != nil {
		return false, err
	}
	if err = addEvent(commitCtx, tx, w.RunID, "worker.managed_process_lost", map[string]string{"worker_id": w.ID, "message_id": msg.ID}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
