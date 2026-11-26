package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

const ClaudeChildUsageBasis = "claude-sdk@0.3.322/sonnet46-one-read-child-v1/modelUsage"

var ErrUsageCounterConflict = errors.New("original native skill session usage counter conflicts with its retained highwater")

// This opt-in path is limited to the exact native surface whose modelUsage
// counters were observed to survive fresh-process resume. It does not change
// the older provider counter or cost interpretations.
func observeSkillUsageTx(ctx context.Context, tx *sql.Tx, identity UsageIdentity, update UsageUpdate) error {
	if update.CounterBasis != ClaudeChildUsageBasis || update.Provider != "claude-sdk" || update.Model != "claude-sonnet-4-6" || update.SessionID == "" || !update.Final {
		return fmt.Errorf("unsupported native skill usage counter basis or identity")
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT record FROM usage_invocations WHERE id=? AND origin_id=?", identity.ID, identity.RunID).Scan(&raw); err != nil {
		return err
	}
	var current UsageIdentity
	if err := decodeJSONWithNumbers(raw, &current); err != nil {
		return err
	}
	if current.Kind != "worker" || current.WorkerID == "" || current.WorkerID != identity.WorkerID || current.Kind != identity.Kind || current.TaskAttemptID != identity.TaskAttemptID || current.SessionID != "" && current.SessionID != update.SessionID || current.Provider != "unknown" && current.Provider != update.Provider || current.Model != "unknown" && current.Model != update.Model {
		return fmt.Errorf("native skill usage ownership changed")
	}
	current.Provider, current.Model, current.SessionID, current.Finished = update.Provider, update.Model, update.SessionID, true
	observation := NormalizeUsage(update)
	if observation.Scope != "session" {
		return fmt.Errorf("native skill usage requires cumulative session totals")
	}
	observation.CounterBasis = update.CounterBasis
	observation.CostIncomplete = update.Incomplete || observation.CostUSD == nil
	if observation.CostIncomplete {
		observation.Complete = false
	}
	observation.ReceiptFingerprint = actionHash([]any{update.CounterBasis, update.Provider, update.Model, update.SessionID, update.Usage, update.CostUSD, update.Final, update.Incomplete})
	receiptKey := "native-receipt:" + observation.ReceiptFingerprint
	var recorded []byte
	if err := tx.QueryRowContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?", identity.ID, receiptKey).Scan(&recorded); err == nil {
		var original UsageObservation
		if err = decodeJSONWithNumbers(recorded, &original); err != nil {
			return err
		}
		if original.CounterReset {
			return ErrUsageCounterConflict
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var prior UsageObservation
	var old []byte
	err := tx.QueryRowContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'", identity.ID).Scan(&old)
	if err == nil {
		if err := decodeJSONWithNumbers(old, &prior); err != nil {
			return err
		}
		if prior.CounterBasis != update.CounterBasis {
			return fmt.Errorf("native skill invocation usage basis changed")
		}
		if prior.ReceiptFingerprint == observation.ReceiptFingerprint {
			if prior.CounterReset {
				return ErrUsageCounterConflict
			}
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	scope := current.Provider + "\x00" + current.WorkerID + "\x00" + current.SessionID + "\x00" + update.CounterBasis
	var previous UsageObservation
	err = tx.QueryRowContext(ctx, "SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?", current.RunID, scope).Scan(&old)
	if err == nil {
		if err := decodeJSONWithNumbers(old, &previous); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Preserve already-reported invocation partials when a later cumulative
	// receipt omits or understates a field. Derive the original session baseline
	// by removing this invocation's prior delta from the session highwater.
	partial, err := skillInvocationPartialFloorTx(ctx, tx, identity.ID)
	if err != nil {
		return err
	}
	retainFloor := func(now **int64, before, charged, floor *int64) {
		if floor == nil {
			return
		}
		base := int64(0)
		if before != nil {
			base = *before
			if charged != nil {
				base -= *charged
			}
			if base < 0 {
				base = 0
			}
		}
		if *floor > math.MaxInt64-base {
			observation.Complete = false
			observation.CostIncomplete = true
			return
		}
		required := base + *floor
		if *now == nil || **now < required {
			*now = &required
			observation.Complete = false
			observation.CostIncomplete = true
		}
	}
	retainFloor(&observation.InputTokens, previous.InputTokens, prior.DeltaInputTokens, partial.InputTokens)
	retainFloor(&observation.CachedInputTokens, previous.CachedInputTokens, prior.DeltaCachedInputTokens, partial.CachedInputTokens)
	retainFloor(&observation.OutputTokens, previous.OutputTokens, prior.DeltaOutputTokens, partial.OutputTokens)
	conflict := previous.CounterReset
	delta := func(now **int64, before, charged *int64) *int64 {
		if *now == nil {
			*now = before
			return charged
		}
		n := **now
		if before != nil {
			if n < *before {
				conflict = true
				*now = before
				n = 0
			} else {
				n -= *before
			}
		}
		if charged != nil {
			if n > math.MaxInt64-*charged {
				conflict = true
				return charged
			}
			n += *charged
		}
		return &n
	}
	observation.DeltaInputTokens = delta(&observation.InputTokens, previous.InputTokens, prior.DeltaInputTokens)
	observation.DeltaCachedInputTokens = delta(&observation.CachedInputTokens, previous.CachedInputTokens, prior.DeltaCachedInputTokens)
	observation.DeltaOutputTokens = delta(&observation.OutputTokens, previous.OutputTokens, prior.DeltaOutputTokens)
	if observation.CostUSD == nil {
		observation.CostUSD = previous.CostUSD
		observation.DeltaCostUSD = prior.DeltaCostUSD
	} else {
		n := *observation.CostUSD
		if previous.CostUSD != nil {
			if n < *previous.CostUSD {
				conflict = true
				observation.CostUSD = previous.CostUSD
				n = 0
			} else {
				n -= *previous.CostUSD
			}
		}
		if prior.DeltaCostUSD != nil {
			n += *prior.DeltaCostUSD
		}
		if math.IsInf(n, 0) || math.IsNaN(n) {
			conflict = true
			observation.DeltaCostUSD = prior.DeltaCostUSD
		} else {
			observation.DeltaCostUSD = &n
		}
	}
	observation.CounterReset = conflict
	if conflict {
		observation.Complete = false
		observation.CostIncomplete = true
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO usage_session_counters VALUES(?,?,?) ON CONFLICT(origin_id,scope_key) DO UPDATE SET record=excluded.record", current.RunID, scope, data); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,'final',?) ON CONFLICT(invocation_id,observation_key) DO UPDATE SET record=excluded.record", identity.ID, data); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO usage_observations(invocation_id,observation_key,record) VALUES(?,?,?)", identity.ID, receiptKey, data); err != nil {
		return err
	}
	raw, err = json.Marshal(current)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE usage_invocations SET record=? WHERE id=?", raw, identity.ID); err != nil {
		return err
	}
	if conflict {
		return ErrUsageCounterConflict
	}
	return nil
}

func skillInvocationPartialFloorTx(ctx context.Context, tx *sql.Tx, id string) (UsageObservation, error) {
	rows, err := tx.QueryContext(ctx, "SELECT record FROM usage_observations WHERE invocation_id=?", id)
	if err != nil {
		return UsageObservation{}, err
	}
	defer rows.Close()
	floor := UsageObservation{}
	for rows.Next() {
		var raw []byte
		var o UsageObservation
		if err = rows.Scan(&raw); err != nil {
			return floor, err
		}
		if err = decodeJSONWithNumbers(raw, &o); err != nil {
			return floor, err
		}
		if o.Final {
			continue
		}
		if o.Scope == "session" {
			floor.InputTokens = maxTokens(floor.InputTokens, o.DeltaInputTokens)
			floor.CachedInputTokens = maxTokens(floor.CachedInputTokens, o.DeltaCachedInputTokens)
			floor.OutputTokens = maxTokens(floor.OutputTokens, o.DeltaOutputTokens)
			continue
		}
		if !addTokens(&floor.InputTokens, o.InputTokens) || !addTokens(&floor.CachedInputTokens, o.CachedInputTokens) || !addTokens(&floor.OutputTokens, o.OutputTokens) {
			return floor, fmt.Errorf("native skill partial accounting floor overflow")
		}
	}
	return floor, rows.Err()
}
