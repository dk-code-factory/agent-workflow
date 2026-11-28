package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func managedUsageFixture(t *testing.T) (*SQLiteStore, context.Context, UsageIdentity) {
	t.Helper()
	store, run := executionStore(t)
	run.Status = RunStatusRunning
	if err := store.Claim(context.Background(), run, RunStatusFailed); err != nil {
		t.Fatal(err)
	}
	identity := UsageIdentity{ID: "original-message", RunID: run.ID, Kind: "worker", WorkerID: "worker", Provider: "codex-app-server", Model: "fixture"}
	if err := store.BeginUsage(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	ctx := runtimeexec.WithBinding(context.Background(), runtimeexec.Binding{Journal: workerInvocationJournal{Journal: store}, RunID: run.ID, LogicalID: identity.ID, Owner: run.Owner, Generation: run.Generation, Kind: "worker"})
	worker := Worker{ID: identity.WorkerID, RunID: run.ID, Role: "parent", Status: RunStatusRunning, Remaining: 0, TurnDuration: time.Minute, MaxOutput: 1 << 20, SessionID: "session"}
	message := WorkerMessage{ID: identity.ID, WorkerID: worker.ID, Generation: run.Generation, RequestID: "original", Sequence: 1, Status: RunStatusRunning, StartedAt: time.Now().UTC()}
	workerRaw, _ := json.Marshal(worker)
	messageRaw, _ := json.Marshal(message)
	if _, err := store.db.Exec(`INSERT INTO workers VALUES(?,?,?,?,?,?)`, worker.ID, run.ID, "", worker.Role, worker.Status, workerRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO messages VALUES(?,?,?,?,?,?)`, message.ID, worker.ID, message.RequestID, message.Sequence, message.Status, messageRaw); err != nil {
		t.Fatal(err)
	}
	if err := BeginManagedCodexInvocation(ctx, run.ID, identity.ID, "build", "policy", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := BindManagedCodexInvocation(ctx, run.ID, identity.ID, "session", "original-turn"); err != nil {
		t.Fatal(err)
	}
	return store, ctx, identity
}
func managedUsageWitness(response string, input int64) ManagedUsageWitness {
	batch, _ := json.Marshal(managedCodexBatchIdentity{ThreadID: "session", TurnID: "original-turn", InvocationID: "original-message", PolicyID: "policy", BuildID: "build", BatchID: "batch-" + response, ResponseID: response, CallIDs: []string{}})
	return ManagedUsageWitness{Batch: batch, ResponseID: response, Digest: strings.Repeat("a", 64), SessionID: "session", TurnID: "original-turn", PolicyID: "policy", BuildID: "build", Model: "fixture", Usage: map[string]any{"input_tokens": input, "cached_input_tokens": int64(1), "output_tokens": int64(2), "total_tokens": input + 2}}
}
func TestManagedCodexUsageEmptyToolsAndOriginalResponseReplay(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	first, final := managedUsageWitness("original-broker", 10), managedUsageWitness("original-final", 20)
	// There is deliberately no public ToolBinding or InvocationTools: repairs
	// retain only the original engine context.
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, first); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := RecordManagedCodexUsage(cancelled, id.RunID, id.ID, first); err != nil {
		t.Fatal("known charge lost after cancellation", err)
	}
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, final); err != nil {
		t.Fatal(err)
	}
	totals, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, final.ResponseID, final.Digest)
	if err != nil || totals["input_tokens"] != int64(30) || totals["output_tokens"] != int64(4) {
		t.Fatal(totals, err)
	}
	report, err := store.Usage(context.Background(), id.RunID)
	if err != nil || *report.Summary.InputTokens != 30 || report.Summary.Complete {
		t.Fatal(report, err)
	}
	changed := first
	changed.Usage = managedUsageWitness("unused", 40).Usage
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, changed); err == nil {
		t.Fatal("conflicting original response accepted")
	}
	report, err = store.Usage(context.Background(), id.RunID)
	if err != nil || *report.Summary.InputTokens != 30 {
		t.Fatal("conflict erased known charge", report, err)
	}
	if _, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, "unobserved", final.Digest); err == nil {
		t.Fatal("unknown final response declared complete")
	}
	if err := store.ObserveUsage(ctx, id, UsageUpdate{Key: "unknown-attempt", Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, final.ResponseID, final.Digest); err == nil {
		t.Fatal("earlier unknown attempt hidden by completed response")
	}
}
func TestManagedCodexUsageRejectsInvalidCountersAndAuthority(t *testing.T) {
	for index, bad := range []any{nil, -1, int64(-1), math.Inf(1), 1.5, json.Number("9223372036854775808")} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			store, ctx, id := managedUsageFixture(t)
			w := managedUsageWitness("response", 10)
			w.Usage["input_tokens"] = bad
			if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err == nil {
				t.Fatalf("invalid counter %v admitted", bad)
			}
			var count int
			if err := store.db.QueryRow(`SELECT count(*) FROM usage_observations WHERE invocation_id=?`, id.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
	store, ctx, id := managedUsageFixture(t)
	w := managedUsageWitness("response", 10)
	for _, alter := range []func(*ManagedUsageWitness){func(w *ManagedUsageWitness) { w.Digest = strings.Repeat("z", 64) }, func(w *ManagedUsageWitness) { w.Usage["total_tokens"] = 999 }, func(w *ManagedUsageWitness) { w.Usage["cached_input_tokens"] = 20 }, func(w *ManagedUsageWitness) { w.Model = "foreign" }, func(w *ManagedUsageWitness) { w.Usage["total"] = map[string]any{"input_tokens": 999} }} {
		bad := managedUsageWitness("response", 10)
		alter(&bad)
		if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, bad); err == nil {
			t.Fatal("invalid witness admitted")
		}
	}
	if err := RecordManagedCodexUsage(context.Background(), id.RunID, id.ID, w); err == nil {
		t.Fatal("no engine context accepted")
	}
	if err := RecordManagedCodexUsage(ctx, id.RunID, "new-message", w); err == nil {
		t.Fatal("foreign invocation accepted")
	}
	if _, err := store.db.Exec(`UPDATE runs SET owner='replacement' WHERE id=?`, id.RunID); err != nil {
		t.Fatal(err)
	}
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err == nil {
		t.Fatal("stale owner accepted")
	}
}
func TestManagedCodexUsageTotalsRejectsForeignProofScope(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	first, final := managedUsageWitness("one", 10), managedUsageWitness("two", 20)
	for _, w := range []ManagedUsageWitness{first, final} {
		if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
			t.Fatal(err)
		}
	}
	var raw []byte
	if err := store.db.QueryRow(`SELECT record FROM usage_observations WHERE invocation_id=? AND observation_key='codex-response:one'`, id.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row managedCodexUsageRecord
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	row.Proof.PolicyID = "foreign"
	raw, _ = json.Marshal(row)
	if _, err := store.db.Exec(`UPDATE usage_observations SET record=? WHERE invocation_id=? AND observation_key='codex-response:one'`, raw, id.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, final.ResponseID, final.Digest); err == nil {
		t.Fatal("different retained policy accepted")
	}
}

func TestManagedCodexAuthorityUsesOriginalConsumedTurnAndDeadline(t *testing.T) {
	store, ctx, id := managedUsageFixture(t)
	binding, _ := runtimeexec.BindingFrom(ctx)
	binding.Kind = "worker"
	ctx = runtimeexec.WithBinding(ctx, binding)
	var worker Worker
	var message WorkerMessage
	var workerRaw, messageRaw []byte
	if err := store.db.QueryRow(`SELECT record FROM workers WHERE id=?`, id.WorkerID).Scan(&workerRaw); err != nil {
		t.Fatal(err)
	}
	if err := decodeJSONWithNumbers(workerRaw, &worker); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT record FROM messages WHERE id=?`, id.ID).Scan(&messageRaw); err != nil {
		t.Fatal(err)
	}
	if err := decodeJSONWithNumbers(messageRaw, &message); err != nil {
		t.Fatal(err)
	}
	if err := CheckManagedCodexAuthority(ctx, id.RunID, id.ID); err != nil {
		t.Fatal("last consumed turn lost existing authority", err)
	}
	// Charge first, then deny expired original admission. Neither its known
	// charge nor its consumed allowance is rolled back by the post-commit gate.
	message.StartedAt = time.Now().Add(-2 * time.Minute)
	messageRaw, _ = json.Marshal(message)
	if _, err := store.db.Exec(`UPDATE messages SET record=? WHERE id=?`, messageRaw, message.ID); err != nil {
		t.Fatal(err)
	}
	w := managedUsageWitness("known-completed", 10)
	if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
		t.Fatal(err)
	}
	if err := CheckManagedCodexAuthority(ctx, id.RunID, id.ID); err == nil {
		t.Fatal("expired admission permitted effect")
	}
	report, err := store.Usage(context.Background(), id.RunID)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 10 {
		t.Fatal("deadline erased known charge", report, err)
	}
	message.StartedAt = time.Now().UTC()
	messageRaw, _ = json.Marshal(message)
	if _, err := store.db.Exec(`UPDATE messages SET record=? WHERE id=?`, messageRaw, message.ID); err != nil {
		t.Fatal(err)
	}
	if err := CheckManagedCodexAuthority(ctx, id.RunID, id.ID); err != nil {
		t.Fatal("fresh original lost authority before SQL status change", err)
	}
	if _, err := store.db.Exec(`UPDATE messages SET status='queued' WHERE id=?`, message.ID); err != nil {
		t.Fatal(err)
	}
	if err := CheckManagedCodexAuthority(ctx, id.RunID, id.ID); err == nil {
		t.Fatal("new/unadmitted mailbox entry inherited authority")
	}
}

func TestManagedCodexUsageTotalsRejectsOverflow(t *testing.T) {
	_, ctx, id := managedUsageFixture(t)
	for _, w := range []ManagedUsageWitness{managedUsageWitness("first", math.MaxInt64-2), managedUsageWitness("last", 10)} {
		if err := RecordManagedCodexUsage(ctx, id.RunID, id.ID, w); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ManagedCodexUsageTotals(ctx, id.RunID, id.ID, "last", strings.Repeat("a", 64)); err == nil {
		t.Fatal("overflow became complete usage")
	}
}
