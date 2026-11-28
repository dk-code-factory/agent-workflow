package workflows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const calledSkillWorkflow = `on:
  workflow_call:
    inputs:
      supplied: {type: skill, required: true}
      task: {type: object, required: true}
jobs:
  work:
    steps:
      - id: selected
        uses: agent/run
        with:
          agent: {select: '${{ inputs.task.role }}', definitions: {review: reviewer.yaml, alternate: alternate.yaml}}
          prompt: Work.
          skill: {input: supplied, arguments: '${{ inputs.task.argument }}', resources: [reference.txt], evidence: {invocation: required}}
      - id: continued
        uses: agent/run
        with: {agent: '${{ steps.selected.outputs.agent }}', prompt: Continue.}
      - id: sibling
        uses: agent/run
        with: {agent: reviewer.yaml, prompt: Independent.}
`

func reusableSkillFixture(t *testing.T) (*Executor, *SQLiteStore, *suppliedSession, map[string]any, string) {
	t.Helper()
	directory, inputs := suppliedSkillFixture(t)
	source := `on: {workflow_call: {inputs: {selected: {type: skill, required: true}, tasks: {type: array, required: true}}}}
jobs:
  dispatch:
    uses: workflows/wrapper.yml
    with: {forwarded: '${{ inputs.selected }}', task: '${{ item }}'}
    for-each: {items: '${{ inputs.tasks }}', key: item.id}
`
	e, store, fake := skillExecutorFixture(t, source, false)
	writeWorkflow(t, e.WorkspaceDir, "wrapper.yml", `on: {workflow_call: {inputs: {forwarded: {type: skill, required: true}, task: {type: object, required: true}}}}
jobs:
  dispatch:
    uses: workflows/native.yml
    with: {supplied: '${{ inputs.forwarded }}', task: '${{ inputs.task }}'}
`)
	writeWorkflow(t, e.WorkspaceDir, "native.yml", calledSkillWorkflow)
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows/alternate.yaml"), []byte("name: alternate\ndescription: Other role\ninstructions: Retained alternate instructions.\nmodel: alternate-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inputs["tasks"] = []any{
		map[string]any{"id": "one", "role": "review", "argument": "first"},
		map[string]any{"id": "two", "role": "alternate", "argument": "second"},
	}
	return e, store, fake, inputs, directory
}

func TestSkillReusableCatalogOccurrencesRetainBytesAndIsolateSiblings(t *testing.T) {
	e, store, fake, inputs, directory := reusableSkillFixture(t)
	prepared, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	// Recovery must not consult definitions, packages or the originally supplied
	// path, even before the first occurrence has been pinned.
	if err = os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(e.WorkspaceDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	run, err := e.Resume(t.Context(), prepared.ID, nil, false)
	if err != nil || run.Status != RunStatusSucceeded {
		t.Fatal(run, err)
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil || len(workers) != 4 {
		t.Fatal(len(workers), err)
	}
	selected := map[string]*SkillInvocation{}
	for _, worker := range workers {
		if worker.Skill == nil {
			if worker.SkillEvidence != nil {
				t.Fatal("sibling received skill evidence")
			}
			continue
		}
		selected[worker.Skill.Arguments] = worker.Skill
		if worker.Skill.Occurrence == "" || worker.Remaining != 30 {
			t.Fatal("occurrence/session not pinned", worker)
		}
		if err = worker.SkillEvidence.Complete(worker.Skill); err != nil {
			t.Fatal(err)
		}
	}
	if len(selected) != 2 || selected["first"] == nil || selected["second"] == nil || selected["first"].ID == selected["second"].ID {
		t.Fatal("iterations shared skill identity", selected)
	}
	if len(fake.requests) != 6 {
		t.Fatal("session continuation count", len(fake.requests))
	}
	for i := 0; i < len(fake.requests); i += 3 {
		first, continued, sibling := fake.requests[i], fake.requests[i+1], fake.requests[i+2]
		if first.Skill == nil || continued.Skill == nil || continued.SessionID == "" || first.ParentWorkerID != continued.ParentWorkerID || first.Skill.ID != continued.Skill.ID {
			t.Fatal("continuation replaced skill/session", first, continued)
		}
		if sibling.Skill != nil || sibling.SkillRoot != "" {
			t.Fatal("sibling inherited skill", sibling)
		}
		if first.Skill.Arguments == "second" && first.Definition.Name != "alternate" {
			t.Fatal("catalog selection drifted", first)
		}
	}
	occurrences, err := store.skillOccurrences(t.Context(), run.ID)
	if err != nil || len(occurrences) != 4 {
		t.Fatal("missing original/continuation pins", len(occurrences), err)
	}
}

func TestSkillReusableLostCompletionReusesPinnedOccurrence(t *testing.T) {
	e, store, fake, inputs, directory := reusableSkillFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER lose_skill_completion BEFORE UPDATE OF status ON attempts WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'lost skill completion'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil || len(fake.requests) != 1 {
		t.Fatal("fault not exercised", run, err, len(fake.requests))
	}
	before, err := store.skillOccurrences(t.Context(), run.ID)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	if err = os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("DROP TRIGGER lose_skill_completion"); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded {
		t.Fatal(resumed, err)
	}
	if len(fake.requests) != 6 {
		t.Fatal("lost completion repeated provider work", len(fake.requests))
	}
	after, err := store.skillOccurrences(t.Context(), run.ID)
	found := false
	for _, occurrence := range after {
		found = found || actionHash(before[0]) == actionHash(occurrence)
	}
	if err != nil || len(after) != 4 || !found {
		t.Fatal("lost original occurrence", before, after, err)
	}
}

func TestSkillReusableUnknownSourceAndCatalogRejectBeforeOccurrenceClaim(t *testing.T) {
	for _, variant := range []string{"source", "catalog"} {
		t.Run(variant, func(t *testing.T) {
			e, store, fake, inputs, _ := reusableSkillFixture(t)
			if variant == "source" {
				unapproved, _ := suppliedSkillFixture(t)
				writeWorkflow(t, e.WorkspaceDir, "wrapper.yml", fmt.Sprintf(`on: {workflow_call: {inputs: {forwarded: {type: skill}, task: {type: object}}}}
jobs: {dispatch: {uses: workflows/native.yml, with: {supplied: {directory: %q}, task: '${{ inputs.task }}'}}}
`, unapproved))
			} else {
				inputs["tasks"].([]any)[0].(map[string]any)["role"] = "unknown"
			}
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
			if err == nil || run == nil || len(fake.requests) != 0 {
				t.Fatal("invalid selection dispatched", run, err, len(fake.requests))
			}
			var attempts int
			if err = store.db.QueryRow("SELECT count(*) FROM attempts WHERE step_id='selected'").Scan(&attempts); err != nil || attempts != 0 {
				t.Fatal("invalid occurrence claimed", attempts, err)
			}
		})
	}
}

func TestSkillReusableRetainedOccurrenceRejectsChangedProfileBeforeResumeClaim(t *testing.T) {
	e, store, fake, inputs, _ := reusableSkillFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER lose_skill_completion BEFORE UPDATE OF status ON attempts WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'lost completion'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil {
		t.Fatal(run, err)
	}
	before, err := store.QueueResume(t.Context(), run.ID, "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake.reject = true
	if _, err = e.Resume(t.Context(), run.ID, nil, true); err == nil {
		t.Fatal("changed runtime accepted")
	}
	after, _, err := store.Load(t.Context(), run.ID)
	if err != nil || before.Generation != after.Generation || before.Status != after.Status || len(fake.requests) != 1 {
		t.Fatal("rejected resume claimed work", before, after, err)
	}
}

func TestSkillEntryCatalogUsesAdmittedInputAndRetainedDefinition(t *testing.T) {
	_, inputs := suppliedSkillFixture(t)
	source := strings.Replace(skillWorkflow, "agent: reviewer.yaml", "agent: {select: review, definitions: {review: reviewer.yaml}}", 1)
	e, _, fake := skillExecutorFixture(t, source, false)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 1 || fake.requests[0].Skill == nil {
		t.Fatal(run, err, fake.requests)
	}
}

func TestSkillReusableIterationsChooseDifferentAdmittedSources(t *testing.T) {
	e, _, fake, inputs, _ := reusableSkillFixture(t)
	otherDirectory, otherInputs := suppliedSkillFixture(t)
	if err := os.WriteFile(filepath.Join(otherDirectory, "SKILL.md"), []byte("---\nname: probe\ndescription: Second skill.\n---\nEXACT_ADMITTED_SKILL\nSECOND_SOURCE\n"), 0640); err != nil {
		t.Fatal(err)
	}
	inputs["alternate"] = otherInputs["selected"]
	inputs["tasks"] = []any{
		map[string]any{"id": "one", "role": "review", "argument": "same", "skill": inputs["selected"]},
		map[string]any{"id": "two", "role": "review", "argument": "same", "skill": inputs["alternate"]},
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", `on: {workflow_call: {inputs: {selected: {type: skill}, alternate: {type: skill}, tasks: {type: array}}}}
jobs:
  dispatch:
    uses: workflows/native.yml
    with: {supplied: '${{ item.skill }}', task: '${{ item }}'}
    for-each: {items: '${{ inputs.tasks }}', key: item.id}
`)
	prepared, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(otherDirectory); err != nil {
		t.Fatal(err)
	}
	run, err := e.Resume(t.Context(), prepared.ID, nil, false)
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 6 {
		t.Fatal(run, err, len(fake.requests))
	}
	first, second := fake.requests[0].Skill, fake.requests[3].Skill
	if first.Binding.SourceDigest == second.Binding.SourceDigest || first.Binding.Assets.ID == second.Binding.Assets.ID || first.ID == second.ID {
		t.Fatal("different selected bytes shared identity", first, second)
	}
}

func TestSkillReusableUnusedSourceDoesNotExpandOrPoisonSelection(t *testing.T) {
	e, _, fake, inputs, _ := reusableSkillFixture(t)
	otherDirectory, otherInputs := suppliedSkillFixture(t)
	if err := os.Remove(filepath.Join(otherDirectory, "reference.txt")); err != nil {
		t.Fatal(err)
	}
	inputs["other"] = otherInputs["selected"]
	writeWorkflow(t, e.WorkspaceDir, "main.yml", `on: {workflow_call: {inputs: {selected: {type: skill}, other: {type: skill}, tasks: {type: array}}}}
jobs:
  dispatch:
    uses: workflows/native.yml
    with: {supplied: '${{ inputs.selected }}', task: '${{ item }}'}
    for-each: {items: '${{ inputs.tasks }}', key: item.id}
`)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 6 {
		t.Fatal(run, err, len(fake.requests))
	}
}

func TestSkillReusableOnlySelectedIterationLoadsSkill(t *testing.T) {
	e, _, fake, inputs, _ := reusableSkillFixture(t)
	tasks := inputs["tasks"].([]any)
	tasks[0].(map[string]any)["use_skill"] = true
	tasks[1].(map[string]any)["use_skill"] = false
	child := strings.Replace(calledSkillWorkflow, "      - id: selected", "      - id: selected\n        if: '${{ inputs.task.use_skill }}'", 1)
	child = strings.Replace(child, "      - id: continued", "      - id: continued\n        if: '${{ inputs.task.use_skill }}'", 1)
	writeWorkflow(t, e.WorkspaceDir, "native.yml", child)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 4 {
		t.Fatal(run, err, len(fake.requests))
	}
	for index, request := range fake.requests {
		if index < 2 {
			if request.Skill == nil {
				t.Fatal("selected session lost skill", request)
			}
		} else if request.Skill != nil || request.SkillRoot != "" {
			t.Fatal("unselected iteration or sibling loaded skill", request)
		}
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil || len(workers) != 3 {
		t.Fatal(workers, err)
	}
	selected := 0
	for _, worker := range workers {
		if worker.Skill != nil {
			selected++
		}
	}
	if selected != 1 {
		t.Fatal("skill escaped selected iteration", selected)
	}
}

func TestSkillReusableLifecycleContinuationKeepsOriginalBinding(t *testing.T) {
	e, store, fake, inputs, _ := reusableSkillFixture(t)
	child := strings.Replace(calledSkillWorkflow, "      - id: continued", "        finally:\n          - id: finalize\n            uses: agent/run\n            with: {agent: '${{ event.agent }}', prompt: Finalize.}\n      - id: continued", 1)
	writeWorkflow(t, e.WorkspaceDir, "native.yml", child)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 8 {
		t.Fatal(run, err, len(fake.requests))
	}
	for _, offset := range []int{0, 4} {
		first, finalizer, continued := fake.requests[offset], fake.requests[offset+1], fake.requests[offset+2]
		if finalizer.SessionID == "" || first.ParentWorkerID != finalizer.ParentWorkerID || first.Skill.ID != finalizer.Skill.ID || first.Skill.ID != continued.Skill.ID {
			t.Fatal("lifecycle replaced admitted skill", first, finalizer, continued)
		}
	}
	pins, err := store.skillOccurrences(t.Context(), run.ID)
	if err != nil || len(pins) != 6 {
		t.Fatal("lifecycle continuation not pinned", len(pins), err)
	}
}

func TestSkillReusableCrashAfterPinBeforeClaimDoesNotRecapture(t *testing.T) {
	e, store, fake, inputs, directory := reusableSkillFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER lose_skill_claim BEFORE INSERT ON attempts WHEN NEW.step_id='selected' BEGIN SELECT RAISE(ABORT,'crash after skill pin'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil || len(fake.requests) != 0 {
		t.Fatal("preclaim crash not exercised", run, err, len(fake.requests))
	}
	pins, err := store.skillOccurrences(t.Context(), run.ID)
	if err != nil || len(pins) != 1 {
		t.Fatal("pin was not committed before claim", pins, err)
	}
	if err = os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("DROP TRIGGER lose_skill_claim"); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded || len(fake.requests) != 6 {
		t.Fatal(resumed, err, len(fake.requests))
	}
	if fake.requests[0].Skill.ID != pins[0].Skill.ID {
		t.Fatal("preclaim pin replaced", fake.requests[0].Skill, pins[0].Skill)
	}
}

func TestSkillReusableRetryAfterCommandFailureKeepsOriginalSessionAndAllowance(t *testing.T) {
	e, _, fake, inputs, directory := reusableSkillFixture(t)
	inputs["task"] = inputs["tasks"].([]any)[0]
	delete(inputs, "tasks")
	writeWorkflow(t, e.WorkspaceDir, "main.yml", `on: {workflow_call: {inputs: {selected: {type: skill}, task: {type: object}}}}
jobs: {dispatch: {uses: workflows/wrapper.yml, with: {forwarded: '${{ inputs.selected }}', task: '${{ inputs.task }}'}}}
`)
	flag := filepath.Join(e.WorkspaceDir, "retry-ready")
	child := strings.Replace(calledSkillWorkflow, "      - id: continued", fmt.Sprintf("      - id: wait-for-retry\n        argv: [test, -f, %q]\n      - id: continued", flag), 1)
	writeWorkflow(t, e.WorkspaceDir, "native.yml", child)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil || len(fake.requests) != 1 {
		t.Fatal("command failure not exercised", run, err, len(fake.requests))
	}
	first := fake.requests[0]
	if err = os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(flag, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded || len(fake.requests) != 3 {
		t.Fatal(resumed, err, len(fake.requests))
	}
	continued := fake.requests[1]
	if continued.SessionID == "" || continued.ParentWorkerID != first.ParentWorkerID || continued.Skill.ID != first.Skill.ID {
		t.Fatal("retry replaced admitted session", first, continued)
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if worker.ID == first.ParentWorkerID && worker.Remaining != 30 {
			t.Fatal("retry replenished turn allowance", worker.Remaining)
		}
	}
}

func TestSkillReusableTamperedOccurrenceRejectsBeforeResumeClaim(t *testing.T) {
	e, store, fake, inputs, _ := reusableSkillFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER lose_skill_claim BEFORE INSERT ON attempts WHEN NEW.step_id='selected' BEGIN SELECT RAISE(ABORT,'crash after pin'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil {
		t.Fatal(run, err)
	}
	if _, err = store.db.Exec(`UPDATE iteration_plans SET record=json_set(CAST(record AS TEXT),'$.items[0].inputs.skill.arguments','tampered') WHERE substr(job_id,1,7)='$skill/'`); err != nil {
		t.Fatal(err)
	}
	queued, err := store.QueueResume(t.Context(), run.ID, "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, true); err == nil || !strings.Contains(err.Error(), "skill occurrence identity changed") {
		t.Fatal("tampered pin accepted", err)
	}
	after, _, err := store.Load(t.Context(), run.ID)
	if err != nil || queued.Generation != after.Generation || after.Status != RunStatusQueued || len(fake.requests) != 0 {
		t.Fatal("tampered occurrence claimed work", queued, after, err)
	}
}

func TestSkillReusableChecklistContinuationsKeepOriginalBinding(t *testing.T) {
	e, _, fake, inputs, _ := reusableSkillFixture(t)
	child := strings.Replace(calledSkillWorkflow, "          prompt: Work.", "          prompt: Work.\n          checklist: {max-retries: 0, stages: [Verify.]}", 1)
	writeWorkflow(t, e.WorkspaceDir, "native.yml", child)
	original := fake.respond
	fake.respond = func(req SessionRequest) (map[string]any, error) {
		if req.Output != nil {
			return map[string]any{"structured": map[string]any{"complete": true, "evidence": []any{"verified"}, "remaining": []any{}}}, nil
		}
		return original(req)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil || run.Status != RunStatusSucceeded || len(fake.requests) != 8 {
		t.Fatal(run, err, len(fake.requests))
	}
	for _, offset := range []int{0, 4} {
		first, checklist, continued, sibling := fake.requests[offset], fake.requests[offset+1], fake.requests[offset+2], fake.requests[offset+3]
		if checklist.SessionID == "" || first.ParentWorkerID != checklist.ParentWorkerID || first.Skill.ID != checklist.Skill.ID || first.Skill.ID != continued.Skill.ID || sibling.Skill != nil || sibling.SkillRoot != "" {
			t.Fatal("checklist changed skill isolation", first, checklist, continued, sibling)
		}
	}
}

func TestSkillReusableContinuationCannotChangeBindingBeforeClaim(t *testing.T) {
	for _, variant := range []string{"model", "workspace", "skill"} {
		t.Run(variant, func(t *testing.T) {
			e, store, fake, inputs, _ := reusableSkillFixture(t)
			value := "model: different-model"
			if variant == "workspace" {
				value = "workspace: different-workspace"
			}
			if variant == "skill" {
				value = "skill: {input: supplied, arguments: changed, resources: [reference.txt], evidence: {invocation: required}}"
			}
			child := strings.Replace(calledSkillWorkflow, "prompt: Continue.", "prompt: Continue., "+value, 1)
			writeWorkflow(t, e.WorkspaceDir, "native.yml", child)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
			if err == nil || run == nil || len(fake.requests) != 1 {
				t.Fatal("changed continuation reached provider", run, err, len(fake.requests))
			}
			var attempts int
			if err = store.db.QueryRow("SELECT count(*) FROM attempts WHERE step_id='continued'").Scan(&attempts); err != nil || attempts != 0 {
				t.Fatal("changed continuation claimed", attempts, err)
			}
		})
	}
}
