package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
)

type actionInput struct {
	Value string `json:"value"`
}
type actionOutput struct {
	Value string `json:"value"`
}

func readSpec(calls *atomic.Int32) ActionSpec[actionInput, actionOutput] {
	return ActionSpec[actionInput, actionOutput]{Implementation: "test-v1", Effect: ActionReadOnly,
		Bindings: func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
			return json.RawMessage(`{"repository":"fixture"}`), nil
		},
		Handler: func(_ context.Context, _ ActionContext, i actionInput) (ActionResult[actionOutput], error) {
			calls.Add(1)
			return ActionResult[actionOutput]{Output: actionOutput{i.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}, nil
		},
		ValidateCheckpoint: func(_ context.Context, _ ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) error {
			if cp.Input.Value != cp.Result.Output.Value || string(cp.Result.Evidence) != `{"digest":"fixture"}` {
				return errors.New("evidence changed")
			}
			return nil
		},
	}
}

func registerTestAction(t *testing.T, e *Executor, spec ActionSpec[actionInput, actionOutput]) {
	t.Helper()
	r := &ActionRegistry{}
	if err := RegisterAction(r, "capture", spec); err != nil {
		t.Fatal(err)
	}
	e.ApplicationActions = r
}

const actionSource = `jobs:
  capture:
    steps:
      - id: capture
        uses: action/capture
        with: {value: hello}
`

func TestRegisteredActionContracts(t *testing.T) {
	var calls atomic.Int32
	spec := readSpec(&calls)
	for _, name := range []string{"", "bad/name", "a.b", "1name"} {
		if err := RegisterAction(&ActionRegistry{}, name, spec); err == nil {
			t.Fatal("accepted name", name)
		}
	}
	for _, change := range []func(*ActionSpec[actionInput, actionOutput]){
		func(s *ActionSpec[actionInput, actionOutput]) { s.Implementation = "" }, func(s *ActionSpec[actionInput, actionOutput]) { s.Effect = "" },
		func(s *ActionSpec[actionInput, actionOutput]) { s.Handler = nil }, func(s *ActionSpec[actionInput, actionOutput]) { s.Bindings = nil }, func(s *ActionSpec[actionInput, actionOutput]) { s.ValidateCheckpoint = nil },
	} {
		copy := spec
		change(&copy)
		if err := RegisterAction(&ActionRegistry{}, "x", copy); err == nil {
			t.Fatal("accepted incomplete spec")
		}
	}
	r := &ActionRegistry{}
	if err := RegisterAction(r, "capture", spec); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAction(r, "capture", spec); err == nil {
		t.Fatal("accepted duplicate")
	}
	r.freeze()
	if err := RegisterAction(r, "other", spec); err == nil {
		t.Fatal("accepted frozen registration")
	}
	if _, _, err := actionSchema[string](); err == nil {
		t.Fatal("accepted scalar")
	}
	if _, _, err := actionSchema[struct{ Bad chan string }](); err == nil {
		t.Fatal("accepted invalid Go field")
	}
	if _, _, err := actionSchema[struct{ Self *struct{ Bad func() } }](); err == nil {
		t.Fatal("accepted invalid nested Go field")
	}
	e, s := durableExecutor(t, actionSource, nil)
	if _, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "missing registered Go action") {
		t.Fatal(err)
	}
	registerTestAction(t, e, spec)
	e.Store = FileStore{Dir: t.TempDir()}
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil {
		t.Fatal("accepted nondurable store")
	}
	runs, _ := s.List(context.Background())
	if len(runs) != 0 || calls.Load() != 0 {
		t.Fatal("invalid admission left work")
	}
}

func TestRegisteredActionSchemasAndOutputs(t *testing.T) {
	ctx := context.Background()
	for _, with := range []string{"{value: 3}", "{}", "{value: hello, extra: true}"} {
		t.Run(with, func(t *testing.T) {
			e, _ := durableExecutor(t, strings.Replace(actionSource, "{value: hello}", with, 1), nil)
			var calls atomic.Int32
			registerTestAction(t, e, readSpec(&calls))
			if _, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"}); err == nil || calls.Load() != 0 {
				t.Fatal("bad input invoked", err)
			}
		})
	}
	e, s := durableExecutor(t, actionSource+`      - id: consume
        uses: action/capture
        with: {value: '${{ steps.capture.outputs.value }}'}
`, nil)
	var calls atomic.Int32
	registerTestAction(t, e, readSpec(&calls))
	r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || r.Jobs["capture"].Steps["consume"].Outputs["value"] != "hello" {
		t.Fatal(r)
	}
	records, _ := s.ActionAttempts(ctx, r.ID)
	_, snap, _ := s.Load(ctx, r.ID)
	if len(records) != 2 || len(snap.ActionContracts) != 1 || snap.ActionContracts["capture"].Fingerprint == "" || records[0].State != "completed" {
		t.Fatal("missing provenance", records, snap)
	}
	if _, err = e.Resume(ctx, r.ID, nil, false); err != nil || calls.Load() != 2 {
		t.Fatal("replayed successful checkpoint", err)
	}
}

func TestRegisteredActionDriftBlocksAllSuccessfulShortcuts(t *testing.T) {
	ctx := context.Background()
	for _, drift := range []string{"implementation", "effect", "schema", "binding", "evidence"} {
		t.Run(drift, func(t *testing.T) {
			e, s := durableExecutor(t, actionSource+`  later:
    needs: capture
    steps: [{argv: [false]}]
`, nil)
			var calls atomic.Int32
			spec := readSpec(&calls)
			registerTestAction(t, e, spec)
			r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil {
				t.Fatal("expected later failure")
			}
			switch drift {
			case "implementation":
				spec.Implementation = "test-v2"
			case "effect":
				spec.Effect = ActionExternalMutation
			case "schema":
				var snap Snapshot
				var b []byte
				if err = s.db.QueryRow("SELECT snapshot FROM runs WHERE id=?", r.ID).Scan(&b); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(b, &snap); err != nil {
					t.Fatal(err)
				}
				c := snap.ActionContracts["capture"]
				c.InputSchema = json.RawMessage(`{"type":"object"}`)
				snap.ActionContracts["capture"] = c
				b, _ = json.Marshal(snap)
				if _, err = s.db.Exec("UPDATE runs SET snapshot=? WHERE id=?", b, r.ID); err != nil {
					t.Fatal(err)
				}
			case "binding":
				spec.Bindings = func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
					return json.RawMessage(`{"repository":"other"}`), nil
				}
			case "evidence":
				spec.ValidateCheckpoint = func(context.Context, ActionContext, ActionCheckpoint[actionInput, actionOutput]) error {
					return errors.New("evidence changed")
				}
			}
			registerTestAction(t, e, spec)
			_, err = e.Resume(ctx, r.ID, nil, true)
			if !errors.Is(err, ErrActionIntegrity) || calls.Load() != 1 {
				t.Fatal("reused drifted action", calls.Load(), err)
			}
		})
	}
}

type faultActionJournal struct {
	ActionJournal
	admission, completion bool
}

func (s faultActionJournal) AdmitAction(ctx context.Context, a ActionAttempt) error {
	if s.admission {
		return errors.New("admission storage failure")
	}
	return s.ActionJournal.AdmitAction(ctx, a)
}
func (s faultActionJournal) CheckpointAction(ctx context.Context, a ActionAttempt) error {
	if s.completion && a.State == "completed" {
		return errors.New("completion storage failure")
	}
	return s.ActionJournal.CheckpointAction(ctx, a)
}

func TestRegisteredActionDurabilityBarriers(t *testing.T) {
	for _, stage := range []string{"admission", "completion"} {
		t.Run(stage, func(t *testing.T) {
			e, s := durableExecutor(t, actionSource+`        continue-on-error: true
      - id: dependent
        uses: action/capture
        with: {value: dependent}
  dependency:
    needs: capture
    if: always()
    steps: [{uses: action/capture, with: {value: dependent}}]
`, nil)
			var calls atomic.Int32
			registerTestAction(t, e, readSpec(&calls))
			e.Store = faultActionJournal{ActionJournal: s, admission: stage == "admission", completion: stage == "completion"}
			r, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			want := int32(0)
			if stage == "completion" {
				want = 1
			}
			if err == nil || r.Status != RunStatusNeedsAttention || calls.Load() != want {
				t.Fatal("durability barrier bypassed", r, calls.Load(), err)
			}
			records, _ := s.ActionAttempts(context.Background(), r.ID)
			if stage == "completion" && (len(records) != 1 || records[0].State != "admitted") {
				t.Fatal("lost completion trusted", records)
			}
		})
	}
}

func TestRegisteredMutationReceiptReconciliation(t *testing.T) {
	for _, outcome := range []string{"completed", "no_effect", "unknown", "error", "missing", "no_proof"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			e, s := durableExecutor(t, actionSource, nil)
			var calls atomic.Int32
			spec := readSpec(&calls)
			spec.Effect = ActionExternalMutation
			var original ActionContext
			spec.Handler = func(_ context.Context, ac ActionContext, i actionInput) (ActionResult[actionOutput], error) {
				n := calls.Add(1)
				if n == 1 {
					original = ac
				} else if ac.OperationKey != original.OperationKey || ac.AttemptID == original.AttemptID || ac.Generation <= original.Generation {
					t.Fatal("wrong replay identity", ac, original)
				}
				r := ActionResult[actionOutput]{Output: actionOutput{i.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`), Receipt: json.RawMessage(`{"publication":"retained-receipt"}`)}
				if n == 1 {
					return r, errors.New("lost mutation response")
				}
				return r, nil
			}
			if outcome != "missing" {
				spec.Reconcile = func(_ context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) (ActionReconciliation[actionOutput], error) {
					if ac != original || cp.Input.Value != "hello" || len(cp.Result.Receipt) == 0 {
						t.Fatal("original operation/receipt lost", ac, cp)
					}
					if outcome == "error" {
						return ActionReconciliation[actionOutput]{}, errors.New("receipt store unavailable")
					}
					r := ActionReconciliation[actionOutput]{Outcome: outcome, Proof: json.RawMessage(`{"receipt":"verified"}`), Result: cp.Result}
					if outcome == "no_proof" {
						r.Outcome = "no_effect"
						r.Proof = nil
					}
					return r, nil
				}
			}
			registerTestAction(t, e, spec)
			r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil {
				t.Fatal("expected uncertainty")
			}
			// Explicit resume can reconcile without granting permission to replay.
			r, err = e.Resume(ctx, r.ID, nil, false)
			want := int32(1)
			if outcome == "no_effect" {
				want = 2
			}
			if outcome == "completed" || outcome == "no_effect" {
				if err != nil || r.Status != RunStatusSucceeded {
					t.Fatal(r, err)
				}
			} else {
				if !errors.Is(err, ErrActionIntegrity) || r.Status != RunStatusNeedsAttention {
					t.Fatal("unknown mutation admitted", r, err)
				}
				if _, err = e.Resume(ctx, r.ID, nil, true); !errors.Is(err, ErrActionIntegrity) {
					t.Fatal("retry flag overrode uncertainty", err)
				}
			}
			if calls.Load() != want {
				t.Fatal("duplicate mutation", calls.Load())
			}
			records, err := s.ActionAttempts(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(records[0].Reconciliations) == 0 || len(records[0].Result.Receipt) == 0 {
				t.Fatal("recovery history lost", records)
			}
			if outcome == "no_effect" && (len(records) != 2 || records[0].State != "no_effect") {
				t.Fatal("no-effect proof not retained", records)
			}
		})
	}
}

func TestRegisteredReadOnlyLostCompletionAndRestart(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, actionSource, nil)
	var calls atomic.Int32
	registerTestAction(t, e, readSpec(&calls))
	e.Store = faultActionJournal{ActionJournal: s, completion: true}
	r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("completion should fail")
	}
	dir := s.Dir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	if err = reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = e.Resume(ctx, r.ID, nil, false)
	if err != nil || calls.Load() != 2 || r.Status != RunStatusSucceeded {
		t.Fatal("read-only did not rerun", r, err)
	}
	records, _ := reopened.ActionAttempts(ctx, r.ID)
	if len(records) != 2 || records[0].State != "admitted" {
		t.Fatal("uncertainty history erased", records)
	}
}

func TestRegisteredActionsAcrossRepeatAndChildRecovery(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, `jobs:
  child:
    uses: workflows/child.yml
  later:
    needs: child
    steps: [{argv: [false]}]
`, nil)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs:
  repeat:
    repeat: {max-attempts: 2, until: '${{ iteration.number == 2 }}'}
    steps: [{uses: action/capture, with: {value: 'iteration-${{ iteration.number }}'}}]
`)
	var calls, validations atomic.Int32
	spec := readSpec(&calls)
	validator := spec.ValidateCheckpoint
	spec.ValidateCheckpoint = func(ctx context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) error {
		validations.Add(1)
		return validator(ctx, ac, cp)
	}
	registerTestAction(t, e, spec)
	r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || calls.Load() != 2 {
		t.Fatal(r, err)
	}
	// Replace the failing external step with a successful retained attempt; the
	// admitted workflow stays immutable and child execution must be reused.
	a, _, err := s.Begin(ctx, r.ID, "later", "step_1", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, a, StepExecution{ID: "step_1", Status: RunStatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	r, err = e.Resume(ctx, r.ID, nil, true)
	if err != nil || r.Status != RunStatusSucceeded || calls.Load() != 2 || validations.Load() < 2 {
		t.Fatal("child/repeat shortcut bypassed validation", r, err, calls.Load(), validations.Load())
	}
	// A completed originating run must validate descendants as well.
	spec.ValidateCheckpoint = func(context.Context, ActionContext, ActionCheckpoint[actionInput, actionOutput]) error {
		return errors.New("child evidence gone")
	}
	registerTestAction(t, e, spec)
	if _, err = e.Resume(ctx, r.ID, nil, false); !errors.Is(err, ErrActionIntegrity) {
		t.Fatal("successful origin skipped child evidence", err)
	}
}

func TestActionCredentialHandlingAndFencing(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, strings.Replace(actionSource, "value: hello", "value: '${{ secrets.token }}'", 1), nil)
	var calls atomic.Int32
	registerTestAction(t, e, readSpec(&calls))
	r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "secret-value"}})
	if err == nil || calls.Load() != 0 {
		t.Fatal("credential identity admitted", r, err)
	}
	records, _ := s.ActionAttempts(ctx, r.ID)
	if len(records) != 0 {
		t.Fatal("credential persisted", records)
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", actionSource)
	spec := readSpec(&calls)
	spec.Handler = func(context.Context, ActionContext, actionInput) (ActionResult[actionOutput], error) {
		return ActionResult[actionOutput]{Output: actionOutput{"secret-value"}, Receipt: json.RawMessage(`{"token":"secret-value"}`)}, nil
	}
	registerTestAction(t, e, spec)
	r, err = e.Run(ctx, RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "secret-value"}})
	if err != nil {
		t.Fatal(err)
	}
	records, _ = s.ActionAttempts(ctx, r.ID)
	b, _ := json.Marshal(records)
	if strings.Contains(string(b), "secret-value") {
		t.Fatal("credential leaked")
	}
	if r.Jobs["capture"].Steps["capture"].Outputs["value"] != "[REDACTED]" {
		t.Fatal(r)
	}
	// A completed run has invalidated its owner's fence.
	old := records[0]
	old.Revision++
	if err = s.CheckpointAction(fenceContext(ctx, &Run{ID: r.ID, Generation: r.Generation, Owner: "stale"}), old); err == nil {
		t.Fatal("stale owner completed action")
	}
}

func TestActionMigrationFromV7(t *testing.T) {
	ctx := context.Background()
	_, s := durableExecutor(t, actionSource, nil)
	if _, err := s.db.Exec("DROP TABLE iteration_attempts; DROP TABLE iteration_plans; DROP TABLE action_attempts; PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	lock, err := ownership.Own(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = s.Migrate(ctx, lock); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Dir, fmt.Sprintf("state.pre-v%d.db", schemaVersion))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActionAttempts(ctx, "unused"); err != nil {
		t.Fatal(err)
	}
}

type invalidActionOutput struct {
	Value string `json:"value"`
}

func (invalidActionOutput) MarshalJSON() ([]byte, error) { return []byte(`{"value":123}`), nil }

func TestRegisteredActionSerializedOutputValidation(t *testing.T) {
	e, s := durableExecutor(t, actionSource, nil)
	r := &ActionRegistry{}
	if err := RegisterAction(r, "capture", ActionSpec[actionInput, invalidActionOutput]{Implementation: "invalid-output-v1", Effect: ActionReadOnly,
		Bindings: func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		},
		Handler: func(context.Context, ActionContext, actionInput) (ActionResult[invalidActionOutput], error) {
			return ActionResult[invalidActionOutput]{Output: invalidActionOutput{}}, nil
		},
		ValidateCheckpoint: func(context.Context, ActionContext, ActionCheckpoint[actionInput, invalidActionOutput]) error {
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	e.ApplicationActions = r
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "value") {
		t.Fatal("serialized output accepted", err)
	}
	records, _ := s.ActionAttempts(context.Background(), run.ID)
	if len(records) != 1 || records[0].State == "completed" {
		t.Fatal("bad output completed", records)
	}
}

func TestRegisteredMutationLostCompletionCommit(t *testing.T) {
	ctx := context.Background()
	e, s := durableExecutor(t, actionSource, nil)
	var calls atomic.Int32
	spec := readSpec(&calls)
	spec.Effect = ActionExternalMutation
	var original ActionContext
	handler := spec.Handler
	spec.Handler = func(ctx context.Context, ac ActionContext, i actionInput) (ActionResult[actionOutput], error) {
		original = ac
		return handler(ctx, ac, i)
	}
	spec.Reconcile = func(_ context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) (ActionReconciliation[actionOutput], error) {
		if ac != original {
			t.Error("lost operation identity")
		}
		return ActionReconciliation[actionOutput]{Outcome: "completed", Proof: json.RawMessage(`{"receipt":"retained-by-application"}`), Result: ActionResult[actionOutput]{Output: actionOutput{cp.Input.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}}, nil
	}
	registerTestAction(t, e, spec)
	e.Store = faultActionJournal{ActionJournal: s, completion: true}
	r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("completion commit should fail")
	}
	e.Store = s
	r, err = e.Resume(ctx, r.ID, nil, true)
	if err != nil || calls.Load() != 1 || r.Status != RunStatusSucceeded {
		t.Fatal("lost completion repeated mutation", r, err, calls.Load())
	}
}
