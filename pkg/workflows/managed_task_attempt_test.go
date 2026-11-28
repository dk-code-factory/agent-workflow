package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func managedTaskFixture(t *testing.T, provider string) (*SQLiteStore, context.Context, *Run, Attempt, Attempt) {
	t.Helper()
	s, run := executionStore(t)
	run.Status = RunStatusRunning
	if err := s.Claim(t.Context(), run, RunStatusFailed); err != nil {
		t.Fatal(err)
	}
	ctx := fenceContext(t.Context(), run)
	parent, _, err := s.BeginConversation(ctx, run.ID, "job", "conversation", 1, "original", "")
	if err != nil {
		t.Fatal(err)
	}
	parent.Conversation = &ConversationState{Roles: []string{"a", "b"}, Active: &ConversationDelivery{ID: "delivery", From: "engine", To: "a", Message: "original"}, Calls: 1, MaxCalls: 2, MaxMessages: 4, Deadline: time.Now().UTC().Add(time.Minute)}
	if err = s.RecordAttempt(ctx, parent); err != nil {
		t.Fatal(err)
	}
	turn, _, err := s.Begin(ctx, run.ID, "job", "conversation/conversation/0", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx = invocationContext(ctx, s, &turn)
	ctx = withManagedConversationAttempt(ctx, &conversationExecution{e: &Executor{Store: s}, attempt: &parent, state: parent.Conversation}, turn, nil)
	identity := UsageIdentity{ID: turn.ID, RunID: run.ID, TaskAttemptID: turn.ID, Kind: "task", Provider: provider, Model: "fixture"}
	if err = s.BeginUsage(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if provider == "claude-sdk" {
		err = BeginManagedSDKInvocation(ctx, run.ID, turn.ID, "fixture")
	} else {
		err = BeginManagedCodexInvocation(ctx, run.ID, turn.ID, "build", "policy", "fixture")
	}
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, run, parent, turn
}
func TestManagedConversationOriginalCoordinateHold(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) {
			s, ctx, run, parent, turn := managedTaskFixture(t, provider)
			before, _ := s.Attempts(ctx, run.ID)
			// Retry, replay-safe, changed input and parent replay cannot erase an older
			// unresolved native attempt, even if a newer row claims success.
			later := turn
			later.ID = "later-success"
			later.Number++
			later.Status = RunStatusSucceeded
			later.FinishedAt = time.Now().UTC()
			raw, _ := json.Marshal(later)
			if _, err := s.db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)`, later.ID, later.RunID, later.JobID, later.StepID, later.Iteration, later.Number, later.Status, raw); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.BeginInput(ctx, run.ID, turn.JobID, turn.StepID, turn.Iteration, true, "changed", true, ""); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("older unresolved marker bypassed", err)
			}
			if _, _, err := s.BeginConversation(ctx, run.ID, parent.JobID, parent.StepID, parent.Iteration, "original", ""); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("parent replay reset active turn", err)
			}
			after, _ := s.Attempts(ctx, run.ID)
			if len(after) != len(before)+1 {
				t.Fatal("hold mutated attempt history", len(before), len(after))
			}
			// Bind a distinct physical sibling to the same actual logical execution.
			snapRaw, _ := json.Marshal(Snapshot{Version: 1, Metadata: map[string]any{"iteration_logical_run_id": "logical-original"}})
			if _, err := s.db.Exec(`UPDATE runs SET snapshot=? WHERE id=?`, snapRaw, run.ID); err != nil {
				t.Fatal(err)
			}
			sibling := &Run{ID: "sibling", Status: RunStatusRunning, Jobs: map[string]JobExecution{}}
			if err := s.Admit(t.Context(), sibling, Snapshot{Version: 1, Metadata: map[string]any{"iteration_logical_run_id": "logical-original"}}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.BeginInput(t.Context(), sibling.ID, turn.JobID, turn.StepID, 1, true, "changed", true, "logical-original"); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("logical sibling granted new attempt", err)
			}
			// Existing settled journals remain reusable; merely flipping marker state
			// without atomic final accounting must still be rejected.
			path := "$.managed_sdk_attempt.state"
			state := "settled"
			key := managedSDKAttemptKey
			if provider == "codex-app-server" {
				path = "$.managed_codex_admission.state"
				state = "completed"
				key = managedCodexAdmissionKey
			}
			if _, err := s.db.Exec(`UPDATE usage_observations SET record=json_set(record,?,?) WHERE invocation_id=? AND observation_key=?`, path, state, turn.ID, key); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Begin(ctx, run.ID, turn.JobID, turn.StepID, 1, true); !errors.Is(err, ErrActionIntegrity) {
				t.Fatal("uncommitted final accounting bypassed", err)
			}
			if _, err := s.db.Exec(`UPDATE usage_invocations SET record=json_set(record,'$.finished',json('true')) WHERE id=?`, turn.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE attempts SET status='succeeded' WHERE id=?`, turn.ID); err != nil {
				t.Fatal(err)
			}
			got, reused, err := s.Begin(ctx, run.ID, turn.JobID, turn.StepID, 1, true)
			if err != nil || !reused || got.ID != later.ID {
				t.Fatal("settled committed journal not reused", got, reused, err)
			}
		})
	}
}
func TestManagedConversationTaskOriginAndAuthority(t *testing.T) {
	s, ctx, run, parent, turn := managedTaskFixture(t, "claude-sdk")
	binding, _ := runtimeexec.BindingFrom(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	tag := ctx.Value(managedTaskKey{}).(managedTaskAdmission)
	if _, err = managedTaskSnapshot(ctx, tx, binding, turn.ID, tag); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	for _, change := range []string{"deadline", "active", "calls"} {
		t.Run(change, func(t *testing.T) {
			altered := parent
			copy := *parent.Conversation
			altered.Conversation = &copy
			switch change {
			case "deadline":
				copy.Deadline = copy.Deadline.Add(time.Minute)
			case "active":
				copy.Active = &ConversationDelivery{ID: "foreign"}
			case "calls":
				copy.Calls++
			}
			if err := s.RecordAttempt(ctx, altered); err != nil {
				t.Fatal(err)
			}
			if err := CheckManagedSDKAuthority(ctx, run.ID, turn.ID); err == nil {
				t.Fatal("changed persisted allowance admitted")
			}
			if err := s.RecordAttempt(ctx, parent); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A nested physical attempt can use only its actual persisted origin owner.
	physical := &Run{ID: "physical-child", OriginID: run.ID, Status: RunStatusRunning, Jobs: map[string]JobExecution{}}
	if err := s.Admit(ctx, physical, Snapshot{Version: 1, Metadata: map[string]any{"origin_run_id": run.ID}}); err != nil {
		t.Fatal(err)
	}
	nestedParent := parent
	nestedParent.ID = "nested-parent"
	nestedParent.RunID = physical.ID
	nestedTurn := turn
	nestedTurn.ID = "nested-turn"
	nestedTurn.RunID = physical.ID
	for _, a := range []Attempt{nestedParent, nestedTurn} {
		raw, _ := json.Marshal(a)
		if _, err := s.db.Exec(`INSERT INTO attempts VALUES(?,?,?,?,?,?,?,?)`, a.ID, a.RunID, a.JobID, a.StepID, a.Iteration, a.Number, a.Status, raw); err != nil {
			t.Fatal(err)
		}
	}
	nestedTag := managedTaskAdmission{Attempt: nestedTurn, ParentID: nestedParent.ID, Deadline: nestedParent.Conversation.Deadline, ConversationSHA256: managedConversationDigest(nestedParent)}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = managedTaskSnapshot(ctx, tx, binding, nestedTurn.ID, nestedTag); err != nil {
		t.Fatal("real nested origin rejected", err)
	}
	foreign := binding
	foreign.RunID = "foreign"
	if _, err = managedTaskSnapshot(ctx, tx, foreign, nestedTurn.ID, nestedTag); err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatal("foreign origin admitted", err)
	}
	tx.Rollback()
}

func TestManagedConversationReplayQueryUsesOriginIndex(t *testing.T) {
	s, _, run, _, _ := managedTaskFixture(t, "claude-sdk")
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+managedTaskCoordinateQuery, run.ID, run.ID, "", "", managedSDKAttemptKey, managedCodexAdmissionKey, "job", "conversation", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		t.Log(detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "usage_origin") || strings.Contains(all, "SCAN u") || !strings.Contains(all, "SEARCH u USING INDEX") {
		t.Fatal("global historical observation scan", all)
	}
}
