package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const managedCodexAdmissionKey = "codex-managed-admission"

type managedCodexAttemptAdmission struct {
	RunID              string                 `json:"run_id"`
	InvocationID       string                 `json:"invocation_id"`
	BuildID            string                 `json:"build_id"`
	PolicyID           string                 `json:"policy_id"`
	Model              string                 `json:"model"`
	SessionID          string                 `json:"session_id,omitempty"`
	TurnID             string                 `json:"turn_id,omitempty"`
	OriginalGeneration int64                  `json:"original_generation"`
	OriginalOwner      string                 `json:"original_owner"`
	Admission          *managedCodexAdmission `json:"admission"`
	State              string                 `json:"state"`
}
type managedCodexAdmissionRow struct {
	UsageObservation
	Admission *managedCodexAttemptAdmission `json:"managed_codex_admission"`
}

func readManagedCodexAdmission(ctx context.Context, tx *sql.Tx, inv string) (map[string]json.RawMessage, *managedCodexAttemptAdmission, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, inv, managedCodexAdmissionKey).Scan(&raw); err != nil {
		return nil, nil, err
	}
	return parseManagedCodexAdmission(raw)
}
func parseManagedCodexAdmission(raw []byte) (map[string]json.RawMessage, *managedCodexAttemptAdmission, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, err
	}
	var row managedCodexAdmissionRow
	if err := decodeJSONWithNumbers(raw, &row); err != nil {
		return nil, nil, err
	}
	if row.Admission == nil || row.Scope != "invocation" || row.Final || row.Complete || row.CounterReset || row.InputTokens != nil || row.CachedInputTokens != nil || row.OutputTokens != nil || row.CostUSD != nil || row.DeltaInputTokens != nil || row.DeltaCachedInputTokens != nil || row.DeltaOutputTokens != nil {
		return nil, nil, fmt.Errorf("managed admission is not exact zero-charge bookkeeping")
	}
	return fields, row.Admission, nil
}
func writeManagedCodexAdmission(ctx context.Context, tx *sql.Tx, inv string, fields map[string]json.RawMessage, value *managedCodexAttemptAdmission) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields["managed_codex_admission"] = raw
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key=?`, raw, inv, managedCodexAdmissionKey)
	return err
}

// BeginManagedCodexInvocation installs zero-charge engine bookkeeping before
// thread/start can receive task data. It never records synthetic usage. An
// unknown first native attempt therefore remains distinguishable from a stock
// adapter turn, even when no Completed response or broker seal ever arrives.
func BeginManagedCodexInvocation(ctx context.Context, run, inv, build, policy, model string) error {
	if err := CheckManagedCodexAuthority(ctx, run, inv); err != nil {
		return err
	}
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
	if err != nil {
		return err
	}
	if build == "" || policy == "" || model == "" {
		return fmt.Errorf("managed admission profile is incomplete")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(ctx, tx, binding); err != nil {
		return err
	}
	_, prior, err := readManagedCodexAdmission(ctx, tx, inv)
	if err == nil {
		recovery, ok := ctx.Value(managedCodexRecoveryKey{}).(managedCodexRecoveryContext)
		if !ok || prior.State != "active" || prior.RunID != run || prior.InvocationID != inv || prior.BuildID != build || prior.PolicyID != policy || prior.Model != model || prior.Admission == nil || recovery.value.Admission == nil || prior.Admission.BoundarySHA256 != recovery.value.Admission.BoundarySHA256 {
			return fmt.Errorf("managed invocation cannot start another attempt without original recovery authority")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	admission, err := managedCaptureAdmission(ctx, tx, binding, inv)
	if err != nil {
		return err
	}
	value := managedCodexAttemptAdmission{RunID: run, InvocationID: inv, BuildID: build, PolicyID: policy, Model: model, OriginalGeneration: binding.Generation, OriginalOwner: binding.Owner, Admission: admission, State: "active"}
	row := managedCodexAdmissionRow{UsageObservation: UsageObservation{Scope: "invocation"}, Admission: &value}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,?,?)`, inv, managedCodexAdmissionKey, raw); err != nil {
		return err
	}
	return tx.Commit()
}

// BindManagedCodexInvocation records official session/turn readback. Empty
// values never clear an earlier identity, and recovery retains the same turn.
func BindManagedCodexInvocation(ctx context.Context, run, inv, session, turn string) error {
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
	fields, value, err := readManagedCodexAdmission(ctx, tx, inv)
	if err != nil {
		return err
	}
	if value.State != "active" || value.RunID != run || value.InvocationID != inv || session == "" || value.SessionID != "" && value.SessionID != session || turn != "" && value.TurnID != "" && value.TurnID != turn {
		return fmt.Errorf("managed official readback differs from original admission")
	}
	value.SessionID = session
	if turn != "" {
		value.TurnID = turn
	}
	if err = writeManagedCodexAdmission(ctx, tx, inv, fields, value); err != nil {
		return err
	}
	return tx.Commit()
}
func validateManagedCodexBookkeeping(value *managedCodexAttemptAdmission, run, inv string, proof *managedCodexUsageProof) error {
	if value == nil || value.State != "active" || value.RunID != run || value.InvocationID != inv || value.OriginalGeneration <= 0 || value.OriginalOwner == "" || value.Admission == nil || value.Admission.MessageID != inv || value.Admission.WorkerID == "" || value.Admission.StartedAt.IsZero() || value.SessionID == "" || value.TurnID == "" {
		return fmt.Errorf("managed original admission is missing, held or unbound")
	}
	if proof != nil && (value.BuildID != proof.BuildID || value.PolicyID != proof.PolicyID || value.Model != proof.Model || value.SessionID != proof.SessionID || value.TurnID != proof.TurnID) {
		return fmt.Errorf("managed bookkeeping differs from original native witness")
	}
	return nil
}

func holdManagedCodexAdmission(ctx context.Context, run, inv string) error {
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
	fields, value, err := readManagedCodexAdmission(ctx, tx, inv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if value.State == "completed" {
		return fmt.Errorf("managed completed invocation cannot become another attempt")
	}
	value.State = "held"
	if err = writeManagedCodexAdmission(ctx, tx, inv, fields, value); err != nil {
		return err
	}
	return tx.Commit()
}
