package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

const managedSDKAttemptKey = "managed-sdk-attempt"
const managedSDKProfile = "claude-sdk@0.3.322/broker-stream-v1"

type managedSDKAttempt struct {
	RunID          string                 `json:"run_id"`
	InvocationID   string                 `json:"invocation_id"`
	Profile        string                 `json:"profile"`
	Model          string                 `json:"model"`
	Generation     int64                  `json:"generation"`
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
	if identity.Model != "unknown" && identity.Model != model {
		return fmt.Errorf("SDK model differs from original usage admission")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, managedSDKAttemptKey).Scan(&count); err != nil {
		return err
	}
	if count != 0 || model == "" {
		return fmt.Errorf("SDK original invocation cannot acquire another native attempt")
	}
	admission, err := managedCaptureAdmission(ctx, tx, binding, inv)
	if err != nil {
		return err
	}
	row := managedSDKAttemptRow{UsageObservation: UsageObservation{Scope: "invocation"}, Attempt: &managedSDKAttempt{RunID: run, InvocationID: inv, Profile: managedSDKProfile, Model: model, Generation: binding.Generation, Owner: binding.Owner, Admission: admission, State: "active"}}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,?,?)`, inv, managedSDKAttemptKey, raw); err != nil {
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
	if row.Attempt == nil || row.Scope != "invocation" || row.Final || row.Complete || row.InputTokens != nil || row.OutputTokens != nil || row.CachedInputTokens != nil || row.CostUSD != nil || row.CounterReset || row.CounterBasis != "" {
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
	return a != nil && a.Profile == managedSDKProfile && a.RunID == b.RunID && a.InvocationID == msg.ID && a.Generation == b.Generation && a.Owner == b.Owner && a.Admission != nil && a.Admission.WorkerID == w.ID && a.Admission.MessageID == msg.ID && a.Admission.StartedAt.Equal(msg.StartedAt) && a.Admission.BoundarySHA256 == managedBoundaryDigest(w, msg) && w.Remaining >= 0 && w.Remaining <= a.Admission.RemainingCeiling
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
