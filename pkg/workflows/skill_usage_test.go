package workflows

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func childUsage(input, output int, cost any) UsageUpdate {
	return UsageUpdate{CounterBasis: ClaudeChildUsageBasis, Provider: "claude-sdk", Model: "claude-sonnet-4-6", SessionID: "original-session", Final: true, Usage: map[string]any{"total": map[string]any{"input_tokens": input, "output_tokens": output, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}, CostUSD: cost}
}

func childUsageIdentity(t *testing.T, s *SQLiteStore, run, id string) UsageIdentity {
	t.Helper()
	identity := UsageIdentity{ID: id, RunID: run, WorkerID: "original-worker", Kind: "worker", Provider: "claude-sdk", Model: "claude-sonnet-4-6", SessionID: "original-session"}
	if err := s.BeginUsage(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestSkillUsageCumulativeResumeAndOldReceiptDoNotDoubleCharge(t *testing.T) {
	s, run := executionStore(t)
	first := childUsageIdentity(t, s, run.ID, "original-turn")
	original := childUsage(80, 15, 0.000465)
	if err := s.ObserveUsage(t.Context(), first, original); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	second := childUsageIdentity(t, resumed, run.ID, "continued-turn")
	if err := resumed.ObserveUsage(t.Context(), second, childUsage(96, 18, 0.000558)); err != nil {
		t.Fatal(err)
	}
	// The earlier original receipt can be replayed after a newer process report.
	for range 2 {
		if err := resumed.ObserveUsage(t.Context(), first, original); err != nil {
			t.Fatal(err)
		}
	}
	report, err := resumed.Usage(t.Context(), run.ID)
	if err != nil || !report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 96 || *report.Summary.OutputTokens != 18 || report.Summary.CostUSD == nil || math.Abs(*report.Summary.CostUSD-0.000558) > 1e-12 {
		t.Fatal("cumulative native resume double charged/reset", report, err)
	}
	if *report.Invocations[0].Observations["final"].DeltaCostUSD != 0.000465 || math.Abs(*report.Invocations[1].Observations["final"].DeltaCostUSD-0.000093) > 1e-12 {
		t.Fatal("original invocation deltas changed", report.Invocations)
	}
}

func TestSkillUsageCounterConflictCommitsKnownChargesBeforeReturningFailure(t *testing.T) {
	s, run := executionStore(t)
	identity := childUsageIdentity(t, s, run.ID, "original-turn")
	if err := s.ObserveUsage(t.Context(), identity, childUsage(80, 15, 0.000465)); err != nil {
		t.Fatal(err)
	}
	err := s.ObserveUsage(t.Context(), identity, childUsage(64, 12, 0.000372))
	if !errors.Is(err, ErrUsageCounterConflict) {
		t.Fatal("conflicting decrease did not fail", err)
	}
	reopened, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.Usage(t.Context(), run.ID)
	if err != nil || report.Summary.Complete || report.Summary.CostComplete || report.Summary.KnownCostUSD == nil || *report.Summary.KnownCostUSD != 0.000465 {
		t.Fatal("API error rolled back conflict or known charges", report, err)
	}
	observation := report.Invocations[0].Observations["final"]
	if !observation.CounterReset || observation.InputTokens == nil || *observation.InputTokens != 80 || *observation.DeltaInputTokens != 80 {
		t.Fatal("conflict lowered original highwater", observation)
	}
	var raw []byte
	if err := reopened.db.QueryRow("SELECT record FROM usage_session_counters WHERE origin_id=?", run.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var counter UsageObservation
	if err := json.Unmarshal(raw, &counter); err != nil || *counter.InputTokens != 80 || *counter.CostUSD != 0.000465 || !counter.CounterReset {
		t.Fatal("durable highwater was lowered", counter, err)
	}
}

func TestSkillUsageMissingCostRetainsChargesAndLateCompletionFillsThem(t *testing.T) {
	s, run := executionStore(t)
	identity := childUsageIdentity(t, s, run.ID, "original-turn")
	for _, update := range []UsageUpdate{childUsage(80, 15, nil), childUsage(80, 15, 0.000465), childUsage(96, 18, nil)} {
		if err := s.ObserveUsage(t.Context(), identity, update); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Usage(t.Context(), run.ID)
	if err != nil || report.Summary.Complete || report.Summary.CostComplete || report.Summary.KnownCostUSD == nil || *report.Summary.KnownCostUSD != 0.000465 {
		t.Fatal("missing cost erased known charges", report, err)
	}
	if err := s.ObserveUsage(t.Context(), identity, childUsage(96, 18, 0.000558)); err != nil {
		t.Fatal(err)
	}
	// Replaying an older incomplete receipt must not undo the later complete
	// accounting or lower the original session's known highwater.
	if err := s.ObserveUsage(t.Context(), identity, childUsage(80, 15, nil)); err != nil {
		t.Fatal(err)
	}
	report, err = s.Usage(t.Context(), run.ID)
	if err != nil || !report.Summary.Complete || !report.Summary.CostComplete || *report.Summary.InputTokens != 96 || *report.Summary.CostUSD != 0.000558 {
		t.Fatal("late original accounting double charged or stayed unknown", report, err)
	}
}

func TestSkillChildPartialReceiptsSurviveRestartAndAreSuperseded(t *testing.T) {
	s, run := executionStore(t)
	plan, ledger, f := childLedgerFixture(t, "selected-child")
	for _, o := range f.Observations {
		if err := ledger.ObserveChild(plan, o); err != nil {
			t.Fatal(err)
		}
	}
	partial, ok := skillChildPartialUsage(plan, ledger)
	if !ok || *tokenValue(partial.Usage["input_tokens"]) != 32 || *tokenValue(partial.Usage["output_tokens"]) != 3 {
		t.Fatal("child messages/result double charged", partial)
	}
	first := UsageIdentity{ID: "original-turn", RunID: run.ID, WorkerID: "original-worker", Kind: "worker", Provider: partial.Provider, Model: partial.Model, SessionID: partial.SessionID}
	if err := s.BeginUsage(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	parent := UsageUpdate{Key: "original-parent", Usage: map[string]any{"input_tokens": 48, "output_tokens": 9}, Incomplete: true}
	for _, u := range []UsageUpdate{parent, partial, partial} {
		if err := s.ObserveUsage(t.Context(), first, u); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.Usage(t.Context(), run.ID)
	if err != nil || report.Summary.Complete || *report.Summary.InputTokens != 80 || *report.Summary.OutputTokens != 12 {
		t.Fatal("original partials were lost or duplicated", report, err)
	}
	complete := childUsage(80, 15, 0.000465)
	complete.SessionID = f.Session
	if err = reopened.ObserveUsage(t.Context(), first, complete); err != nil {
		t.Fatal(err)
	}
	continued := first
	continued.ID = "continued-turn"
	if err = reopened.BeginUsage(t.Context(), continued); err != nil {
		t.Fatal(err)
	}
	next := childUsage(96, 18, 0.000558)
	next.SessionID = f.Session
	if err = reopened.ObserveUsage(t.Context(), continued, next); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = reopened.ObserveUsage(t.Context(), first, partial); err != nil {
			t.Fatal(err)
		}
	}
	if err = reopened.ObserveUsage(t.Context(), first, complete); err != nil {
		t.Fatal(err)
	}
	report, err = reopened.Usage(t.Context(), run.ID)
	if err != nil || !report.Summary.Complete || *report.Summary.InputTokens != 96 || *report.Summary.OutputTokens != 18 || math.Abs(*report.Summary.CostUSD-0.000558) > 1e-12 {
		t.Fatal("late aggregate/old partial reset or doubled original charges", report, err)
	}
}

func TestSkillChildIncompleteAccountingCommitsEveryKnownFloor(t *testing.T) {
	s, run := executionStore(t)
	_, _, f := childLedgerFixture(t, "selected-child")
	var raw map[string]any
	for _, o := range f.Observations {
		if o.Kind == "parent_terminal" {
			raw = childRaw(o)
		}
	}
	childObject(raw["usage"])["input_tokens"] = 100
	delete(childObject(childObject(raw["modelUsage"])["claude-sonnet-4-6"]), "outputTokens")
	childObject(raw["modelUsage"])["unexpected-model"] = nil
	delete(raw, "total_cost_usd")
	update, problem := SkillChildUsage(raw)
	if problem == nil || !update.Incomplete {
		t.Fatal("conflicting receipt completed", update, problem)
	}
	identity := UsageIdentity{ID: "original-turn", RunID: run.ID, WorkerID: "original-worker", Kind: "worker", Provider: update.Provider, Model: update.Model, SessionID: update.SessionID}
	if err := s.BeginUsage(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveUsage(t.Context(), identity, update); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	report, err := reopened.Usage(t.Context(), run.ID)
	if err != nil || report.Summary.Complete || report.Summary.CostComplete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 100 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 9 || report.Summary.KnownCostUSD == nil || math.Abs(*report.Summary.KnownCostUSD-0.000465) > 1e-12 {
		t.Fatal("incomplete accounting discarded known floors after reopen", report, err)
	}
}
