package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checklistWorkflow(config string) string {
	return "jobs:\n  review:\n    steps:\n      - id: task\n        uses: agent/run\n        with:\n          agent: reviewer.yaml\n          prompt: implement\n          checklist: " + config + "\n"
}

func checklistResponse(complete bool) map[string]any {
	return map[string]any{"text": fmt.Sprintf(`{"complete":%t,"evidence":["inspected changes"],"remaining":[]}`, complete)}
}

func TestChecklistAdmission(t *testing.T) {
	for _, config := range []string{
		"null", "{}", "{max-retries: 1, stages: []}", "{max-retries: -1, stages: [verify]}",
		"{max-retries: 101, stages: [verify]}", "{max-retries: '2', stages: [verify]}",
		"{max-retries: 1.5, stages: [verify]}", "{max-retries: true, stages: [verify]}",
		"{max-retries: 1, stages: [!!binary /w==]}",
		"{max-retries: 1, stages: [{argv: [echo, !!binary /w==]}]}",
		"{max-retries: 1, stages: [null]}", "{max-retries: 1, stages: ['']}",
		"{max-retries: 1, stages: [{prompt: verify, max-retries: -1}]}",
		"{max-retries: 1, stages: [{prompt: verify, argv: [true]}]}",
		"{max-retries: 1, stages: [{run: test}]}",
		"{max-retries: 1, stages: [{argv: []}]}",
		"{max-retries: 1, stages: [{argv: [3]}]}",
		"{max-retries: 1, stages: [{argv: [true], timeout: 0s}]}",
		"{max-retries: 1, stages: [{prompt: verify, timeout: 1m}]}",
		"{max-retries: 1, stages: [{prompt: verify, extra: yes}]}",
		"{max-retries: 1, stages: [verify], extra: yes}",
	} {
		t.Run(config, func(t *testing.T) {
			e, _, fake := agentTaskFixture(t, checklistWorkflow(config), nil)
			if _, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil {
				t.Fatal("invalid checklist admitted")
			}
			if len(fake.requests) != 0 {
				t.Fatal("provider called during admission")
			}
		})
	}
}

func TestChecklistStagesShareSessionAndBudget(t *testing.T) {
	calls := 0
	e, _, fake := agentTaskFixture(t, checklistWorkflow("{max-retries: 2, stages: [{prompt: first, max-retries: 1}, second]}"), func(req SessionRequest) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"text": "original deliverable"}, nil
		}
		return checklistResponse(calls == 3 || calls == 5), nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	result := run.Jobs["review"].Steps["task"].Outputs
	if result["text"] != "original deliverable" {
		t.Fatal(result)
	}
	report := result["checklist"].(map[string]any)
	if report["complete"] != true || fmt.Sprint(report["retries"]) != "2" || calls != 5 {
		t.Fatal(report, calls)
	}
	for i, req := range fake.requests {
		if req.ParentWorkerID != fake.requests[0].ParentWorkerID || i > 0 && req.SessionID == "" {
			t.Fatal("checklist changed session", fake.requests)
		}
	}
}

func TestChecklistExhaustionStopsStagesAndRunsFailureHooks(t *testing.T) {
	for _, config := range []string{
		"{max-retries: 1, stages: [{prompt: first, max-retries: 3}, never]}",
		"{max-retries: 4, stages: [{prompt: first, max-retries: 1}, never]}",
		"{max-retries: 0, stages: [first, never]}",
	} {
		t.Run(config, func(t *testing.T) {
			source := checklistWorkflow(config) + "        onFailure:\n          - argv: [sh, -c, 'echo failed > failure-marker']\n"
			calls := 0
			e, _, _ := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
				calls++
				if strings.Contains(req.Prompt, "never") {
					t.Error("later stage ran")
				}
				return checklistResponse(false), nil
			})
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), "checklist retry budget exhausted") {
				t.Fatal(run, err)
			}
			if _, err := os.Stat(filepath.Join(e.WorkspaceDir, "failure-marker")); err != nil {
				t.Fatal("failure hook did not run", err)
			}
			if calls > 3 {
				t.Fatal("retry bound exceeded", calls)
			}
			before := calls
			if _, err := e.Resume(context.Background(), run.ID, nil, true); err == nil {
				t.Fatal("exhaustion reset on resume")
			}
			if calls != before {
				t.Fatal("resume replenished retries", calls, before)
			}
		})
	}
}

func TestChecklistCommandRepairsBeforeRecheck(t *testing.T) {
	source := checklistWorkflow("{max-retries: 1, stages: [{argv: [sh, -c, 'test -f repaired || { echo ''${{ inputs.literal }}'' >&2; exit 1; }'], max-retries: 1}, {shell: sh, run: 'test -f repaired'}]}")
	var workspace string
	calls := 0
	e, _, fake := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"text": "implemented"}, nil
		}
		input, _ := json.Marshal(req.Input)
		if !strings.Contains(string(input), "${{ secrets.unavailable }}") {
			t.Error("command feedback was evaluated or lost", string(input))
		}
		if err := os.WriteFile(filepath.Join(workspace, "repaired"), []byte("fixed"), 0600); err != nil {
			return nil, err
		}
		return map[string]any{"text": "fixed"}, nil
	})
	workspace = e.WorkspaceDir
	// Expression-shaped command output must remain data on its way to the agent.
	writeWorkflow(t, e.WorkspaceDir, "main.yml", "on: {workflow_call: {inputs: {literal: {type: string}}}}\n"+source)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"literal": "${{ secrets.unavailable }}"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || fake.requests[1].ParentWorkerID != fake.requests[0].ParentWorkerID {
		t.Fatal("repair changed session", fake.requests)
	}
	if run.Jobs["review"].Steps["task"].Outputs["checklist"].(map[string]any)["complete"] != true {
		t.Fatal(run)
	}
}

type checklistLostCompletion struct {
	*SQLiteStore
	lost bool
}

func (s *checklistLostCompletion) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	if a.StepID == "task" && !s.lost {
		s.lost = true
		return errors.New("injected storage failure before checklist owner completion")
	}
	return s.SQLiteStore.Complete(ctx, a, result)
}

func TestChecklistRecoveryReusesStagesAndRetryConsumption(t *testing.T) {
	calls := 0
	e, store, _ := agentTaskFixture(t, checklistWorkflow("{max-retries: 1, stages: [verify, finish]}"), func(req SessionRequest) (map[string]any, error) {
		calls++
		return checklistResponse(calls != 2), nil
	})
	e.Store = &checklistLostCompletion{SQLiteStore: store}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("completion loss not injected")
	}
	before := calls
	run, err = e.Resume(context.Background(), run.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != before || calls != 4 {
		t.Fatal("completed turns replayed", calls, before)
	}
	report := run.Jobs["review"].Steps["task"].Outputs["checklist"].(map[string]any)
	if report["complete"] != true || fmt.Sprint(report["retries"]) != "1" {
		t.Fatal(report)
	}
}

func TestChecklistCommandMustPassAfterRepair(t *testing.T) {
	calls := 0
	e, _, _ := agentTaskFixture(t, checklistWorkflow("{max-retries: 2, stages: [{argv: [sh, -c, 'exit 1'], max-retries: 1}, never]}"), func(req SessionRequest) (map[string]any, error) {
		calls++
		return checklistResponse(true), nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "checklist retry budget exhausted") || calls != 2 {
		t.Fatal("repair claim bypassed failing command", run, err, calls)
	}
}

func TestChecklistOperationalFailuresDoNotRetry(t *testing.T) {
	for _, command := range []string{
		"{argv: [sh, -c, 'sleep 1'], timeout: 1ms}",
	} {
		t.Run(command, func(t *testing.T) {
			calls := 0
			e, _, _ := agentTaskFixture(t, checklistWorkflow("{max-retries: 3, stages: ["+command+", never]}"), func(SessionRequest) (map[string]any, error) {
				calls++
				return checklistResponse(true), nil
			})
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || calls != 1 {
				t.Fatal("operational failure entered repair", err, calls)
			}
		})
	}
}

func TestChecklistRespectsTaskWorkspaceAndTurnAllowance(t *testing.T) {
	source := strings.Replace(checklistWorkflow("{max-retries: 0, stages: [{argv: [sh, -c, 'test -f expected']}, verify]}"), "          prompt: implement", "          prompt: implement\n          workspace: nested\n          turns: 2", 1)
	calls := 0
	e, _, _ := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) { calls++; return checklistResponse(true), nil })
	if err := os.Mkdir(filepath.Join(e.WorkspaceDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "nested", "expected"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

type checklistLostCommandReceipt struct {
	*SQLiteStore
	lost bool
}

func (s *checklistLostCommandReceipt) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	if err := s.SQLiteStore.Complete(ctx, a, result); err != nil {
		return err
	}
	if strings.Contains(a.StepID, "/stage_1/attempt_1/check") && !s.lost {
		s.lost = true
		return errors.New("injected storage response loss after command completion")
	}
	return nil
}

func TestChecklistRecoveryReusesFailedCommandBeforeRepair(t *testing.T) {
	var workspace string
	calls := 0
	e, store, _ := agentTaskFixture(t, checklistWorkflow("{max-retries: 1, stages: [{shell: sh, run: 'echo observed >> checks; test -f fixed'}]}"), func(SessionRequest) (map[string]any, error) {
		calls++
		if calls > 1 {
			return map[string]any{"text": "fixed"}, os.WriteFile(filepath.Join(workspace, "fixed"), nil, 0600)
		}
		return map[string]any{"text": "implemented"}, nil
	})
	workspace = e.WorkspaceDir
	e.Store = &checklistLostCommandReceipt{SQLiteStore: store}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("response loss not injected")
	}
	run, err = e.Resume(context.Background(), run.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "checks"))
	if err != nil || strings.Count(string(data), "observed") != 2 || calls != 2 {
		t.Fatal("failed check replayed before repair", string(data), err, calls)
	}
	report := run.Jobs["review"].Steps["task"].Outputs["checklist"].(map[string]any)
	if report["complete"] != true || fmt.Sprint(report["retries"]) != "1" {
		t.Fatal(report)
	}
}

func TestChecklistPreservesWorkflowEvent(t *testing.T) {
	calls := 0
	source := "on: {workflow_call: {inputs: {literal: {type: string}}}}\n" + checklistWorkflow(`{max-retries: 0, stages: [{prompt: 'Verify ${{ event.expected }}'}, {argv: [test, '${{ event.expected }}', '=', '${{ inputs.literal }}']}, {shell: sh, run: 'test "${{ event.plain }}" = expected'}]}`)
	e, _, _ := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
		calls++
		if calls > 1 && !strings.Contains(req.Prompt, "Verify ${{ secrets.unavailable }}") {
			t.Errorf("workflow event lost or evaluated twice: %s", req.Prompt)
		}
		return checklistResponse(true), nil
	})
	_, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Event: map[string]any{"expected": "${{ secrets.unavailable }}", "plain": "expected"}, Inputs: map[string]any{"literal": "${{ secrets.unavailable }}"}})
	if err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}

func TestChecklistOrderedContinuedFailure(t *testing.T) {
	for _, successfulTurns := range []int{0, 1} {
		t.Run(fmt.Sprint(successfulTurns), func(t *testing.T) {
			calls := 0
			source := strings.Replace(checklistWorkflow("{max-retries: 0, stages: [verify]}"), "        uses: agent/run", "        uses: agent/run\n        continue-on-error: true", 1) + "      - id: disposition\n        argv: [true]\n"
			e, _, _ := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) {
				calls++
				if calls > successfulTurns {
					return nil, ErrProviderFailure
				}
				return map[string]any{"text": "implemented"}, nil
			})
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", ordered: true})
			if err != nil || run.Jobs["review"].Steps["task"].FailureClass != "provider_failure" || run.Jobs["review"].Steps["disposition"].Status != RunStatusSucceeded {
				t.Fatal(run, err)
			}
		})
	}
}

func TestChecklistSummarySurvivesLifecycleReplacement(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			source := strings.Replace(checklistWorkflow("{max-retries: 0, stages: [verify]}"), "          prompt: implement", "          prompt: implement\n          output: {format: json, schema: {type: object, required: [complete], properties: {complete: {type: boolean}}}}", 1) + "        onIncomplete:\n          - uses: agent/run\n            with: {agent: '${{ event.agent }}', prompt: finish}\n"
			calls := 0
			e, store, _ := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) {
				calls++
				return checklistResponse(calls != 1), nil
			})
			if lost {
				e.Store = &checklistLostCompletion{SQLiteStore: store}
			}
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if lost {
				if err == nil {
					t.Fatal("completion fault was not reached")
				}
				e.Store = store
				run, err = e.Resume(context.Background(), run.ID, nil, true)
			}
			if err != nil || calls != 3 {
				t.Fatal(run, err, calls)
			}
			outputs := run.Jobs["review"].Steps["task"].Outputs
			report, ok := outputs["checklist"].(map[string]any)
			if !ok || report["complete"] != true || len(report["stages"].([]any)) != 1 || outputs["complete"] != true {
				t.Fatal(outputs)
			}
		})
	}
}
