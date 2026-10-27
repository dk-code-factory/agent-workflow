package workflows

import (
	"context"
	"errors"
	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"math"
	"testing"
)

func usageTokens(input, output int) map[string]any {
	return map[string]any{"input_tokens": input, "output_tokens": output}
}
func TestUsageNormalizationIncludesClaudeCacheAndUnknownCost(t *testing.T) {
	value := NormalizeUsage(UsageUpdate{Provider: "claude-sdk", Usage: map[string]any{"input_tokens": 100, "cache_read_input_tokens": 20, "cache_creation_input_tokens": 10, "output_tokens": 5}, Final: true, CostUSD: 0.0})
	if value.InputTokens == nil || *value.InputTokens != 130 || *value.CachedInputTokens != 20 || *value.OutputTokens != 5 || value.CostUSD == nil || *value.CostUSD != 0 || !value.Complete {
		t.Fatalf("bad normalization %+v", value)
	}
	unknown := NormalizeUsage(UsageUpdate{Provider: "codex-app-server", Usage: usageTokens(30, 2), Final: true})
	if unknown.CostUSD != nil || !unknown.Complete {
		t.Fatalf("unknown subscription cost became zero: %+v", unknown)
	}
}

func TestUsageObservationsReplaceCountersAndRetainLostFinalUsage(t *testing.T) {
	store, run := executionStore(t)
	ctx := context.Background()
	identity := UsageIdentity{ID: "task", RunID: run.ID, Kind: "task", Provider: "claude-sdk", Model: "fixture"}
	if err := store.BeginUsage(ctx, identity); err != nil {
		t.Fatal(err)
	}
	for _, update := range []UsageUpdate{{Key: "message-one", Usage: usageTokens(10, 1)}, {Key: "message-one", Usage: usageTokens(12, 2)}, {Key: "message-two", Usage: usageTokens(15, 3)}, {Key: "final", Final: true, Incomplete: true}} {
		if err := store.ObserveUsage(ctx, identity, update); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.InputTokens == nil || *report.Summary.InputTokens != 27 || *report.Summary.OutputTokens != 5 || report.Summary.Complete || report.Summary.CostUSD != nil {
		t.Fatalf("partial observations double-counted or erased: %+v", report.Summary)
	}
	if err = store.ObserveUsage(ctx, identity, UsageUpdate{Key: "final", Final: true, Usage: usageTokens(30, 6), CostUSD: 0.25}); err != nil {
		t.Fatal(err)
	}
	report, err = store.Usage(ctx, run.ID)
	if err != nil || *report.Summary.InputTokens != 30 || !report.Summary.Complete || *report.Summary.CostUSD != 0.25 {
		t.Fatal(report, err)
	}
}

func TestSessionCumulativeCountersAreNotSummedPerTurn(t *testing.T) {
	store, run := executionStore(t)
	ctx := context.Background()
	for index, total := range []int{100, 140} {
		id := []string{"turn-one", "turn-two"}[index]
		identity := UsageIdentity{ID: id, RunID: run.ID, Kind: "worker", WorkerID: "worker", Provider: "codex-app-server", Model: "fixture", SessionID: "session"}
		if err := store.BeginUsage(ctx, identity); err != nil {
			t.Fatal(err)
		}
		if err := store.ObserveUsage(ctx, identity, UsageUpdate{Key: "final", Final: true, Usage: map[string]any{"last": map[string]any{"inputTokens": 12, "outputTokens": 1}, "total": map[string]any{"inputTokens": total, "outputTokens": total / 10, "cachedInputTokens": total / 2}}}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *report.Summary.InputTokens != 140 || *report.Summary.OutputTokens != 14 || *report.Summary.CachedInputTokens != 70 || report.Summary.Invocations != 2 {
		t.Fatalf("cumulative session snapshots were summed: %+v", report.Summary)
	}
}

func TestSessionObservationsAccumulateDeltasAcrossRequests(t *testing.T) {
	store, run := executionStore(t)
	ctx := context.Background()
	identity := UsageIdentity{ID: "worker-turn", RunID: run.ID, Kind: "worker", WorkerID: "w", Provider: "codex-app-server", SessionID: "thread"}
	if err := store.BeginUsage(ctx, identity); err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		key   string
		input int
		final bool
	}{{"turn", 100, false}, {"turn", 150, false}, {"final", 150, true}} {
		if err := store.ObserveUsage(ctx, identity, UsageUpdate{Key: value.key, Final: value.final, Usage: map[string]any{"total": map[string]any{"inputTokens": value.input, "outputTokens": 10}}}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil || *report.Summary.InputTokens != 150 || *report.Summary.OutputTokens != 10 {
		t.Fatal("cumulative updates repeated usage", report, err)
	}
}

func TestRepairsAndCheckpointReuseHaveDistinctUsageIdentities(t *testing.T) {
	tasks, repairs := 0, 0
	runner := agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		if !req.RepairOnly {
			tasks++
			return map[string]any{"text": "invalid", "usage": usageTokens(10, 1), "cost_usd": 0.01}, nil
		}
		repairs++
		text := "invalid"
		if repairs == 2 {
			text = `{"ok":true}`
		}
		return map[string]any{"text": text, "usage": usageTokens(repairs+1, 1), "cost_usd": float64(repairs+1) / 100}, nil
	})
	e, store := durableExecutor(t, `jobs:
  build:
    steps:
      - uses: agent/a
        with:
          prompt: task
          output:
            format: json
            repair-attempts: 1
            schema: {type: object, required: [ok], properties: {ok: {type: boolean}}}
`, runner)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("expected schema failure")
	}
	result, err := e.Resume(context.Background(), run.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.Usage(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || repairs != 2 || report.Summary.Invocations != 3 || *report.Summary.InputTokens != 15 || math.Abs(*report.Summary.CostUSD-0.06) > 1e-9 || result.Usage == nil {
		t.Fatalf("cached task usage repeated or repair omitted: tasks=%d repairs=%d %+v", tasks, repairs, report.Summary)
	}
}

func TestFailedProviderCallUsageIsCounted(t *testing.T) {
	e, store := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/a, with: {prompt: task}}]}}", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"usage": usageTokens(9, 2), "cost_usd": 0.02}, errors.New("provider failed after reporting usage")
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("expected failed call")
	}
	report, err := store.Usage(context.Background(), run.ID)
	if err != nil || *report.Summary.InputTokens != 9 || *report.Summary.CostUSD != 0.02 {
		t.Fatal(report, err)
	}
}

func TestMigrationBackfillsHistoricalProviderUsage(t *testing.T) {
	e, store := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/a, with: {prompt: task}}]}}", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": "done", "provider": "claude-cli", "usage": map[string]any{"input_tokens": 8, "cache_read_input_tokens": 2, "output_tokens": 1}, "cost_usd": 0.03}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("DROP TABLE iteration_attempts; DROP TABLE iteration_plans; DROP TABLE action_attempts; DROP TABLE usage_observations; DROP TABLE usage_session_counters; DROP TABLE usage_invocations; DROP TABLE retention_groups; ALTER TABLE executions DROP COLUMN finalizing; PRAGMA user_version=4;"); err != nil {
		t.Fatal(err)
	}
	lock, err := ownership.Own(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = store.Migrate(context.Background(), lock); err != nil {
		t.Fatal(err)
	}
	report, err := store.Usage(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Invocations != 1 || *report.Summary.InputTokens != 10 || *report.Summary.CostUSD != 0.03 {
		t.Fatalf("historical reported usage was discarded: %+v", report)
	}
}

func TestInterruptedCallRetainsDurablePartialUsage(t *testing.T) {
	e, store := durableExecutor(t, "jobs: {x: {steps: [{uses: agent/a, with: {prompt: task}}]}}", agentFunc(func(_ context.Context, request AgentRequest) (map[string]any, error) {
		if request.ObserveUsage == nil {
			return nil, errors.New("observer not installed")
		}
		if err := request.ObserveUsage(UsageUpdate{Key: "provider-message", Provider: "claude-sdk", Usage: usageTokens(17, 3)}); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("expected interrupted call")
	}
	view, err := OpenSQLiteInspection(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	report, err := view.Usage(context.Background(), run.ID)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 17 || report.Summary.Complete || report.Summary.CostUSD != nil {
		t.Fatalf("partial usage was lost or billed as zero: %+v %v", report, err)
	}
}

func TestAccountingOverflowRemainsExplicitUnknown(t *testing.T) {
	maximum := int64(math.MaxInt64)
	one := int64(1)
	cost := math.MaxFloat64
	items := []InvocationUsage{{UsageIdentity: UsageIdentity{ID: "one", Finished: true}, Observations: map[string]UsageObservation{"final": {Final: true, Complete: true, InputTokens: &maximum, OutputTokens: &one, CostUSD: &cost}}}, {UsageIdentity: UsageIdentity{ID: "two", Finished: true}, Observations: map[string]UsageObservation{"final": {Final: true, Complete: true, InputTokens: &one, OutputTokens: &one, CostUSD: &cost}}}}
	summary := SummarizeUsage(items)
	if !summary.Overflow || summary.Complete || summary.InputTokens != nil || summary.CostUSD != nil || summary.KnownCostUSD != nil {
		t.Fatalf("overflow became a clamped or zero billing value: %+v", summary)
	}
}
