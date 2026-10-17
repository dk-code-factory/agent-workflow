package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type faultJournal struct {
	Journal
	failAdmission  bool
	failBegin      bool
	failCompletion bool
	failSaves      bool
}

func (s *faultJournal) Admit(ctx context.Context, r *Run, snap Snapshot) error {
	if s.failAdmission {
		return errors.New("disk full during admission")
	}
	return s.Journal.Admit(ctx, r, snap)
}
func (s *faultJournal) Begin(ctx context.Context, run, job, step string, iteration int, retry bool) (Attempt, bool, error) {
	if s.failBegin {
		return Attempt{}, false, errors.New("disk full before attempt")
	}
	return s.Journal.Begin(ctx, run, job, step, iteration, retry)
}
func (s *faultJournal) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	if s.failCompletion {
		return errors.New("disk full after effect")
	}
	return s.Journal.Complete(ctx, a, result)
}
func (s *faultJournal) Save(ctx context.Context, r *Run) error {
	if s.failSaves {
		return errors.New("disk full during checkpoint")
	}
	return s.Journal.Save(ctx, r)
}

const dependentFaultWorkflow = `jobs:
  edit:
    steps: [{id: edit, uses: agent/a, with: {prompt: edit}}]
  dependent:
    needs: edit
    steps: [{id: dependent, uses: agent/a, with: {prompt: dependent}}]
`

func TestStorageFailureBeforeAdmissionOrAttemptLaunchesNoEffect(t *testing.T) {
	for _, stage := range []string{"admission", "attempt"} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			e, s := durableExecutor(t, dependentFaultWorkflow, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls++
				return map[string]any{"text": "done"}, nil
			}))
			fault := &faultJournal{Journal: s, failAdmission: stage == "admission", failBegin: stage == "attempt"}
			e.Store = fault
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "disk full") {
				t.Fatal("fault ignored", err)
			}
			if calls != 0 {
				t.Fatal("effect launched without durable admission")
			}
		})
	}
}
func TestLostCompletionRequiresExplicitEffectRetry(t *testing.T) {
	ctx := context.Background()
	calls := 0
	e, s := durableExecutor(t, dependentFaultWorkflow, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		calls++
		return map[string]any{"text": "done"}, nil
	}))
	fault := &faultJournal{Journal: s, failCompletion: true}
	e.Store = fault
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || calls != 1 {
		t.Fatal("dependent admitted after lost completion", calls, err)
	}
	fault.failCompletion = false
	if _, err = e.Resume(ctx, run.ID, nil, false); err == nil || calls != 1 {
		t.Fatal("uncertain effect replayed silently", calls, err)
	}
	if _, err = e.Resume(ctx, run.ID, nil, true); err != nil || calls != 3 {
		t.Fatal("explicit retry did not complete", calls, err)
	}
}
func TestLostJobCheckpointRetainsCompletedStepOnRecovery(t *testing.T) {
	ctx := context.Background()
	calls := map[string]int{}
	e, s := durableExecutor(t, dependentFaultWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls[r.Prompt]++
		return map[string]any{"text": "done"}, nil
	}))
	fault := &faultJournal{Journal: s, failSaves: true}
	e.Store = fault
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || calls["edit"] != 1 || calls["dependent"] != 0 {
		t.Fatal("checkpoint failure did not stop dependents", calls, err)
	}
	fault.failSaves = false
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatal(result, err)
	}
	if calls["edit"] != 1 || calls["dependent"] != 1 {
		t.Fatal("completed work replayed after lost checkpoint", calls)
	}
}
func TestStateDurabilityPragmas(t *testing.T) {
	_, s := durableExecutor(t, "jobs: {x: {steps: [{argv: [true]}]}}", nil)
	var mode string
	var synchronous, foreignKeys int
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || synchronous != 2 || foreignKeys != 1 {
		t.Fatalf("durability disabled: %s synchronous=%d foreign_keys=%d", mode, synchronous, foreignKeys)
	}
}

const repairFaultWorkflow = `jobs:
  implement:
    steps:
      - id: implement
        uses: agent/a
        with:
          prompt: IMPLEMENT EXACTLY ONCE
          output:
            format: json
            repair-attempts: 1
            schema:
              type: object
              required: [ok]
              properties: {ok: {type: boolean}}
`

func TestResumeAfterSchemaRepairFailureNeverRepeatsImplementation(t *testing.T) {
	ctx := context.Background()
	tasks, repairs := 0, 0
	failRepair := true
	e, s := durableExecutor(t, repairFaultWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		if !r.RepairOnly {
			tasks++
			return map[string]any{"text": `{"ok":"invalid"}`}, nil
		}
		repairs++
		if strings.Contains(r.Prompt, "IMPLEMENT EXACTLY ONCE") {
			t.Error("repair repeated original task prompt")
		}
		if failRepair {
			return nil, errors.New("temporary gateway failure")
		}
		return map[string]any{"text": `{"ok":true}`}, nil
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || tasks != 1 || repairs != 1 {
		t.Fatal("unexpected first attempt", tasks, repairs, err)
	}
	attempts, err := s.Attempts(ctx, run.ID)
	if err != nil || attempts[0].Phase != "repair_admitted" || attempts[0].TaskOutputs == nil {
		t.Fatal("implementation result was not checkpointed", attempts, err)
	}
	failRepair = false
	result, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatal(result, err)
	}
	if tasks != 1 || repairs != 2 {
		t.Fatal("schema-only resume repeated implementation", tasks, repairs)
	}
	attempts, _ = s.Attempts(ctx, run.ID)
	if attempts[1].TaskAttemptID != attempts[0].ID || attempts[1].RepairCount != 2 {
		t.Fatal("schema repair history lost", attempts)
	}
}

func TestLostCompletionAfterRepairReusesBothTaskAndRepair(t *testing.T) {
	ctx := context.Background()
	tasks, repairs := 0, 0
	e, s := durableExecutor(t, repairFaultWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		if !r.RepairOnly {
			tasks++
			return map[string]any{"text": `{"ok":"invalid"}`}, nil
		}
		repairs++
		return map[string]any{"text": `{"ok":true}`}, nil
	}))
	fault := &faultJournal{Journal: s, failCompletion: true}
	e.Store = fault
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || tasks != 1 || repairs != 1 {
		t.Fatal(tasks, repairs, err)
	}
	fault.failCompletion = false
	result, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatal(result, err)
	}
	if tasks != 1 || repairs != 1 {
		t.Fatal("known completed provider calls were replayed", tasks, repairs)
	}
}
