package workflows

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

func TestManagedSDKCumulativeCostOriginalSettlement(t *testing.T) {
	store, ctx, id := sdkAttemptFixture(t)
	id.SessionID = "cost-session"
	raw, _ := json.Marshal(id)
	if _, err := store.db.Exec(`UPDATE usage_invocations SET record=? WHERE id=?`, raw, id.ID); err != nil {
		t.Fatal(err)
	}
	makeResult := func(cost any) SessionResult {
		return SessionResult{SessionID: id.SessionID, Outputs: map[string]any{"usage": map[string]any{"input_tokens": 16, "output_tokens": 5}, "cost_usd": cost}}
	}
	first := makeResult(0.000123)
	settle := func(identity UsageIdentity, result SessionResult, commit bool) error {
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = observeManagedSDKFinalTx(ctx, tx, identity, result); err != nil {
			return err
		}
		if commit {
			return tx.Commit()
		}
		return nil
	}
	if err := settle(id, first, true); err != nil {
		t.Fatal(err)
	}
	secondID := id
	secondID.ID = "continuation"
	secondID.TaskAttemptID = "continuation"
	if err := store.BeginUsage(ctx, secondID); err != nil {
		t.Fatal(err)
	}
	second := makeResult(0.000246)
	baseline := func() []byte {
		var b []byte
		if err := store.db.QueryRow(`SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, "claude-sdk\x00"+id.SessionID+"\x00"+managedSDKCostBasis).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	old := string(baseline())
	if err := settle(secondID, second, false); err != nil {
		t.Fatal(err)
	}
	if string(baseline()) != old {
		t.Fatal("aborted completion advanced cost highwater")
	}
	var finals int
	if err := store.db.QueryRow(`SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, secondID.ID).Scan(&finals); err != nil || finals != 0 {
		t.Fatal("aborted completion published final", finals, err)
	}
	if err := settle(secondID, second, true); err != nil {
		t.Fatal(err)
	}
	committed := string(baseline())
	for _, pair := range []struct {
		id UsageIdentity
		r  SessionResult
	}{{id, first}, {secondID, second}} {
		if err := settle(pair.id, pair.r, true); err != nil {
			t.Fatal("original receipt replay failed", err)
		}
	}
	if string(baseline()) != committed {
		t.Fatal("original replay changed highwater")
	}
	report, err := store.Usage(ctx, id.RunID)
	if err != nil || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000246) > 1e-12 {
		t.Fatal("cumulative native costs double charged", report, err)
	}
	thirdID := id
	thirdID.ID = "third"
	if err := store.BeginUsage(ctx, thirdID); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []any{-1.0, math.Inf(1), math.NaN(), "0.001", nil, 0.0001} {
		if err := settle(thirdID, makeResult(bad), true); err == nil {
			t.Fatalf("invalid/reset native cost accepted: %v", bad)
		}
		if string(baseline()) != committed {
			t.Fatal("cost rejection lost prior highwater")
		}
	}
	if err := settle(id, makeResult(0.000124), true); err == nil {
		t.Fatal("changed old original cost accepted")
	}
	changed := secondID
	changed.Model = "other-model"
	if err := settle(changed, second, true); err == nil {
		t.Fatal("cost model changed within original session")
	}
	if _, err := store.db.Exec(`UPDATE usage_session_counters SET record='{"profile":"wrong"}' WHERE origin_id=?`, id.RunID); err != nil {
		t.Fatal(err)
	}
	if err := settle(thirdID, makeResult(0.000369), true); err == nil {
		t.Fatal("malformed counter accepted")
	}
	report, err = store.Usage(context.WithoutCancel(ctx), id.RunID)
	if err != nil || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000246) > 1e-12 {
		t.Fatal("cost hold erased earlier original charges", report, err)
	}
}

func TestManagedSDKKnownTerminalCostSurvivesCancellationWithoutSettlement(t *testing.T) {
	store, ctx, id := sdkAttemptFixture(t)
	if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
	if err := store.ObserveUsage(ctx, id, UsageUpdate{Key: "sdk-response:original", Provider: "claude-sdk", Model: "fixture", SessionID: "session", Usage: usage, Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	id.SessionID = "session"
	baselineKey := "claude-sdk\x00session\x00" + managedSDKCostBasis
	total := 0.000123
	baseline := managedSDKCostCounter{Profile: managedSDKCostBasis, Model: id.Model, SessionID: id.SessionID, InvocationID: "settled-before", Receipt: "known-before", TotalUSD: &total}
	raw, _ := json.Marshal(baseline)
	if _, err := store.db.Exec(`INSERT INTO usage_session_counters VALUES(?,?,?)`, id.RunID, baselineKey, raw); err != nil {
		t.Fatal(err)
	}
	baselineBefore := string(raw)
	result := SessionResult{Status: RunStatusSucceeded, ProviderStatus: "success", ProviderVersion: "claude-agent-sdk@0.3.322", SessionID: id.SessionID, Outputs: map[string]any{"text": "known output", "usage": usage, "cost_usd": 0.000246}}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for range 2 {
		if err := RecordManagedSDKTerminalCost(cancelled, id.RunID, id.ID, result); err != nil {
			t.Fatal(err)
		}
	}
	var current []byte
	if err := store.db.QueryRow(`SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, baselineKey).Scan(&current); err != nil || string(current) != baselineBefore {
		t.Fatal("known charge advanced session baseline", string(current), err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, attempt, err := readManagedSDKAttempt(ctx, tx, id.ID)
	if err != nil || attempt.State != "active" {
		t.Fatal("known charge settled original attempt", attempt, err)
	}
	tx.Rollback()
	var identityRaw []byte
	if err = store.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, id.ID).Scan(&identityRaw); err != nil {
		t.Fatal(err)
	}
	var identity UsageIdentity
	if err = json.Unmarshal(identityRaw, &identity); err != nil || identity.Finished {
		t.Fatal("known charge finished original invocation", identity, err)
	}
	report, err := store.Usage(ctx, id.RunID)
	if err != nil || report.Summary.Complete || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000123) > 1e-12 {
		t.Fatal("known original delta lost/double counted", report, err)
	}
	changed := result
	changed.Outputs = cloneJSONMap(result.Outputs)
	changed.Outputs["cost_usd"] = 0.000247
	if err = RecordManagedSDKTerminalCost(ctx, id.RunID, id.ID, changed); err == nil {
		t.Fatal("changed original cost accepted")
	}
	// A later independently settled original cannot change this original's
	// previously captured delta when its exact completion is reconciled.
	later := 0.000369
	baseline.TotalUSD = &later
	baseline.InvocationID = "settled-later"
	raw, _ = json.Marshal(baseline)
	if _, err = store.db.Exec(`UPDATE usage_session_counters SET record=? WHERE origin_id=? AND scope_key=?`, raw, id.RunID, baselineKey); err != nil {
		t.Fatal(err)
	}
	laterBaseline := string(raw)
	for _, commit := range []bool{false, true, true} {
		tx, err = store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = observeManagedSDKFinalTx(ctx, tx, id, result); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = store.db.QueryRow(`SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, baselineKey).Scan(&current); err != nil || string(current) != laterBaseline {
			t.Fatal("older settlement changed later baseline", string(current), err)
		}
	}
	report, err = store.Usage(ctx, id.RunID)
	if err != nil || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000123) > 1e-12 {
		t.Fatal("success upgrade changed original known delta", report, err)
	}
}
