package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

const managedSDKAttemptKey = "managed-sdk-attempt"
const managedSDKProfile = "claude-sdk@0.3.322/broker-stream-v1"

type managedSDKAttempt struct {
	RunID          string                 `json:"run_id"`
	InvocationID   string                 `json:"invocation_id"`
	Profile        string                 `json:"profile"`
	Model          string                 `json:"model"`
	RequestedModel string                 `json:"requested_model"`
	ModelBound     bool                   `json:"model_bound,omitempty"`
	Generation     int64                  `json:"generation"`
	LastGeneration int64                  `json:"last_generation,omitempty"`
	LastOwner      string                 `json:"last_owner,omitempty"`
	Terminal       *SessionResult         `json:"terminal,omitempty"`
	Owner          string                 `json:"owner"`
	Admission      *managedCodexAdmission `json:"original_admission"`
	State          string                 `json:"state"`
	SessionID      string                 `json:"session_id,omitempty"`
	TerminalSHA256 string                 `json:"terminal_sha256,omitempty"`
}
type managedSDKAttemptRow struct {
	UsageObservation
	Attempt *managedSDKAttempt `json:"managed_sdk_attempt"`
}
type managedSDKTerminalKey struct{}
type managedSDKTerminalRecorder func(context.Context, SessionResult) error

// BeginManagedSDKInvocation is bookkeeping, never a zero-token observation.
// Unknown native attempts remain held even when every known tool receipt and
// runtime cleanup has settled. This path does not replay SDK native callbacks.
func BeginManagedSDKInvocation(ctx context.Context, run, inv, model string) error {
	if err := checkManagedProviderAuthority(ctx, run, inv, "claude-sdk"); err != nil {
		return err
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return err
	}
	identity, err := managedProviderUsageIdentity(ctx, tx, run, inv, "claude-sdk")
	if err != nil {
		return err
	}
	if identity.Model != "unknown" && identity.Model != model && model != "" {
		return fmt.Errorf("SDK model differs from original usage admission")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, managedSDKAttemptKey).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("SDK original invocation cannot acquire another native attempt")
	}
	admission, err := managedCaptureAdmission(ctx, tx, binding, inv)
	if err != nil {
		return err
	}
	row := managedSDKAttemptRow{UsageObservation: UsageObservation{Scope: "invocation"}, Attempt: &managedSDKAttempt{RunID: run, InvocationID: inv, Profile: managedSDKProfile, Model: model, RequestedModel: model, Generation: binding.Generation, Owner: binding.Owner, Admission: admission, State: "active"}}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,?,?)`, inv, managedSDKAttemptKey, raw); err != nil {
		return err
	}
	return tx.Commit()
}

// BindManagedSDKModel records the pinned public initialization's once-resolved
// model before the bridge releases any task data. The requested selector stays
// immutable; retained cost/session identity must match the canonical model.
func BindManagedSDKModel(ctx context.Context, run, inv, requested, resolved string) error {
	if resolved == "" || resolved == "unknown" || len(resolved) > 256 {
		return fmt.Errorf("SDK initialized model is missing or invalid")
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return err
	}
	fields, a, err := readManagedSDKAttempt(ctx, tx, inv)
	if err != nil {
		return err
	}
	if a.Profile != managedSDKProfile || a.RunID != run || a.InvocationID != inv || a.Generation != binding.Generation || a.Owner != binding.Owner || a.State != "active" || a.ModelBound || a.RequestedModel != requested {
		return fmt.Errorf("SDK initialized model differs from original admission")
	}
	id, err := managedProviderUsageIdentity(ctx, tx, run, inv, "claude-sdk")
	if err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key!=?`, inv, managedSDKAttemptKey).Scan(&count); err != nil {
		return err
	}
	if count != 0 || id.Finished {
		return fmt.Errorf("SDK model binding follows task effects")
	}
	id.Model = resolved
	if err = validateManagedSDKRetainedCost(ctx, tx, id); err != nil {
		return err
	}
	if _, err = managedCaptureAdmission(ctx, tx, binding, inv); err != nil {
		return err
	}
	a.Model, a.ModelBound = resolved, true
	if err = putManagedSDKAttempt(ctx, tx, inv, fields, a); err != nil {
		return err
	}
	// Preserve any independently retained JSON fields in the usage identity.
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_invocations WHERE id=?`, inv).Scan(&raw); err != nil {
		return err
	}
	var identity map[string]json.RawMessage
	if err = json.Unmarshal(raw, &identity); err != nil {
		return err
	}
	identity["model"], _ = json.Marshal(resolved)
	raw, err = json.Marshal(identity)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_invocations SET record=? WHERE id=?`, raw, inv); err != nil {
		return err
	}
	return tx.Commit()
}

func readManagedSDKAttempt(ctx context.Context, tx *sql.Tx, inv string) (map[string]json.RawMessage, *managedSDKAttempt, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, managedSDKAttemptKey).Scan(&raw); err != nil {
		return nil, nil, err
	}
	var fields map[string]json.RawMessage
	var row managedSDKAttemptRow
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	if err := decodeJSONWithNumbers(raw, &row); err != nil {
		return nil, nil, err
	}
	if row.Attempt == nil || !managedZeroChargeBookkeeping(row.UsageObservation) {
		return nil, nil, fmt.Errorf("SDK original attempt bookkeeping is malformed")
	}
	return fields, row.Attempt, nil
}
func putManagedSDKAttempt(ctx context.Context, tx *sql.Tx, inv string, fields map[string]json.RawMessage, a *managedSDKAttempt) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	fields["managed_sdk_attempt"] = raw
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, inv, managedSDKAttemptKey)
	return err
}
func sdkAttemptMatches(a *managedSDKAttempt, b runtimeexec.Binding, w Worker, msg WorkerMessage) bool {
	return a != nil && a.Profile == managedSDKProfile && a.RunID == b.RunID && a.InvocationID == msg.ID && (a.Generation == b.Generation && a.Owner == b.Owner || a.LastGeneration == b.Generation && a.LastOwner == b.Owner) && a.Admission != nil && a.Admission.WorkerID == w.ID && a.Admission.MessageID == msg.ID && a.Admission.StartedAt.Equal(msg.StartedAt) && a.Admission.BoundarySHA256 == managedBoundaryDigest(w, msg) && w.Remaining >= 0 && w.Remaining <= a.Admission.RemainingCeiling
}
func (m *WorkerManager) sdkTerminalRecorder(w Worker, msg WorkerMessage, secrets map[string]string) managedSDKTerminalRecorder {
	return func(ctx context.Context, result SessionResult) error {
		if err := checkManagedProviderAuthority(ctx, w.RunID, msg.ID, "claude-sdk"); err != nil {
			return err
		}
		if result.Status != RunStatusSucceeded || result.ProviderStatus != "success" || result.ProviderVersion != "claude-agent-sdk@0.3.322" || result.SessionID == "" || !NormalizeUsage(usageUpdate(result.Outputs, true)).Complete {
			return fmt.Errorf("SDK terminal/accounting is incomplete")
		}
		result.Outputs, _ = redactValue(result.Outputs, secrets).(map[string]any)
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		limit := w.MaxOutput
		if limit == 0 {
			limit = m.Limits.MaxOutput
		}
		if limit <= 0 || len(raw) > limit {
			return fmt.Errorf("SDK terminal exceeds original output limit")
		}
		digest, err := managedResultDigest(result)
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
		fields, a, err := readManagedSDKAttempt(ctx, tx, msg.ID)
		if err != nil {
			return err
		}
		if !sdkAttemptMatches(a, binding, w, msg) || a.State != "active" {
			return fmt.Errorf("SDK original terminal admission changed")
		}
		if err = validateManagedSDKResponseTotals(ctx, tx, msg.ID, result); err != nil {
			return err
		}
		a.Terminal = &result
		a.State = "terminal"
		a.SessionID = result.SessionID
		a.TerminalSHA256 = digest
		if err = putManagedSDKAttempt(ctx, tx, msg.ID, fields, a); err != nil {
			return err
		}
		return tx.Commit()
	}
}

// CheckpointManagedSDKTerminal is callable only with the engine's private
// original-message closure; provider events/result fields cannot install it.
func CheckpointManagedSDKTerminal(ctx context.Context, result SessionResult) error {
	record, ok := ctx.Value(managedSDKTerminalKey{}).(managedSDKTerminalRecorder)
	if !ok {
		return fmt.Errorf("SDK terminal lacks original engine authority")
	}
	return record(ctx, result)
}
func settleManagedSDKAttempt(ctx context.Context, tx *sql.Tx, w Worker, msg WorkerMessage, result SessionResult) error {
	fields, a, err := readManagedSDKAttempt(ctx, tx, msg.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	binding, ok := runtimeexec.BindingFrom(ctx)
	if !ok || !sdkAttemptMatches(a, binding, w, msg) || a.State != "terminal" || msg.Status != RunStatusSucceeded || a.SessionID != result.SessionID {
		return fmt.Errorf("SDK original attempt remains unsettled")
	}
	digest, err := managedResultDigest(result)
	if err != nil || digest != a.TerminalSHA256 {
		return fmt.Errorf("SDK original terminal result changed")
	}
	identity, err := managedProviderUsageIdentity(ctx, tx, w.RunID, msg.ID, "claude-sdk")
	if err != nil {
		return err
	}
	if err = validateManagedSDKResponseTotals(ctx, tx, msg.ID, result); err != nil {
		return err
	}
	if err = observeManagedSDKFinalTx(ctx, tx, identity, result); err != nil {
		return err
	}
	a.State = "settled"
	return putManagedSDKAttempt(ctx, tx, msg.ID, fields, a)
}
func managedSDKWorkerHold(ctx context.Context, tx *sql.Tx, worker string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations u JOIN messages m ON m.id=u.invocation_id WHERE m.worker_id=? AND u.observation_key=? AND COALESCE(json_extract(CAST(u.record AS TEXT),'$.managed_sdk_attempt.state'),'')!='settled'`, worker, managedSDKAttemptKey).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("SDK original native attempt must settle before new work or acknowledgement")
	}
	return nil
}

func CheckManagedSDKAuthority(ctx context.Context, run, inv string) error {
	return checkManagedProviderAuthority(ctx, run, inv, "claude-sdk")
}

func validateManagedSDKResponseTotals(ctx context.Context, tx *sql.Tx, inv string, result SessionResult) error {
	rows, err := tx.QueryContext(ctx, `SELECT observation_key,record FROM usage_observations WHERE invocation_id=?`, inv)
	if err != nil {
		return err
	}
	defer rows.Close()
	input, cached, output := int64(0), int64(0), int64(0)
	count := 0
	for rows.Next() {
		var key string
		var raw []byte
		if err = rows.Scan(&key, &raw); err != nil {
			return err
		}
		if key == managedSDKAttemptKey {
			continue
		}
		if key == "final" {
			var known UsageObservation
			if err = decodeJSONWithNumbers(raw, &known); err != nil {
				return err
			}
			var identity UsageIdentity
			// Model is checked below against the engine-owned invocation row.
			var identityRaw []byte
			if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_invocations WHERE id=?`, inv).Scan(&identityRaw); err != nil {
				return err
			}
			if err = decodeJSONWithNumbers(identityRaw, &identity); err != nil {
				return err
			}
			if known.Complete || !known.CostIncomplete || !managedSDKKnownCostMatches(known, identity, result) {
				return fmt.Errorf("SDK known terminal cost receipt conflicts")
			}
			continue
		}
		if !strings.HasPrefix(key, "sdk-response:") || len(strings.TrimPrefix(key, "sdk-response:")) == 0 {
			return fmt.Errorf("SDK original attempt has an unknown observation")
		}
		var row UsageObservation
		if err = decodeJSONWithNumbers(raw, &row); err != nil {
			return err
		}
		if row.Scope != "invocation" || row.Final || row.CounterReset || row.CounterBasis != "" || row.InputTokens == nil || row.CachedInputTokens == nil || row.OutputTokens == nil || *row.InputTokens < 0 || *row.CachedInputTokens < 0 || *row.OutputTokens < 0 || *row.InputTokens > math.MaxInt64-input || *row.CachedInputTokens > math.MaxInt64-cached || *row.OutputTokens > math.MaxInt64-output {
			return fmt.Errorf("SDK original response accounting is incomplete")
		}
		input += *row.InputTokens
		cached += *row.CachedInputTokens
		output += *row.OutputTokens
		count++
		if count > 4096 {
			return fmt.Errorf("SDK original response count exceeds bound")
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("SDK terminal lacks original response accounting")
	}
	usage := NormalizeUsage(UsageUpdate{Provider: "claude-sdk", Usage: usageUpdate(result.Outputs, true).Usage})
	if usage.InputTokens == nil || usage.CachedInputTokens == nil || usage.OutputTokens == nil || *usage.InputTokens != input || *usage.CachedInputTokens != cached || *usage.OutputTokens != output {
		return fmt.Errorf("SDK saved terminal differs from original known response rows")
	}
	return nil
}

func managedSDKReadTerminalCandidate(ctx context.Context, tx *sql.Tx, w Worker, msg WorkerMessage, run Run, identity UsageIdentity) (*managedCodexCandidate, error) {
	fields, a, err := readManagedSDKAttempt(ctx, tx, msg.ID)
	_ = fields
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if a.State != "terminal" {
		return nil, nil
	}
	if a.Terminal == nil || a.SessionID == "" || a.Profile != managedSDKProfile || a.RunID != run.ID || a.InvocationID != msg.ID || identity.ID != msg.ID || identity.RunID != run.ID || identity.WorkerID != w.ID || identity.Provider != "claude-sdk" || identity.Finished || identity.Model != a.Model || identity.SessionID != a.SessionID {
		return nil, fmt.Errorf("SDK original terminal identity is invalid")
	}
	oldGen, oldOwner := a.Generation, a.Owner
	if a.LastGeneration != 0 {
		oldGen, oldOwner = a.LastGeneration, a.LastOwner
	}
	b := runtimeexec.Binding{RunID: run.ID, Generation: oldGen, Owner: oldOwner}
	if !sdkAttemptMatches(a, b, w, msg) || msg.Generation != oldGen || run.Generation <= oldGen || run.Owner == oldOwner || oldOwner == "" || run.Owner == "" || msg.Status != RunStatusRunning || w.Status != RunStatusNeedsAttention {
		return nil, nil
	}
	deadline := managedOriginalDeadline(w, msg)
	if deadline.IsZero() || !time.Now().Before(deadline) {
		return nil, nil
	}
	digest, err := managedResultDigest(*a.Terminal)
	if err != nil || digest != a.TerminalSHA256 || a.Terminal.SessionID != a.SessionID || a.Terminal.Status != RunStatusSucceeded {
		return nil, fmt.Errorf("SDK original saved terminal is corrupted")
	}
	if !msg.RepairOnly && w.SessionID != a.SessionID {
		return nil, fmt.Errorf("SDK original terminal session changed")
	}
	if err = validateManagedSDKResponseTotals(ctx, tx, msg.ID, *a.Terminal); err != nil {
		return nil, err
	}
	var previousRaw []byte
	var cancelled bool
	if err = tx.QueryRowContext(ctx, `SELECT record,cancellation_requested FROM executions WHERE run_id=? AND generation=?`, run.ID, oldGen).Scan(&previousRaw, &cancelled); err != nil {
		return nil, err
	}
	var previous Run
	if err = decodeJSONWithNumbers(previousRaw, &previous); err != nil {
		return nil, err
	}
	if cancelled || previous.Status != RunStatusNeedsAttention || previous.FailureClass != "owner_loss" {
		return nil, nil
	}
	if err = checkEffectAdmission(ctx, tx, run.ID); err != nil {
		return nil, err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM invocations WHERE run_id=? AND state!='cleaned'`, run.ID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, nil
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_calls WHERE run_id=? AND invocation_id=? AND status NOT IN ('completed','denied')`, run.ID, msg.ID).Scan(&pending); err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, nil
	}
	return &managedCodexCandidate{worker: w, message: msg, run: run, sdk: a}, nil
}
