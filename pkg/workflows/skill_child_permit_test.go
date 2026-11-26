package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillChildPermitCommitSurvivesLostAckAndCannotDispatchAgain(t *testing.T) {
	store, run := executionStore(t)
	queued, err := store.QueueResume(t.Context(), run.ID, "original", true, 128)
	if err != nil {
		t.Fatal(err)
	}
	queued.Status = RunStatusRunning
	if err = store.Claim(t.Context(), queued, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	plan, ledger, fixture := childLedgerFixture(t, "selected-child")
	worker := Worker{ID: "original-worker", RunID: run.ID, Skill: plan, SkillEvidence: ledger, SessionID: fixture.Session, Status: RunStatusRunning, SkillTurnUncertain: true, SkillActiveMessageID: "original-message", Remaining: 31, TaskTurns: 32}
	raw, _ := json.Marshal(worker)
	if _, err = store.db.Exec("INSERT INTO workers VALUES(?,?,?,?,?,?)", worker.ID, run.ID, "", "native", worker.Status, raw); err != nil {
		t.Fatal(err)
	}
	var permit SkillChildObservation
	for _, observation := range fixture.Observations {
		if observation.Kind == "permit" {
			permit = observation
		}
	}
	ctx := fenceContext(t.Context(), queued)
	// Simulate losing the reply immediately after the transaction commits.
	if err = store.authorizeSkillChild(ctx, worker.ID, "original-message", permit); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, variant := range []string{"identical", "new-call", "changed-options"} {
		changed := permit
		if variant == "new-call" {
			changed.ParentCallID = "replacement-call"
		}
		if variant == "changed-options" {
			changed.Raw = json.RawMessage(strings.Replace(string(changed.Raw), "Admitted deterministic child", "changed", 1))
		}
		if err = reopened.authorizeSkillChild(ctx, worker.ID, "original-message", changed); err == nil {
			t.Fatal("lost acknowledgement dispatched another child", variant)
		}
	}
	w, err := (&WorkerManager{Store: reopened}).Get(t.Context(), run.ID, worker.ID)
	if err != nil || len(w.SkillChildPermits) != 1 || w.Remaining != 31 || len(w.SkillEvidence.Children) != 1 {
		t.Fatal("restart lost allowance/original permit", w, err)
	}
	if err = reopened.RecoverToolCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := reopened.ToolCalls(t.Context(), run.ID, true)
	if err != nil || len(rows) != 1 || rows[0].Status != "uncertain" || rows[0].CallID != permit.ParentCallID {
		t.Fatal("lost child dispatch not held uncertain", rows, err)
	}
}

func TestSkillChildUsageUsesOriginalAllModelTotalsAndPreservesKnownChargesOnError(t *testing.T) {
	_, _, fixture := childLedgerFixture(t, "selected-child")
	var original map[string]any
	for _, observation := range fixture.Observations {
		if observation.Kind == "parent_terminal" {
			original = childRaw(observation)
		}
	}
	update, err := SkillChildUsage(original)
	total := childObject(update.Usage["total"])
	if err != nil || *tokenValue(total["input_tokens"]) != 80 || *tokenValue(total["output_tokens"]) != 15 || *costValue(update.CostUSD) != 0.000465 || update.Incomplete {
		t.Fatal("parent-only 48/9 replaced complete child accounting", update, err)
	}
	for _, variant := range []string{"missing-cost", "unknown-model", "missing-count", "parent-floor", "cost-conflict"} {
		data, _ := json.Marshal(original)
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		model := childObject(childObject(raw["modelUsage"])["claude-sonnet-4-6"])
		switch variant {
		case "missing-cost":
			delete(raw, "total_cost_usd")
		case "unknown-model":
			childObject(raw["modelUsage"])["unexpected-model"] = model
		case "missing-count":
			delete(model, "outputTokens")
		case "parent-floor":
			childObject(raw["usage"])["input_tokens"] = float64(100)
		case "cost-conflict":
			raw["total_cost_usd"] = 0.0001
		}
		known, err := SkillChildUsage(raw)
		if err == nil || !known.Incomplete || costValue(known.CostUSD) == nil || *costValue(known.CostUSD) < 0.000465 || tokenValue(childObject(known.Usage["total"])["input_tokens"]) == nil {
			t.Fatal("invalid accounting discarded known charges or completed", variant, known, err)
		}
	}
}
