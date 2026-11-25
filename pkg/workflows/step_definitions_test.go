package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const reusableStepsWorkflow = `step-definitions:
  review:
    parameters:
      prompt: {type: string, required: true}
    step:
      sequence:
        - id: check
          uses: agent/run
          with: {agent: a.yaml, prompt: '${{ parameters.prompt }}', turns: 1}
        - id: refine
          uses: agent/run
          with: {agent: a.yaml, prompt: '${{ steps.check.outputs.text }} refined', turns: 1}
jobs:
  review:
    steps:
      - {id: first, use-definition: review, with: {prompt: first}}
      - {id: second, use-definition: review, with: {prompt: '${{ steps.first__refine.outputs.text }}'}}
    outputs: {text: '${{ steps.second__refine.outputs.text }}'}
`

func TestStepDefinitionsExpansionAndValidation(t *testing.T) {
	for _, tc := range []struct{ name, old, next, want string }{
		{name: "valid"},
		{"unknown definition", "use-definition: review", "use-definition: absent", "unknown step definition"},
		{"unknown binding", "with: {prompt: first}", "with: {extra: first}", "unknown parameter"},
		{"missing binding", "with: {prompt: first}", "with: {}", "required parameter"},
		{"wrong type", "with: {prompt: first}", "with: {prompt: 42}", "must have type string"},
		{"unknown parameter reference", "parameters.prompt", "parameters.absent", "unknown parameter"},
		{"textual parameter interpolation", "${{ parameters.prompt }}", "prefix ${{ parameters.prompt }}", "whole value"},
		{"missing use identity", "id: first, use-definition", "use-definition", "explicit valid id"},
		{"mixed action", "id: first, use-definition", "argv: [echo], id: first, use-definition", "cannot combine"},
		{"duplicate expanded ID", "id: second, use-definition", "id: first, use-definition", "duplicate step id"},
		{"duplicate local ID", "id: refine", "id: check", "duplicate definition step id"},
		{"forward reference", "steps.check.outputs.text", "steps.refine.outputs.text", "unavailable"},
		{"unknown result", "steps.check.outputs.text", "steps.absent.outputs.text", "definition-local step"},
		{"private instance scope", "steps.check.outputs.text", "steps.second__check.outputs.text", "definition-local step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := reusableStepsWorkflow
			if tc.old != "" {
				source = strings.Replace(source, tc.old, tc.next, 1)
			}
			w, err := Parse([]byte(source))
			if err == nil {
				err = Validate(w)
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			first, second := w.Jobs["review"].Steps[0], w.Jobs["review"].Steps[1]
			if first.Sequence[0].ID != "first__check" || second.Sequence[0].ID != "second__check" || second.Sequence[1].With["prompt"] != "${{ steps.second__check.outputs.text }} refined" {
				t.Fatalf("expansion: %+v %+v", first, second)
			}
			if second.Sequence[0].With["prompt"] != "${{ steps.first__refine.outputs.text }}" {
				t.Fatal("caller binding was rewritten into definition scope")
			}
			if first.Reuse.Definition != "review" || first.Sequence[0].Reuse.Instance != "first" || first.Reuse.UseLine == 0 || first.Reuse.DefinitionLine == 0 {
				t.Fatal("missing provenance")
			}
			again, err := Parse([]byte(source))
			if err != nil || !reflect.DeepEqual(w, again) {
				t.Fatalf("nondeterministic expansion: %v", err)
			}
		})
	}
}

func TestStepDefinitionsBoundsAndCycles(t *testing.T) {
	for _, body := range []string{
		"sequence: [{id: loop, use-definition: self}]",
		"parallel: [{sequence: [{id: loop, use-definition: self}]}]",
	} {
		_, err := Parse([]byte("step-definitions: {self: {step: {" + body + "}}}\njobs: {x: {steps: [{argv: [echo]}]}}"))
		if err == nil || !strings.Contains(err.Error(), "recursive") {
			t.Fatalf("cycle: %v", err)
		}
	}
	var defs strings.Builder
	for i := 0; i < 18; i++ {
		body := "argv: [echo]"
		if i < 17 {
			body = fmt.Sprintf("sequence: [{id: child, use-definition: d%d}]", i+1)
		}
		fmt.Fprintf(&defs, "  d%d: {step: {%s}}\n", i, body)
	}
	_, err := Parse([]byte("step-definitions:\n" + defs.String() + "jobs: {x: {steps: [{id: use, use-definition: d0}]}}"))
	if err == nil || !strings.Contains(err.Error(), "nesting exceeds") {
		t.Fatalf("depth: %v", err)
	}
	source := "step-definitions: {one: {step: {argv: [echo]}}}\njobs:\n  x:\n    steps:\n"
	for i := 0; i < maxExpandedSteps+1; i++ {
		source += fmt.Sprintf("      - {id: s%d, use-definition: one}\n", i)
	}
	if _, err := Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "expanded steps exceed") {
		t.Fatalf("capacity: %v", err)
	}
}

func TestStepDefinitionsTypedDefaultsAndNestedUses(t *testing.T) {
	source := `step-definitions:
  leaf:
    parameters:
      command: {type: array, default: [echo, default]}
      tolerate: {type: boolean, default: true}
    step:
      argv: '${{ parameters.command }}'
      continue-on-error: '${{ parameters.tolerate }}'
  outer:
    step:
      sequence:
        - {id: leaf, use-definition: leaf}
jobs:
  x:
    steps:
      - {id: a, use-definition: outer}
      - {id: b, use-definition: leaf, with: {command: [echo, override], tolerate: false}}
`
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	a, b := w.Jobs["x"].Steps[0].Sequence[0], w.Jobs["x"].Steps[1]
	if a.ID != "a__leaf" || !a.ContinueOnError || !reflect.DeepEqual(a.Argv, []string{"echo", "default"}) || b.ContinueOnError || b.Argv[1] != "override" {
		t.Fatalf("typed expansion: %+v %+v", a, b)
	}
}

func TestStepDefinitionsPlacementAndBindingsBeforeEffects(t *testing.T) {
	for _, tc := range []struct{ body, job, want string }{
		{"uses: agent/run, with: {agent: a.yaml, prompt: hi}", "container: ubuntu\n", "shared containers"},
		{"argv: [python3], protocol: action-v1, effect: read_only, input-schema: {type: object}, output-schema: {type: object}", "", "requires a job container"},
	} {
		w, err := Parse([]byte("step-definitions: {s: {step: {" + tc.body + "}}}\njobs:\n  x:\n    " + tc.job + "    steps: [{id: use, use-definition: s}]\n"))
		if err == nil {
			err = Validate(w)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, want %s", err, tc.want)
		}
	}
	source := `step-definitions:
  check:
    parameters: {text: {type: string, required: true}}
    step: {argv: [echo, '${{ parameters.text }}']}
jobs: {x: {steps: [{id: use, use-definition: check, with: {text: '${{ inputs.text }}'}}]}}
`
	w, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	step := w.Jobs["x"].Steps[0]
	if err := validateReuseBindings(step, expressionContext{Inputs: map[string]any{"text": 42}}); err == nil {
		t.Fatal("runtime type mismatch accepted")
	}
	if err := validateReuseBindings(step, expressionContext{Inputs: map[string]any{"text": "ok"}}); err != nil {
		t.Fatal(err)
	}
}

func TestStepDefinitionsRestartPinsExpansionAndAgentAssets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	calls := map[string]int{}
	e, store := durableExecutor(t, reusableStepsWorkflow, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		calls[req.Prompt]++
		return map[string]any{"text": req.Prompt}, nil
	}))
	e.Slots = make(chan struct{}, 1)
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "first"}
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil {
		t.Fatal("expected lost block checkpoint")
	}
	if err := os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", "main.yml"), []byte("invalid: changed"), 0600); err != nil {
		t.Fatal(err)
	}
	// Native definition paths are retained too; resumed work cannot reread them.
	if err := os.Remove(filepath.Join(e.WorkspaceDir, "workflows", "a.yaml")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	e.Store = store
	run, err = nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, false)
	if err != nil || run.Status != RunStatusSucceeded {
		t.Fatalf("resume: %+v %v", run, err)
	}
	if run.Jobs["review"].Outputs["text"] != "first refined refined" {
		t.Fatal(run.Jobs["review"].Outputs)
	}
	if calls["first"] != 1 || calls["first refined"] != 2 || calls["first refined refined"] != 1 {
		t.Fatalf("replayed or missing effects: %v", calls)
	}
	if len(e.Slots) != 0 {
		t.Fatal("capacity leaked")
	}
	workers, err := e.Workers.List(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 4 {
		t.Fatalf("worker instances: %d", len(workers))
	}
	for _, worker := range workers {
		if worker.Remaining != 0 {
			t.Fatalf("replenished turn budget: %+v", worker)
		}
	}
	result := run.Jobs["review"].Steps["second__check"]
	if result.Reuse == nil || result.Reuse.Instance != "second" {
		t.Fatalf("inspection lost provenance: %+v", result)
	}
	_, snapshot, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	if !strings.Contains(string(data), "step-definitions") || !strings.Contains(string(data), "second__check") {
		t.Fatal("snapshot missing source or expansion")
	}
}

func TestStepDefinitionsNullEntriesAndLiteralOperands(t *testing.T) {
	source := `step-definitions:
  s:
    step:
      sequence:
        - null
        - {id: check, argv: [echo, check]}
        - id: compare
          name: '${{ steps.check.outputs.stdout }}'
          argv: [echo, '${{ steps.check.outputs.stdout }}']
          if: "steps.check.outputs.stdout != 'steps.check.outputs.stdout'"
jobs:
  x:
    steps:
      - null
      - {id: first, use-definition: s}
`
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	steps := w.Jobs["x"].Steps
	if len(steps) != 1 || steps[0].Reuse == nil || len(steps[0].Sequence) != 2 || steps[0].Sequence[1].Reuse == nil {
		t.Fatalf("null steps lost provenance: %+v", steps)
	}
	leaf := steps[0].Sequence[1]
	if leaf.Name != "${{ steps.check.outputs.stdout }}" || leaf.If != "steps.first__check.outputs.stdout != 'steps.check.outputs.stdout'" {
		t.Fatalf("literal rewritten: %+v", leaf)
	}
}

func TestStepDefinitionsHelperInstancesRetainIndependentReceipts(t *testing.T) {
	e, store, d := containerExecutor(t)
	f := &protocolFixture{binding: "resource-a"}
	source := `step-definitions:
  helper:
    parameters: {value: {type: string, required: true}}
    step:
      argv: [helper]
      protocol: action-v1
      effect: read_only
      input-schema: {type: object}
      output-schema: {type: object}
      input: {value: '${{ parameters.value }}'}
jobs:
  capture:
    container: {image: ubuntu:24.04, files: [helpers/]}
    steps:
      - {id: first, use-definition: helper, with: {value: first}}
      - {id: second, use-definition: helper, with: {value: second}}
`
	putHelper(t, e.WorkspaceDir, "workflows/helpers/retained.txt", "admitted data", 0644)
	installProtocolWorkflow(t, e, d, f, source)
	ctx := t.Context()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if f.execute != 2 {
		t.Fatalf("execute count: %d", f.execute)
	}
	keys := map[string]bool{}
	values := map[string]bool{}
	for _, r := range f.requests {
		if r["phase"] == "execute" {
			keys[r["context"].(map[string]any)["operation_key"].(string)] = true
			values[r["input"].(map[string]any)["value"].(string)] = true
		}
	}
	if len(keys) != 2 || !values["first"] || !values["second"] {
		t.Fatalf("shared identity/input: %v %v", keys, values)
	}
	_, snap, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Definitions["workflows/test.yml"].Jobs["capture"].Container.Implementation == nil {
		t.Fatal("missing pinned helper implementation")
	}
	if err := os.RemoveAll(filepath.Join(e.WorkspaceDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resume(ctx, run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if f.execute != 2 || f.validations == 0 {
		t.Fatalf("recovery replayed helper effects: execute=%d validate=%d", f.execute, f.validations)
	}
	if snap.Definitions["workflows/test.yml"].Jobs["capture"].Container.Implementation.Bundle == nil {
		t.Fatal("missing pinned helper bundle")
	}
}

func TestStepDefinitionsLifecycleScopesAndUsePolicies(t *testing.T) {
	source := `step-definitions:
  task:
    parameters: {message: {type: string, required: true}}
    step:
      argv: [echo, '${{ parameters.message }}']
      continue-on-error: true
      finally:
        - {id: a, argv: [echo, cleanup]}
        - {id: b, argv: [echo, '${{ steps.a.outputs.stdout }}']}
  sequence:
    step:
      sequence:
        - {id: seed, argv: [echo, seed]}
        - id: work
          argv: [echo, '${{ steps.seed.outputs.stdout }}']
          finally:
            - {id: seed, argv: [echo, '${{ steps.seed.outputs.stdout }}']}
            - {id: after, argv: [echo, '${{ steps.seed.outputs.stdout }}']}
jobs:
  x:
    steps:
      - {id: first, use-definition: task, with: {message: first}, continue-on-error: false}
      - {id: second, use-definition: task, with: {message: second}}
      - {id: third, use-definition: sequence}
`
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	steps := w.Jobs["x"].Steps
	if steps[0].ContinueOnError || !steps[1].ContinueOnError || steps[0].Finally[1].Argv[1] != "${{ steps.a.outputs.stdout }}" {
		t.Fatalf("use policy or handler scope changed: %+v", steps)
	}
	work := steps[2].Sequence[1]
	if work.Finally[0].Argv[1] != "${{ steps.third__seed.outputs.stdout }}" || work.Finally[1].Argv[1] != "${{ steps.seed.outputs.stdout }}" {
		t.Fatalf("handler shadowing changed: %+v", work.Finally)
	}
	if _, err := Parse([]byte("step-definitions: {unused: {step: {unknown: invalid}}}\njobs: {x: {steps: [{argv: [echo]}]}}")); err == nil {
		t.Fatal("unknown template field accepted")
	}
}

func TestStepDefinitionsRejectRuntimeBindingBeforeLaunchingCommand(t *testing.T) {
	source := `step-definitions:
  command:
    parameters: {text: {type: string, required: true}}
    step: {argv: [sh, -c, 'touch should-not-exist'], with: {input: '${{ parameters.text }}'}}
jobs:
  x:
    steps: [{id: use, use-definition: command, with: {text: '${{ inputs.text }}'}}]
`
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", source)
	e := &Executor{WorkspaceDir: root}
	_, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"text": 42}})
	if err == nil || !strings.Contains(err.Error(), "must have type string") {
		t.Fatalf("wrong binding accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("invalid binding launched command: %v", err)
	}
}

func TestStepDefinitionsRejectExpandedIdentityCollisionAndMaterialization(t *testing.T) {
	source := reusableStepsWorkflow + "      # end\n"
	source = strings.Replace(source, "      - {id: second,", "      - {id: first__check, argv: [echo]}\n      - {id: second,", 1)
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err == nil || !strings.Contains(err.Error(), "duplicate step id") {
		t.Fatalf("expanded identity collision: %v", err)
	}
	large := strings.Repeat("x", 1<<20)
	source = "step-definitions: {copy: {parameters: {text: {type: string, required: true}}, step: {argv: [echo, '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}', '${{ parameters.text }}']}}}\njobs: {x: {steps: [{id: use, use-definition: copy, with: {text: '" + large + "'}}]}}"
	if _, err := Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatalf("unbounded parameter duplication: %v", err)
	}
}

func TestStepDefinitionsNamedOutputSchemasUseInstanceScope(t *testing.T) {
	source := `schemas:
  Choice:
    type: object
    required: [choice]
    properties:
      choice: {type: string, enum: ['${{ steps.seed.outputs.text }}']}
step-definitions:
  task:
    step:
      sequence:
        - {id: seed, argv: [echo, seed]}
        - id: choose
          uses: agent/run
          with: {agent: a.yaml, prompt: Choose, output: {format: json, schema: Choice}}
jobs:
  x:
    steps:
      - {id: first, use-definition: task}
      - {id: second, use-definition: task}
`
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range w.Jobs["x"].Steps {
		output := step.Sequence[1].With["output"].(map[string]any)
		enum := output["schema"].(map[string]any)["properties"].(map[string]any)["choice"].(map[string]any)["enum"].([]any)
		if enum[0] != "${{ steps."+step.ID+"__seed.outputs.text }}" {
			t.Fatalf("shared schema bound to another instance: %v", enum)
		}
	}
}

func TestStepDefinitionsSkippedUsesDoNotEvaluateRuntimeBindings(t *testing.T) {
	source := `step-definitions:
  command:
    parameters: {text: {type: string, required: true}}
    step: {argv: [echo, '${{ parameters.text }}']}
jobs:
  x:
    steps: [{id: use, use-definition: command, if: 'false', with: {text: '${{ inputs.absent }}'}}]
`
	w, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	step := w.Jobs["x"].Steps[0]
	e := &Executor{}
	ec := expressionContext{Steps: map[string]StepExecution{}}
	result, err := e.executeDeclaredStep(t.Context(), step, "step_1", ec, RunRequest{}, nil)
	if err != nil || result.Status != RunStatusSkipped {
		t.Fatalf("skipped use evaluated bindings: %+v %v", result, err)
	}
	result, err = e.executeLifecycleStep(t.Context(), step, ec, nil, false, true)
	if err != nil || result.Status != RunStatusSkipped {
		t.Fatalf("skipped handler use evaluated bindings: %+v %v", result, err)
	}
}
