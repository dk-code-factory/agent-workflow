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

func TestSkillChildOriginalCompletionReconcilesBeforeUncertainToolHold(t *testing.T) {
	for _, variant := range []string{"lost-completion", "uncertain-attempt", "missing-stop", "accounting-model", "failed-turn", "later-turn"} {
		t.Run(variant, func(t *testing.T) {
			store, run := executionStore(t)
			owned, err := store.QueueResume(t.Context(), run.ID, "original", true, 128)
			if err != nil {
				t.Fatal(err)
			}
			owned.Status = RunStatusRunning
			if err = store.Claim(t.Context(), owned, RunStatusQueued); err != nil {
				t.Fatal(err)
			}
			plan, ledger, fixture := childLedgerFixture(t, "selected-child")
			w := Worker{ID: "worker", RunID: run.ID, Skill: plan, SkillEvidence: ledger, SessionID: fixture.Session, Status: RunStatusRunning, SkillTurnUncertain: true, SkillActiveMessageID: "message", Remaining: 31, TaskTurns: 32}
			raw, _ := json.Marshal(w)
			if _, err = store.db.Exec("INSERT INTO workers VALUES(?,?,?,?,?,?)", w.ID, run.ID, "", "native", w.Status, raw); err != nil {
				t.Fatal(err)
			}
			var permit SkillChildObservation
			for _, o := range fixture.Observations {
				if o.Kind == "permit" {
					permit = o
				}
			}
			if err = store.authorizeSkillChild(fenceContext(t.Context(), owned), w.ID, "message", permit); err != nil {
				t.Fatal(err)
			}
			w, err = (&WorkerManager{Store: store}).Get(t.Context(), run.ID, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range fixture.Observations {
				if o.Kind == "permit" || variant == "missing-stop" && o.Kind == "stop" {
					continue
				}
				if variant == "accounting-model" && o.Kind == "parent_terminal" {
					r := childRaw(o)
					models := childObject(r["modelUsage"])
					models["unexpected-model"] = models["claude-sonnet-4-6"]
					o.Raw, _ = json.Marshal(r)
				}
				if err = w.SkillEvidence.ObserveChild(plan, o); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "uncertain-attempt" {
				body, _ := json.Marshal(map[string]any{"session_id": w.SessionID, "type": "system", "subtype": "api_retry", "attempt": 1})
				if err = w.SkillEvidence.ObserveUncertainAttempt(plan, SkillAttemptUncertainty{Key: "native-attempt", Provider: "claude-sdk", SessionID: w.SessionID, Raw: body}); err != nil {
					t.Fatal(err)
				}
			}
			message := WorkerMessage{ID: "message", WorkerID: w.ID, RequestID: "original-request", Status: RunStatusRunning, Generation: owned.Generation}
			if variant == "failed-turn" {
				message.Status = RunStatusFailed
				message.Error = "original validation failure"
			}
			if variant == "later-turn" {
				w.SkillActiveMessageID = "later-message"
			}
			w.Status = "closed"
			raw, _ = json.Marshal(w)
			if _, err = store.db.Exec("UPDATE workers SET status=?,record=? WHERE id=?", w.Status, raw, w.ID); err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(message)
			if _, err = store.db.Exec("INSERT INTO messages VALUES(?,?,?,?,?,?)", message.ID, w.ID, message.RequestID, 1, message.Status, raw); err != nil {
				t.Fatal(err)
			}
			if err = store.BeginUsage(t.Context(), UsageIdentity{ID: message.ID, RunID: run.ID, WorkerID: w.ID, Kind: "worker", Provider: "claude-sdk", Model: plan.Model, SessionID: w.SessionID}); err != nil {
				t.Fatal(err)
			}
			owned.Status = RunStatusInterrupted
			if err = store.Save(t.Context(), owned); err != nil {
				t.Fatal(err)
			}
			if err = store.RecoverToolCalls(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLite(store.Dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			err = reopened.verifySkillContinuations(t.Context(), run.ID)
			if variant != "lost-completion" {
				if err == nil {
					t.Fatal("incomplete/foreign original turn was reconciled", variant)
				}
				rows, readErr := reopened.ToolCalls(t.Context(), run.ID, true)
				if readErr != nil || len(rows) != 1 || rows[0].Status != "uncertain" {
					t.Fatal(rows, readErr)
				}
				if variant == "missing-stop" || variant == "accounting-model" || variant == "uncertain-attempt" {
					report, e := reopened.Usage(t.Context(), run.ID)
					if e != nil || variant == "uncertain-attempt" && report.Summary.Complete || report.Summary.KnownCostUSD == nil || *report.Summary.KnownCostUSD < 0.000465 {
						t.Fatal("incomplete original completion discarded known charges", report, e)
					}
				}
				return
			}
			if err != nil {
				t.Fatal("original full completion did not reconcile", err)
			}
			w, err = (&WorkerManager{Store: reopened}).Get(t.Context(), run.ID, w.ID)
			if err != nil || w.SkillTurnUncertain || w.SkillActiveMessageID != "" || w.Remaining != 31 || len(w.SkillChildPermits) != 1 {
				t.Fatal("reconciliation reset allowance or original uncertainty", w, err)
			}
			rows, err := reopened.ToolCalls(t.Context(), run.ID, false)
			if err != nil || len(rows) != 1 || rows[0].Status != "completed" || !strings.Contains(string(rows[0].Result), "FULL_ORIGINAL_CHILD_OUTPUT_139") {
				t.Fatal("child completion lacks original full output", rows, err)
			}
			messages, err := (&WorkerManager{Store: reopened}).Messages(t.Context(), run.ID, w.ID)
			if err != nil || len(messages) != 1 || messages[0].Status != RunStatusSucceeded || messages[0].Result.Outputs["text"] != "SYNTHETIC_PARENT_TERMINAL_139" {
				t.Fatal(messages, err)
			}
			if err = reopened.verifySkillContinuations(t.Context(), run.ID); err != nil {
				t.Fatal("repeat reconciliation was not idempotent", err)
			}
			report, err := reopened.Usage(t.Context(), run.ID)
			if err != nil || *report.Summary.InputTokens != 80 || *report.Summary.OutputTokens != 15 || *report.Summary.CostUSD != 0.000465 {
				t.Fatal("original usage replay double charged", report, err)
			}
		})
	}
}
