package workflows

import (
	"context"
	"encoding/json"
	"fmt"
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
	for _, annotated := range []bool{false, true} {
		t.Run(fmt.Sprint("provider-annotated=", annotated), func(t *testing.T) {
			testManagedSDKKnownTerminalCost(t, annotated)
		})
	}
}

func testManagedSDKKnownTerminalCost(t *testing.T, annotated bool) {
	store, ctx, id := sdkAttemptFixture(t)
	if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, "fixture"); err != nil {
		t.Fatal(err)
	}
	usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 2}
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
	if annotated {
		result.Outputs["provider"] = "claude-sdk"
		result.Outputs["model"] = id.Model
		result.Outputs["session_id"] = id.SessionID
	}
	callerBefore, err := json.Marshal(result.Outputs)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for range 2 {
		if err := RecordManagedSDKTerminalCost(cancelled, id.RunID, id.ID, result); err != nil {
			t.Fatal(err)
		}
	}
	var knownRaw []byte
	if err = store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&knownRaw); err != nil {
		t.Fatal(err)
	}
	var known UsageObservation
	if err = json.Unmarshal(knownRaw, &known); err != nil || known.InputTokens == nil || *known.InputTokens != 21 || known.CachedInputTokens == nil || *known.CachedInputTokens != 3 || known.OutputTokens == nil || *known.OutputTokens != 5 || known.Complete || !known.CostIncomplete {
		t.Fatal("known cached-token receipt changed", known, err)
	}
	var current []byte
	if err := store.db.QueryRow(`SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, baselineKey).Scan(&current); err != nil || string(current) != baselineBefore {
		t.Fatal("known charge advanced session baseline", string(current), err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
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
	for _, field := range []string{"cost_usd", "input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens", "provider", "session"} {
		changed := result
		changed.Outputs = cloneJSONMap(result.Outputs)
		switch field {
		case "cost_usd":
			changed.Outputs[field] = 0.000247
		case "provider":
			changed.Outputs[field] = "codex-app-server"
		case "session":
			changed.SessionID = "another-session"
		default:
			changed.Outputs["usage"].(map[string]any)[field] = usage[field].(int) + 1
		}
		if err = RecordManagedSDKTerminalCost(ctx, id.RunID, id.ID, changed); err == nil {
			t.Fatal("changed original terminal accepted", field)
		}
		var retained []byte
		if err = store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&retained); err != nil || string(retained) != string(knownRaw) {
			t.Fatal("rejected terminal changed original receipt", field, err)
		}
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
	manager := &WorkerManager{Store: store, Limits: DefaultWorkerLimits()}
	worker, err := manager.Get(ctx, id.RunID, id.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := manager.Messages(ctx, id.RunID, worker.ID)
	if err != nil || len(messages) != 1 {
		t.Fatal(messages, err)
	}
	ctx = context.WithValue(ctx, managedSDKTerminalKey{}, manager.sdkTerminalRecorder(worker, messages[0], nil))
	if err = CheckpointManagedSDKTerminal(ctx, result); err != nil {
		t.Fatal("unchanged cached terminal could not checkpoint", err)
	}
	message := messages[0]
	message.Status = RunStatusSucceeded
	for phase, commit := range []bool{false, true, true} {
		tx, err = store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if phase < 2 {
			err = settleManagedSDKAttempt(ctx, tx, worker, message, result)
		} else {
			err = observeManagedSDKFinalTx(ctx, tx, id, result)
		}
		if err != nil {
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
		var finalRaw, attemptRaw, identityRaw []byte
		if err = store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, id.ID).Scan(&finalRaw); err != nil {
			t.Fatal(err)
		}
		if err = store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key=?`, id.ID, managedSDKAttemptKey).Scan(&attemptRaw); err != nil {
			t.Fatal(err)
		}
		if err = store.db.QueryRow(`SELECT record FROM usage_invocations WHERE id=?`, id.ID).Scan(&identityRaw); err != nil {
			t.Fatal(err)
		}
		var final UsageObservation
		var marker managedSDKAttemptRow
		var currentIdentity UsageIdentity
		if err = json.Unmarshal(finalRaw, &final); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(attemptRaw, &marker); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(identityRaw, &currentIdentity); err != nil {
			t.Fatal(err)
		}
		wantState := "terminal"
		if commit {
			wantState = "settled"
		}
		if final.Complete != commit || final.CostIncomplete == commit || currentIdentity.Finished != commit || marker.Attempt == nil || marker.Attempt.State != wantState || currentIdentity.Provider != id.Provider || currentIdentity.Model != id.Model || currentIdentity.SessionID != id.SessionID {
			t.Fatal("cached settlement was not atomic", phase, final, currentIdentity, marker.Attempt)
		}
		if final.InputTokens == nil || *final.InputTokens != 21 || final.CachedInputTokens == nil || *final.CachedInputTokens != 3 || final.OutputTokens == nil || *final.OutputTokens != 5 {
			t.Fatal("settlement changed cached-token totals", phase, final)
		}
		if !commit && string(finalRaw) != string(knownRaw) {
			t.Fatal("rolled back settlement changed known receipt")
		}
		if err = store.db.QueryRow(`SELECT record FROM usage_session_counters WHERE origin_id=? AND scope_key=?`, id.RunID, baselineKey).Scan(&current); err != nil || string(current) != laterBaseline {
			t.Fatal("older settlement changed later baseline", string(current), err)
		}
	}
	callerAfter, err := json.Marshal(result.Outputs)
	if err != nil || string(callerAfter) != string(callerBefore) {
		t.Fatal("terminal normalization mutated caller outputs", err)
	}
	report, err = store.Usage(ctx, id.RunID)
	if err != nil || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000123) > 1e-12 {
		t.Fatal("success upgrade changed original known delta", report, err)
	}
}

func TestManagedSDKTerminalRejectsOutputIdentitySubstitution(t *testing.T) {
	for _, stage := range []string{"first-known", "repeated-known", "private-checkpoint", "final-settlement"} {
		for _, field := range []string{"provider", "model", "session_id"} {
			t.Run(stage+"/"+field, func(t *testing.T) {
				store, ctx, id := sdkAttemptFixture(t)
				if err := BeginManagedSDKInvocation(ctx, id.RunID, id.ID, id.Model); err != nil {
					t.Fatal(err)
				}
				// Zero cache counters ensure provider identity is checked independently
				// of provider-specific token arithmetic.
				usage := map[string]any{"input_tokens": 16, "output_tokens": 5, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
				if err := store.ObserveUsage(ctx, id, UsageUpdate{Key: "sdk-response:original", Provider: id.Provider, Model: id.Model, SessionID: "session", Usage: usage, Incomplete: true}); err != nil {
					t.Fatal(err)
				}
				id.SessionID = "session"
				result := SessionResult{Status: RunStatusSucceeded, ProviderStatus: "success", ProviderVersion: "claude-agent-sdk@0.3.322", SessionID: id.SessionID, Outputs: map[string]any{"usage": usage, "cost_usd": 0.000123}}
				if stage == "repeated-known" {
					if err := RecordManagedSDKTerminalCost(ctx, id.RunID, id.ID, result); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := func() string {
					t.Helper()
					var raw string
					if err := store.db.QueryRow(`SELECT json_group_array(CAST(record AS TEXT)) FROM (
 SELECT record FROM usage_invocations WHERE id=? UNION ALL
 SELECT record FROM usage_observations WHERE invocation_id=? UNION ALL
 SELECT record FROM usage_session_counters WHERE origin_id=?)`, id.ID, id.ID, id.RunID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					return raw
				}
				before := snapshot()
				result.Outputs[field] = "foreign"
				callerBefore, err := json.Marshal(result.Outputs)
				if err != nil {
					t.Fatal(err)
				}
				switch stage {
				case "first-known", "repeated-known":
					err = RecordManagedSDKTerminalCost(ctx, id.RunID, id.ID, result)
				case "private-checkpoint":
					manager := &WorkerManager{Store: store, Limits: DefaultWorkerLimits()}
					worker, loadErr := manager.Get(ctx, id.RunID, id.WorkerID)
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					messages, loadErr := manager.Messages(ctx, id.RunID, worker.ID)
					if loadErr != nil || len(messages) != 1 {
						t.Fatal(messages, loadErr)
					}
					ctx = context.WithValue(ctx, managedSDKTerminalKey{}, manager.sdkTerminalRecorder(worker, messages[0], nil))
					err = CheckpointManagedSDKTerminal(ctx, result)
				case "final-settlement":
					tx, beginErr := store.db.BeginTx(ctx, nil)
					if beginErr != nil {
						t.Fatal(beginErr)
					}
					defer tx.Rollback()
					err = observeManagedSDKFinalTx(ctx, tx, id, result)
					if err == nil {
						if commitErr := tx.Commit(); commitErr != nil {
							t.Fatal(commitErr)
						}
					} else if rollbackErr := tx.Rollback(); rollbackErr != nil {
						t.Fatal(rollbackErr)
					}
				}
				if err == nil {
					t.Fatal("changed output identity accepted", stage, field)
				}
				if snapshot() != before {
					t.Fatal("identity rejection changed accounting, admission or baseline")
				}
				callerAfter, marshalErr := json.Marshal(result.Outputs)
				if marshalErr != nil || string(callerBefore) != string(callerAfter) || result.SessionID != id.SessionID {
					t.Fatal("identity validation mutated caller result", marshalErr)
				}
			})
		}
	}
}
