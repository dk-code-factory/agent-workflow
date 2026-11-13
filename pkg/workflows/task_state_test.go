package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

func taskStateRun(t *testing.T, s *SQLiteStore, group string) (*Run, context.Context) {
	t.Helper()
	r := &Run{ID: randomID(), Version: 1, Status: RunStatusQueued, Jobs: map[string]JobExecution{}}
	meta := map[string]any{"origin_run_id": r.ID}
	if group != "" {
		meta["task_group"] = group
	}
	if err := s.Admit(t.Context(), r, Snapshot{Version: 1, Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	r.Status = RunStatusRunning
	if err := s.Claim(t.Context(), r, RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	return r, fenceContext(t.Context(), r)
}

func taskStateStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func taskStateCall(t *testing.T, s *SQLiteStore, ctx context.Context, req TaskStateRequest) TaskStateResponse {
	t.Helper()
	out, err := s.TaskState(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Operation, err)
	}
	return out
}

func TestTaskStateClaimsAreAtomicAndReplayDoesNotDispatch(t *testing.T) {
	s := taskStateStore(t)
	_, ctx := taskStateRun(t, s, "atomic")
	const n = 16
	results := make([]TaskStateResponse, n)
	failures := make([]error, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], failures[i] = s.TaskState(ctx, TaskStateRequest{Operation: "claim", Task: "review", Fingerprint: "input1", OperationID: fmt.Sprintf("op-%d", i)})
		}(i)
	}
	wg.Wait()
	count := 0
	winner := ""
	for i, out := range results {
		if failures[i] != nil {
			t.Fatal(failures[i])
		}
		if out.Dispatch {
			count++
			winner = out.Claim.OperationID
		}
	}
	if count != 1 {
		t.Fatalf("dispatch grants %d", count)
	}
	for i, out := range results {
		if out.Claim.OperationID != winner {
			t.Fatalf("contender %d did not retain original claim: %+v", i, out)
		}
	}
	replay := taskStateCall(t, s, ctx, TaskStateRequest{Operation: "claim", Task: "review", Fingerprint: "input1", OperationID: winner})
	if replay.Dispatch || replay.Claim.State != "claimed" {
		t.Fatalf("replayed lost acknowledgement grants dispatch: %+v", replay)
	}
	complete := TaskStateRequest{Operation: "complete", Task: "review", Fingerprint: "input1", OperationID: winner, ResultReference: "artifact://review1", EvidenceFingerprint: "current-evidence"}
	taskStateCall(t, s, ctx, complete)
	taskStateCall(t, s, ctx, complete)
	complete.ResultReference = "artifact://replacement"
	if _, err := s.TaskState(ctx, complete); err == nil {
		t.Fatal("changed completed result accepted")
	}
	for _, evidence := range []string{"", "old-evidence", "current-evidence"} {
		out := taskStateCall(t, s, ctx, TaskStateRequest{Operation: "get_claim", Task: "review", Fingerprint: "input1", EvidenceFingerprint: evidence})
		if out.Reusable != (evidence == "current-evidence") {
			t.Fatalf("reuse evidence %q: %+v", evidence, out)
		}
	}
	changed := taskStateCall(t, s, ctx, TaskStateRequest{Operation: "claim", Task: "review", Fingerprint: "input2", OperationID: "new-input"})
	if !changed.Dispatch {
		t.Fatal("changed input reused old claim")
	}
	if _, err := s.TaskState(ctx, TaskStateRequest{Operation: "claim", Task: "another", Fingerprint: "input1", OperationID: winner}); err == nil {
		t.Fatal("operation identity rebound to another task")
	}
}

func TestTaskStateRestartAndUncertainClaimRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	original, ctx := taskStateRun(t, s, "recover")
	claim := TaskStateRequest{Operation: "claim", Task: "mutate", Fingerprint: "input", OperationID: "original-effect"}
	if !taskStateCall(t, s, ctx, claim).Dispatch {
		t.Fatal("first claim denied")
	}
	original.Status = RunStatusFailed
	if err = s.Save(t.Context(), original); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, recovery := taskStateRun(t, s, "recover")
	resumed := taskStateCall(t, s, recovery, TaskStateRequest{Operation: "claim", Task: "mutate", Fingerprint: "input", OperationID: "replacement-effect"})
	if resumed.Dispatch || resumed.Claim.OperationID != "original-effect" {
		t.Fatalf("owner loss granted effect replay: %+v", resumed)
	}
	if _, err = s.TaskState(recovery, TaskStateRequest{Operation: "complete", Task: "mutate", Fingerprint: "input", OperationID: "original-effect", ResultReference: "result", EvidenceFingerprint: "evidence"}); err == nil {
		t.Fatal("replacement execution directly completed old claim")
	}
	unknown := TaskStateRequest{Operation: "reconcile", Task: "mutate", Fingerprint: "input", OperationID: "original-effect", RequestID: "lookup1", Outcome: "unknown", Proof: json.RawMessage(`{"observed":"provider unavailable"}`)}
	taskStateCall(t, s, recovery, unknown)
	taskStateCall(t, s, recovery, unknown)
	unknown.RequestID = "lookup2"
	taskStateCall(t, s, recovery, unknown)
	if taskStateCall(t, s, recovery, claim).Dispatch {
		t.Fatal("unknown evidence granted replay")
	}
	noEffect := unknown
	noEffect.RequestID = "authoritative-empty"
	noEffect.Outcome = "no_effect"
	noEffect.Proof = json.RawMessage(`{"receipt":"authoritative no-effect proof"}`)
	taskStateCall(t, s, recovery, noEffect)
	taskStateCall(t, s, recovery, noEffect)
	replacement := TaskStateRequest{Operation: "claim", Task: "mutate", Fingerprint: "input", OperationID: "replacement-effect"}
	if !taskStateCall(t, s, recovery, replacement).Dispatch {
		t.Fatal("authoritative no-effect did not allow new physical claim")
	}
	if taskStateCall(t, s, recovery, claim).Dispatch {
		t.Fatal("original operation dispatched twice")
	}
	inspect, err := s.InspectTaskState(t.Context(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspect.Claims) != 2 || len(inspect.Requests) != 3 || inspect.Claims[0].State != "no_effect" {
		t.Fatalf("recovery history erased: %+v", inspect)
	}
}

func TestTaskStateRecoverCompletedProofWithoutDispatch(t *testing.T) {
	s := taskStateStore(t)
	r, ctx := taskStateRun(t, s, "completed-recovery")
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "claim", Task: "capture", Fingerprint: "input", OperationID: "effect"})
	r.Status = RunStatusFailed
	if err := s.Save(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	_, next := taskStateRun(t, s, "completed-recovery")
	req := TaskStateRequest{Operation: "reconcile", Task: "capture", Fingerprint: "input", OperationID: "effect", RequestID: "lookup", Outcome: "completed", Proof: json.RawMessage(`{"receipt":"recorded"}`), ResultReference: "artifact", EvidenceFingerprint: "evidence"}
	out := taskStateCall(t, s, next, req)
	if out.Dispatch || !out.Reusable || out.Claim.State != "completed" {
		t.Fatalf("receipt recovery: %+v", out)
	}
	req.Outcome = "no_effect"
	if _, err := s.TaskState(next, req); err == nil {
		t.Fatal("conflicting reconciliation delivery accepted")
	}
}

func TestTaskStateFencesOwnersAndAdmittedGroups(t *testing.T) {
	s := taskStateStore(t)
	r, ctx := taskStateRun(t, s, "private")
	req := TaskStateRequest{Operation: "reserve", Name: "reviews", OperationID: "first", Limit: 1, Amount: 1}
	if _, err := s.TaskState(t.Context(), req); err == nil {
		t.Fatal("unowned call accepted")
	}
	stale := *r
	stale.Owner = "superseded"
	if _, err := s.TaskState(fenceContext(t.Context(), &stale), req); err == nil {
		t.Fatal("stale owner accepted")
	}
	_, unscoped := taskStateRun(t, s, "")
	if _, err := s.TaskState(unscoped, req); err == nil {
		t.Fatal("unadmitted group accepted")
	}
	taskStateCall(t, s, ctx, req)
	_, other := taskStateRun(t, s, "other")
	req.Operation = "get_budget"
	if _, err := s.TaskState(other, req); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another group read private budget: %v", err)
	}
	if err := s.BeginFinalization(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TaskState(ctx, req); err == nil {
		t.Fatal("finalizing owner retained task service")
	}
}

func TestTaskStateBudgetConcurrencyContinuationAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ctx := taskStateRun(t, s, "budgets")
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.TaskState(ctx, TaskStateRequest{Operation: "reserve", Name: "reviews", OperationID: fmt.Sprintf("reserve-%d", i), Limit: 5, Amount: 1})
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("accepted %d reservations", accepted)
	}
	before := taskStateCall(t, s, ctx, TaskStateRequest{Operation: "get_budget", Name: "reviews"})
	if before.Budget.Used != 5 || before.Budget.Limit != 5 {
		t.Fatal(before.Budget)
	}
	extension := TaskStateRequest{Operation: "extend_budget", Name: "reviews", RequestID: "user-followup", Amount: 2, Instruction: "perform two more reviews", Source: "user message 123"}
	taskStateCall(t, s, ctx, extension)
	taskStateCall(t, s, ctx, extension)
	extension.Amount = 3
	if _, err := s.TaskState(ctx, extension); err == nil {
		t.Fatal("continuation identity replaced")
	}
	extension.RequestID = "unproven"
	extension.Source = ""
	if _, err := s.TaskState(ctx, extension); err == nil {
		t.Fatal("continuation without provenance accepted")
	}
	r.Status = RunStatusFailed
	if err = s.Save(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, next := taskStateRun(t, s, "budgets")
	after := taskStateCall(t, s, next, TaskStateRequest{Operation: "get_budget", Name: "reviews"})
	if after.Budget.Used != 5 || after.Budget.Limit != 7 {
		t.Fatalf("replacement reset allowance: %+v", after)
	}
	reserve := TaskStateRequest{Operation: "reserve", Name: "reviews", OperationID: "post-restart", Amount: 2, Limit: 5}
	taskStateCall(t, s, next, reserve)
	taskStateCall(t, s, next, reserve)
	reserve.OperationID = "exhausted"
	reserve.Amount = 1
	if _, err := s.TaskState(next, reserve); err == nil {
		t.Fatal("exhausted budget accepted")
	}
	reserve.Limit = 100
	if _, err := s.TaskState(next, reserve); err == nil {
		t.Fatal("initial allowance reset by new caller")
	}
}

func TestTaskStateDeadlineRetainsAbsoluteTimeAndExtensions(t *testing.T) {
	s := taskStateStore(t)
	r, ctx := taskStateRun(t, s, "deadlines")
	original := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	create := TaskStateRequest{Operation: "deadline", Name: "ci", OperationID: "first-window", Deadline: original}
	taskStateCall(t, s, ctx, create)
	taskStateCall(t, s, ctx, create)
	create.OperationID = "restart-window"
	create.Deadline = original.Add(time.Hour)
	if _, err := s.TaskState(ctx, create); err == nil {
		t.Fatal("new process reset existing deadline")
	}
	extension := TaskStateRequest{Operation: "extend_deadline", Name: "ci", RequestID: "pending-extension", Amount: 60, Instruction: "wait one more minute", Source: "user message 456"}
	taskStateCall(t, s, ctx, extension)
	taskStateCall(t, s, ctx, extension)
	r.Status = RunStatusFailed
	if err := s.Save(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	_, next := taskStateRun(t, s, "deadlines")
	out := taskStateCall(t, s, next, TaskStateRequest{Operation: "get_deadline", Name: "ci"})
	if out.Deadline.Extensions != 1 || !out.Deadline.OriginalDeadline.Equal(original) || !out.Deadline.Deadline.Equal(original.Add(time.Minute)) {
		t.Fatalf("deadline changed across run: %+v", out)
	}
	extension.RequestID = "overflow"
	extension.Amount = math.MaxInt64
	if _, err := s.TaskState(next, extension); err == nil {
		t.Fatal("overflowing deadline accepted")
	}
}

func TestTaskStateRetentionPreservesGroupAndClosedTombstone(t *testing.T) {
	s := taskStateStore(t)
	r, ctx := taskStateRun(t, s, "retention")
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "claim", Task: "mutate", Fingerprint: "input", OperationID: "uncertain"})
	r.Status = RunStatusFailed
	if err := s.Save(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrigin(t.Context(), r.ID); err == nil {
		t.Fatal("cleanup discarded unresolved owner evidence")
	}
	_, next := taskStateRun(t, s, "retention")
	taskStateCall(t, s, next, TaskStateRequest{Operation: "reconcile", Task: "mutate", Fingerprint: "input", OperationID: "uncertain", RequestID: "proof", Outcome: "no_effect", Proof: json.RawMessage(`{"effect":"none"}`)})
	taskStateCall(t, s, next, TaskStateRequest{Operation: "reserve", Name: "reviews", OperationID: "one", Amount: 1, Limit: 1})
	if err := s.DeleteOrigin(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	current, _, err := s.Load(t.Context(), next.Value(fenceKey{}).(executionFence).id)
	if err != nil {
		t.Fatal(err)
	}
	current.Status = RunStatusSucceeded
	if err = s.Save(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOrigin(t.Context(), current.ID); err != nil {
		t.Fatal(err)
	}
	last, lastCtx := taskStateRun(t, s, "retention")
	out := taskStateCall(t, s, lastCtx, TaskStateRequest{Operation: "get_budget", Name: "reviews"})
	if out.Budget.Used != 1 {
		t.Fatal("removing last run reset allowance")
	}
	close := TaskStateRequest{Operation: "close_group", RequestID: "finish", Instruction: "finish and retain tombstone", Source: "user request"}
	taskStateCall(t, s, lastCtx, close)
	taskStateCall(t, s, lastCtx, close)
	last.Status = RunStatusSucceeded
	if err = s.Save(t.Context(), last); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteOrigin(t.Context(), last.ID); err != nil {
		t.Fatal(err)
	}
	var closed bool
	var detail int
	if err = s.db.QueryRow("SELECT closed FROM task_groups WHERE id='retention'").Scan(&closed); err != nil || !closed {
		t.Fatalf("closed tombstone lost: %v %v", closed, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM task_budgets WHERE group_id='retention'").Scan(&detail); err != nil || detail != 0 {
		t.Fatalf("closed group not purged: %d %v", detail, err)
	}
	fresh := &Run{ID: randomID(), Status: RunStatusQueued, Version: 1}
	if err = s.Admit(t.Context(), fresh, Snapshot{Version: 1, Metadata: map[string]any{"task_group": "retention"}}); err == nil {
		t.Fatal("closed group admitted a reset")
	}
}

func TestTaskStateClosedGroupValidationAdmission(t *testing.T) {
	s := taskStateStore(t)
	original, ctx := taskStateRun(t, s, "closed-validation")
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "budget", Name: "reviews", Limit: 1})
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "close_group", RequestID: "finish", Instruction: "finish", Source: "test"})
	newValidation := func() *Run {
		return &Run{ID: randomID(), OriginID: original.ID, Owner: randomID(), Generation: 1, Status: RunStatusRunning, Version: 1}
	}
	snapshot := Snapshot{Version: 1, Metadata: map[string]any{"task_group": "closed-validation"}}
	validationCtx := context.WithValue(t.Context(), taskGroupValidationKey{}, original.ID)
	if err := s.Admit(validationCtx, newValidation(), snapshot); err == nil {
		t.Fatal("validation admitted before the original member succeeded")
	}
	original.Status = RunStatusSucceeded
	if err := s.Save(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := s.Admit(t.Context(), newValidation(), snapshot); err == nil {
		t.Fatal("ordinary admission bypassed closure by copying the original run identity")
	}
	validation := newValidation()
	if err := s.Admit(validationCtx, validation, snapshot); err != nil {
		t.Fatal(err)
	}
	owned := fenceContext(t.Context(), validation)
	observed := taskStateCall(t, s, owned, TaskStateRequest{Operation: "get_budget", Name: "reviews"})
	if observed.Budget.Limit != 1 || observed.Budget.Used != 0 {
		t.Fatalf("validation lost retained state: %+v", observed)
	}
	if _, err := s.TaskState(owned, TaskStateRequest{Operation: "reserve", Name: "reviews", OperationID: "new", Limit: 1, Amount: 1}); err == nil {
		t.Fatal("validation reopened closed task-state writes")
	}
}

func TestTaskStateBackupMigrationAndReadOnlyInspection(t *testing.T) {
	s := taskStateStore(t)
	r, ctx := taskStateRun(t, s, "portable")
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "reserve", Name: "attempts", OperationID: "first", Amount: 1, Limit: 3})
	lock, err := ownership.Own(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	backup := filepath.Join(t.TempDir(), "state.db")
	if err = s.Backup(t.Context(), backup, lock); err != nil {
		t.Fatal(err)
	}
	copied, err := openBackupSource(t.Context(), backup)
	if err != nil {
		t.Fatal(err)
	}
	copied.Close()
	inspect, err := OpenSQLiteInspection(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := inspect.InspectTaskState(t.Context(), r.ID)
	inspect.Close()
	if err != nil || len(out.Budgets) != 1 || out.Budgets[0].Used != 1 {
		t.Fatalf("read-only inspection: %+v %v", out, err)
	}
	// A genuine v11 fixture contains no task-state tables. Upgrade is owned and
	// leaves the complete pre-upgrade database available for rollback.
	if _, err = s.db.Exec(`DROP TRIGGER task_state_closed_cleanup; DROP TABLE task_state_requests; DROP TABLE task_deadlines; DROP TABLE task_budgets; DROP TABLE task_claims; DROP TABLE task_group_members; DROP TABLE task_groups; PRAGMA user_version=11;`); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(t.Context(), lock); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Dir, "state.pre-v12.db")); err != nil {
		t.Fatal(err)
	}
	before, err := openBackupSource(t.Context(), filepath.Join(s.Dir, "state.pre-v12.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	var version int
	if err = before.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 11 {
		t.Fatalf("pre-upgrade version %d %v", version, err)
	}
	// Corrupt/missing v12 tables cannot be accepted as a usable recovery source.
	writable, err := sql.Open("sqlite", backup)
	if err != nil {
		t.Fatal(err)
	}
	_, err = writable.Exec("DROP TABLE task_state_requests")
	writable.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bad, err := openBackupSource(t.Context(), backup); err == nil {
		bad.Close()
		t.Fatal("incomplete task-state recovery schema accepted")
	}
}

func TestTaskStateZeroBudgetNeedsRetainedContinuation(t *testing.T) {
	s := taskStateStore(t)
	_, ctx := taskStateRun(t, s, "zero-budget")
	initial := TaskStateRequest{Operation: "budget", Name: "optional", Limit: 0}
	taskStateCall(t, s, ctx, initial)
	reserve := TaskStateRequest{Operation: "reserve", Name: "optional", OperationID: "first", Limit: 0, Amount: 1}
	if _, err := s.TaskState(ctx, reserve); err == nil {
		t.Fatal("zero allowance dispatched")
	}
	taskStateCall(t, s, ctx, TaskStateRequest{Operation: "extend_budget", Name: "optional", RequestID: "explicit", Amount: 1, Instruction: "allow one attempt", Source: "user message"})
	if out := taskStateCall(t, s, ctx, initial); out.Budget.Limit != 1 || out.Budget.Used != 0 {
		t.Fatalf("repeated declaration reset budget: %+v", out)
	}
	taskStateCall(t, s, ctx, reserve)
	initial.Limit = 9
	if _, err := s.TaskState(ctx, initial); err == nil {
		t.Fatal("declaration reset initial allowance")
	}
}
