package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
// Completion commits this mutation set. Known-charge recording evaluates it
// inside a savepoint and retains only the incomplete original cost receipt.
func observeManagedSDKFinalTx(ctx context.Context, tx *sql.Tx, id UsageIdentity, result SessionResult) error {
	total := costValue(result.Outputs["cost_usd"])
	if total == nil || id.Provider != "claude-sdk" || id.Model == "" || id.Model == "unknown" || result.SessionID == "" || id.SessionID != result.SessionID {
		return fmt.Errorf("managed SDK cumulative cost identity/value is incomplete")
	}
	receipt := managedSDKCostReceipt(id, result)
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
	var known *UsageObservation
	finalErr := tx.QueryRowContext(ctx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&finalRaw)
	if finalErr == nil {
		var final UsageObservation
		if err = decodeJSONWithNumbers(finalRaw, &final); err != nil {
			return err
		}
		if !managedSDKKnownCostMatches(final, id, result) {
			return fmt.Errorf("managed SDK original final cost receipt conflicts")
		}
		if final.Complete {
			if previous.TotalUSD == nil || *previous.TotalUSD < *total {
				return fmt.Errorf("managed SDK original final cost baseline conflicts")
			}
			return nil
		}
		known = &final
	}
	if finalErr != nil && !errors.Is(finalErr, sql.ErrNoRows) {
		return finalErr
	}
	if previous.InvocationID == id.ID {
		return fmt.Errorf("managed SDK original final cost receipt is missing")
	}
	delta := *total
	if known != nil {
		// This original's charge was already captured against its admitted
		// baseline. Later settled turns cannot change that known delta.
		delta = *known.DeltaCostUSD
	} else if previous.TotalUSD != nil {
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
	if previous.TotalUSD != nil && *previous.TotalUSD > *total {
		return nil // An older original receipt never rewinds a later baseline.
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

func managedSDKCostReceipt(id UsageIdentity, result SessionResult) string {
	return actionHash([]any{id.ID, id.RunID, id.Model, result.SessionID, result.Outputs["usage"], result.Outputs["cost_usd"]})
}
func managedSDKKnownCostMatches(row UsageObservation, id UsageIdentity, result SessionResult) bool {
	total := costValue(result.Outputs["cost_usd"])
	update := usageUpdate(result.Outputs, true)
	// Match the provider inheritance used when the original receipt was stored.
	if update.Provider == "" {
		update.Provider = id.Provider
	}
	expected := NormalizeUsage(update)
	left, _ := json.Marshal([]any{row.InputTokens, row.CachedInputTokens, row.OutputTokens})
	right, _ := json.Marshal([]any{expected.InputTokens, expected.CachedInputTokens, expected.OutputTokens})
	return string(left) == string(right) && row.Final && row.Scope == "invocation" && row.CounterBasis == managedSDKCostBasis && row.ReceiptFingerprint == managedSDKCostReceipt(id, result) && total != nil && row.CostUSD != nil && *row.CostUSD == *total && row.DeltaCostUSD != nil && costValue(*row.DeltaCostUSD) != nil && *row.DeltaCostUSD <= *total && !row.CounterReset
}

// RecordManagedSDKTerminalCost preserves a validated native terminal's known
// charge before process cleanup/cancellation can return. This incomplete receipt
// does not finish the invocation, advance its session baseline, settle the
// original attempt, or authorize more work. Only original completion can do so.
func RecordManagedSDKTerminalCost(ctx context.Context, run, inv string, result SessionResult) error {
	store, binding, err := managedCodexUsageOwner(ctx, run, inv)
	if err != nil {
		return err
	}
	if result.Status != RunStatusSucceeded || result.ProviderStatus != "success" || result.ProviderVersion != "claude-agent-sdk@0.3.322" || result.SessionID == "" || !NormalizeUsage(usageUpdate(result.Outputs, true)).Complete {
		return fmt.Errorf("SDK known cost lacks a complete original native terminal")
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(commitCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = managedCodexUsageFence(commitCtx, tx, binding); err != nil {
		return err
	}
	id, err := managedProviderUsageIdentity(commitCtx, tx, run, inv, "claude-sdk")
	if err != nil {
		return err
	}
	_, a, err := readManagedSDKAttempt(commitCtx, tx, inv)
	if err != nil {
		return err
	}
	if a.Profile != managedSDKProfile || a.RunID != run || a.InvocationID != inv || a.Generation != binding.Generation || a.Owner != binding.Owner || a.Admission == nil || a.State != "active" || id.Finished || id.Model != a.Model || id.SessionID != result.SessionID {
		return fmt.Errorf("SDK known terminal cost differs from original admission")
	}
	if err = validateManagedSDKResponseTotals(commitCtx, tx, inv, result); err != nil {
		return err
	}
	var existing []byte
	err = tx.QueryRowContext(commitCtx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, inv).Scan(&existing)
	if err == nil {
		// Response-total validation above already matched this incomplete
		// original receipt. Preserve its exact bytes/delta on repeated delivery.
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Reuse exact final-cost validation inside this transaction, then restore
	// the prior baseline and unfinished identity before committing known cost.
	// A SAVEPOINT keeps the completion helper's mutation set explicit and atomic.
	if _, err = tx.ExecContext(commitCtx, `SAVEPOINT managed_known_cost`); err != nil {
		return err
	}
	if err = observeManagedSDKFinalTx(commitCtx, tx, id, result); err != nil {
		return err
	}
	var raw []byte
	if err = tx.QueryRowContext(commitCtx, `SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, inv).Scan(&raw); err != nil {
		return err
	}
	var known UsageObservation
	if err = decodeJSONWithNumbers(raw, &known); err != nil {
		return err
	}
	if _, err = tx.ExecContext(commitCtx, `ROLLBACK TO managed_known_cost`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(commitCtx, `RELEASE managed_known_cost`); err != nil {
		return err
	}
	known.Complete = false
	known.CostIncomplete = true
	raw, err = json.Marshal(known)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(commitCtx, `INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,'final',?) ON CONFLICT(invocation_id,observation_key) DO UPDATE SET record=excluded.record`, inv, raw); err != nil {
		return err
	}
	return tx.Commit()
}
