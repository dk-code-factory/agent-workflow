package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const managedSDKCostBasis = managedSDKProfile + "/cumulative-cost-v1"

type managedSDKCostCounter struct {
	Profile      string   `json:"profile"`
	Model        string   `json:"model"`
	SessionID    string   `json:"session_id"`
	InvocationID string   `json:"invocation_id"`
	Receipt      string   `json:"receipt"`
	TotalUSD     *float64 `json:"total_usd"`
}

// The pinned native terminal cost survives fresh-process resume. Only cost
// uses this session baseline: original response token totals remain flat.
// Called exclusively inside the existing original completion transaction.
func observeManagedSDKFinalTx(ctx context.Context, tx *sql.Tx, id UsageIdentity, result SessionResult) error {
	total := costValue(result.Outputs["cost_usd"])
	if total == nil || id.Provider != "claude-sdk" || id.Model == "" || id.Model == "unknown" || result.SessionID == "" || id.SessionID != result.SessionID {
		return fmt.Errorf("managed SDK cumulative cost identity/value is incomplete")
	}
	receipt := actionHash([]any{id.ID, id.RunID, id.Model, result.SessionID, result.Outputs["usage"], result.Outputs["cost_usd"]})
	key := "claude-sdk\x00" + result.SessionID + "\x00" + managedSDKCostBasis
	var raw []byte
	var previous managedSDKCostCounter
	err := tx.QueryRowContext(ctx, `SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, key).Scan(&raw)
	if err == nil {
		if err = decodeJSONWithNumbers(raw, &previous); err != nil {
			return err
		}
		if previous.Profile != managedSDKCostBasis || previous.Model != id.Model || previous.SessionID != result.SessionID || previous.InvocationID == "" || previous.Receipt == "" || previous.TotalUSD == nil || costValue(*previous.TotalUSD) == nil {
			return fmt.Errorf("managed SDK cumulative cost baseline is malformed")
		}

	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// An original committed receipt stays idempotent even after later turns
	// advance this session's baseline. Changed originals never rewrite it.
	var finalRaw []byte
	finalErr := tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&finalRaw)
	if finalErr == nil {
		var final UsageObservation
		if err = decodeJSONWithNumbers(finalRaw, &final); err != nil {
			return err
		}
		if previous.TotalUSD == nil || *previous.TotalUSD < *total || final.CounterBasis != managedSDKCostBasis || final.ReceiptFingerprint != receipt || final.CostUSD == nil || *final.CostUSD != *total || final.DeltaCostUSD == nil || costValue(*final.DeltaCostUSD) == nil || !final.Complete || final.CounterReset {
			return fmt.Errorf("managed SDK original final cost receipt conflicts")
		}
		return nil
	}
	if !errors.Is(finalErr, sql.ErrNoRows) {
		return finalErr
	}
	if previous.InvocationID == id.ID {
		return fmt.Errorf("managed SDK original final cost receipt is missing")
	}
	delta := *total
	if previous.TotalUSD != nil {
		if delta < *previous.TotalUSD {
			return fmt.Errorf("managed SDK cumulative cost regressed")
		}
		delta -= *previous.TotalUSD
	}
	if err = observeUsageTx(ctx, tx, id, usageUpdate(result.Outputs, true)); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&raw); err != nil {
		return err
	}
	var final UsageObservation
	if err = decodeJSONWithNumbers(raw, &final); err != nil {
		return err
	}
	if final.Scope != "invocation" || final.CounterBasis != "" || !final.Complete {
		return fmt.Errorf("managed SDK original final token accounting changed")
	}
	final.CounterBasis = managedSDKCostBasis
	final.DeltaCostUSD = &delta
	final.ReceiptFingerprint = receipt
	raw, err = json.Marshal(final)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key='final'`, raw, id.ID); err != nil {
		return err
	}
	next := managedSDKCostCounter{Profile: managedSDKCostBasis, Model: id.Model, SessionID: result.SessionID, InvocationID: id.ID, Receipt: receipt, TotalUSD: total}
	raw, err = json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_session_counters(origin_id,scope_key,record) VALUES(?,?,?) ON CONFLICT(origin_id,scope_key) DO UPDATE SET record=excluded.record`, id.RunID, key, raw)
	return err
}

// A retained native session needs this exact managed baseline before task
// data. An older/unmanaged history must not silently reacquire this profile.
func validateManagedSDKRetainedCost(ctx context.Context, tx *sql.Tx, id UsageIdentity) error {
	if id.SessionID == "" {
		return nil
	}
	key := "claude-sdk\x00" + id.SessionID + "\x00" + managedSDKCostBasis
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, key).Scan(&raw); err != nil {
		return fmt.Errorf("SDK retained session lacks its original managed cost baseline: %w", err)
	}
	var c managedSDKCostCounter
	if err := decodeJSONWithNumbers(raw, &c); err != nil {
		return err
	}
	if c.Profile != managedSDKCostBasis || c.SessionID != id.SessionID || c.Model != id.Model || c.TotalUSD == nil || costValue(*c.TotalUSD) == nil || c.InvocationID == "" || c.Receipt == "" {
		return fmt.Errorf("SDK retained managed session cost identity changed")
	}
	var a managedSDKAttemptRow
	if err := tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, c.InvocationID, managedSDKAttemptKey).Scan(&raw); err != nil {
		return err
	}
	if err := decodeJSONWithNumbers(raw, &a); err != nil {
		return err
	}
	if a.Attempt == nil || a.Attempt.State != "settled" || a.Attempt.Profile != managedSDKProfile || a.Attempt.RunID != id.RunID || a.Attempt.SessionID != id.SessionID || a.Attempt.Model != id.Model {
		return fmt.Errorf("SDK retained session original terminal is not settled")
	}
	return nil
}
