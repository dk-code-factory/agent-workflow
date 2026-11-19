package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"gopkg.in/yaml.v3"
)

func TestLogicalORPrecedenceShortCircuitAndQuotedOperands(t *testing.T) {
	for expression, want := range map[string]bool{
		"true or false and false":                         true,
		"false and true or true":                          true,
		"false or true and false":                         false,
		"true or missing.value":                           true,
		"false and missing.value or true":                 true,
		"'literal or text' == 'literal or text' and true": true,
		"false or not false and true":                     true,
	} {
		got, err := evalIf(expression, expressionContext{})
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", expression, got, err)
		}
	}
	var seen []string
	if err := walkResultExpression("needs.a.outputs.x or needs.b.outputs.x and 'needs.quoted.outputs.x or text'", func(parts []string) error {
		if parts[0] == "needs" {
			seen = append(seen, parts[1])
		}
		return nil
	}); err != nil || strings.Join(seen, ",") != "a,b" {
		t.Fatal(seen, err)
	}
}

func TestNumericWholeExpressionsRoundTripAndStrictResolution(t *testing.T) {
	source := `jobs:
  repeat:
    uses: workflows/child.yml
    repeat: {max-attempts: '${{ inputs.bound }}', until: true}
  each:
    uses: workflows/child.yml
    for-each: {items: [], key: item.id, concurrency: '${{ inputs.bound }}'}
  native:
    steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work, turns: '${{ inputs.bound }}'}}]
`
	w, err := Parse([]byte(source))
	if err == nil {
		err = Validate(w)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "${{ inputs.bound }}") != 3 {
		t.Fatal(string(b))
	}
	data, _ := json.Marshal(w)
	var saved Workflow
	if err := json.Unmarshal(data, &saved); err != nil || saved.Jobs["repeat"].Repeat.MaxAttemptsExpression != "${{ inputs.bound }}" {
		t.Fatal(string(data), err)
	}
	for _, value := range []any{nil, true, "3", json.Number("1.1"), json.Number("0"), json.Number("101"), json.Number("1e1000000")} {
		if _, err := resolveInteger("${{ inputs.bound }}", expressionContext{Inputs: map[string]any{"bound": value}}, 1, 100); err == nil {
			t.Fatalf("accepted invalid resolved bound %#v", value)
		}
	}
	for _, value := range []any{3, json.Number("3.0"), json.Number("3e1")} {
		if _, err := resolveInteger("${{ inputs.bound }}", expressionContext{Inputs: map[string]any{"bound": value}}, 1, 100); err != nil {
			t.Fatal(value, err)
		}
	}
}

func TestNumericExpressionBlocksRejectNonMappingYAML(t *testing.T) {
	for field, values := range map[string][]string{
		"repeat":   {"[]", "true", "2", "text", "[max-attempts, 2, until, true]", "[max-attempts, 1.5, until, true]", "[max-attempts, 2, until, true, dangling]", "[max-attempts, 2, max-attempts, 3, until, true]", "[max-attempts, 2, until, true, extra, ignored]"},
		"for-each": {"[]", "true", "2", "text", "[items, [{id: a}, {id: b}], key, item.id, concurrency, 1.5]", "[items, [], key, item.id]", "[items, [], key, item.id, dangling]", "[items, [], key, item.id, key, item.other]", "[items, [], key, item.id, extra, ignored]"},
	} {
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				source := fmt.Sprintf("jobs: {loop: {uses: workflows/child.yml, %s: %s}}", field, value)
				if _, err := Parse([]byte(source)); err == nil {
					t.Fatal("accepted malformed enclosing block", source)
				}
			})
		}
	}
}

func TestNumericExpressionBlocksPreserveStrictMappingChecks(t *testing.T) {
	for field, values := range map[string][]string{
		"repeat":   {"{max-attempts: 2, max-attempts: 3, until: true}", "{max-attempts: 2, until: true, extra: ignored}"},
		"for-each": {"{items: [], key: item.id, key: item.other}", "{items: [], key: item.id, extra: ignored}", "{items: [], key: item.id, concurrency: 1.5}", "{items: [], key: item.id, concurrency: null}", "{items: [], key: item.id, concurrency: true}", "{items: [], key: item.id, concurrency: '2'}"},
	} {
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				if _, err := Parse([]byte(fmt.Sprintf("jobs: {loop: {uses: workflows/child.yml, %s: %s}}", field, value))); err == nil {
					t.Fatal("mapping strictness changed", field, value)
				}
			})
		}
	}
	for field, value := range map[string]string{"repeat": "{max-attempts: 2, until: true}", "for-each": "{items: [], key: item.id}"} {
		source := fmt.Sprintf("jobs:\n  first:\n    uses: workflows/child.yml\n    %s: &selected %s\n  second:\n    uses: workflows/child.yml\n    %s: *selected\n", field, value, field)
		if _, err := Parse([]byte(source)); err == nil || !strings.Contains(err.Error(), "aliases") {
			t.Fatal("mapping alias accepted", field, err)
		}
	}
}

func TestNumericExpressionBlocksPreserveMappingAndNullCompatibility(t *testing.T) {
	for field, values := range map[string][]string{
		"repeat":   {"null", "{max-attempts: 2, until: true}", "{max-attempts: 1.5, until: true}", "{max-attempts: '${{ inputs.bound }}', until: true}"},
		"for-each": {"null", "{items: [], key: item.id}", "{items: [], key: item.id, concurrency: 2}", "{items: [], key: item.id, concurrency: '${{ inputs.bound }}'}"},
	} {
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				workflow, err := Parse([]byte(fmt.Sprintf("jobs: {loop: {uses: workflows/child.yml, %s: %s}}", field, value)))
				if err == nil {
					err = Validate(workflow)
				}
				if err != nil {
					t.Fatal("mapping/null compatibility changed", err)
				}
				if value == "null" && (workflow.Jobs["loop"].Repeat != nil || workflow.Jobs["loop"].ForEach != nil) {
					t.Fatal("null block allocated a controller")
				}
			})
		}
	}
	workflow, err := Parse([]byte("jobs: {loop: {uses: workflows/child.yml}}"))
	if err == nil {
		err = Validate(workflow)
	}
	if err != nil || workflow.Jobs["loop"].Repeat != nil || workflow.Jobs["loop"].ForEach != nil {
		t.Fatal("omitted controllers changed", workflow, err)
	}
}

func TestExpressionRepeatPinsResolvedBoundAndValidatesCompletedRecovery(t *testing.T) {
	e, s := durableExecutor(t, "jobs: {noop: {steps: [{argv: [true]}]}}", nil)
	writeWorkflow(t, e.WorkspaceDir, "limit.yml", `on: {workflow_call: {inputs: {bound: {type: number}}}}
jobs:
  loop:
    repeat: {max-attempts: '${{ inputs.bound }}', until: '${{ iteration.number == inputs.bound }}'}
    steps: [{argv: [true]}]
`)
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/limit.yml", Inputs: map[string]any{"bound": 2}})
	if err != nil || len(run.Jobs["loop"].Iterations) != 2 {
		t.Fatal(run, err)
	}
	p, err := s.IterationPlan(t.Context(), run.ID, "$repeat/loop")
	if err != nil || p.ResolvedBound != 2 {
		t.Fatal(p, err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	p.ResolvedBound = 3
	b, _ := json.Marshal(p)
	if _, err = s.db.ExecContext(t.Context(), "UPDATE iteration_plans SET record=? WHERE run_id=? AND job_id=?", b, run.ID, "$repeat/loop"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil || !strings.Contains(err.Error(), "repeat bound changed") {
		t.Fatal("changed bound accepted", err)
	}
	if _, err = s.db.ExecContext(t.Context(), "DELETE FROM iteration_plans WHERE run_id=? AND job_id=?", run.ID, "$repeat/loop"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil || !strings.Contains(err.Error(), "repeat bound missing") {
		t.Fatal("missing bound accepted", err)
	}
}

func TestExpressionRepeatPreservesJobAdmissionBeforeResolvingBound(t *testing.T) {
	for _, test := range []struct {
		name, dependency, condition, status string
		canceled, effectCanceled            bool
	}{
		{name: "failed dependency", dependency: RunStatusFailed, status: RunStatusSkipped},
		{name: "skipped dependency", dependency: RunStatusSkipped, status: RunStatusSkipped},
		{name: "attention dependency despite condition", dependency: RunStatusNeedsAttention, condition: "true", status: RunStatusSkipped},
		{name: "disabled", condition: "false", status: RunStatusSkipped},
		{name: "canceled", canceled: true, status: RunStatusCanceled},
		{name: "effect cancellation", effectCanceled: true, status: RunStatusWaiting},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, expression := range []bool{false, true} {
				e, store := durableExecutor(t, "jobs: {prepare: {steps: [{argv: [true]}]}}", nil)
				job := Job{If: test.condition, Repeat: &Repeat{MaxAttempts: 1, Until: "true"}}
				if expression {
					// This missing output is deliberately invalid if the job is admitted.
					job.Repeat.MaxAttemptsExpression = "${{ needs.prepare.outputs.cycles }}"
				}
				needs := map[string]JobExecution{}
				if test.dependency != "" {
					needs["prepare"] = JobExecution{Status: test.dependency}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if test.canceled {
					cancel()
				}
				req := RunRequest{Ref: "workflows/main.yml"}
				if test.effectCanceled {
					run, err := e.Prepare(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.RequestCancelGeneration(t.Context(), run.ID, run.Generation); err != nil {
						t.Fatal(err)
					}
					req.runID = run.ID
				}
				all := map[string]*Workflow{"workflows/main.yml": {Jobs: map[string]Job{"prepare": {}}}}
				got := e.executeJob(ctx, "loop", job, nil, req, needs, all, make(chan struct{}, 1), 0)
				if got.Status != test.status || len(got.Steps) != 0 || got.ChildRunID != "" {
					t.Fatalf("expression=%v: admission result %+v, want %s without dispatch", expression, got, test.status)
				}
			}
		})
	}
}

func TestExpressionRepeatAdmitsExplicitAndContinuedFailures(t *testing.T) {
	for _, test := range []struct{ name, options string }{
		{"explicit condition", "    if: true\n"},
		{"continued dependency", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			continued := test.name == "continued dependency"
			source := fmt.Sprintf(`on: {workflow_call: {inputs: {bound: {type: number}}}}
jobs:
  prepare:
    continue-on-error: %t
    steps: [{argv: [false]}]
  loop:
    needs: [prepare]
%s    repeat: {max-attempts: '${{ inputs.bound }}', until: true}
    steps: [{argv: [true]}]
`, continued, test.options)
			e, s := durableExecutor(t, source, nil)
			run, _ := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"bound": 2}})
			if run == nil || run.Jobs["loop"].Status != RunStatusSucceeded || len(run.Jobs["loop"].Iterations) != 1 {
				t.Fatalf("admitted repeat did not execute: %+v", run)
			}
			pinned, err := s.IterationPlan(t.Context(), run.ID, "$repeat/loop")
			if err != nil || pinned.ResolvedBound != 2 {
				t.Fatal(pinned, err)
			}
		})
	}
}

func TestCustomDefinitionsRootRetainsNestedWorkflowDirectory(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"automation", "workflows"} {
		path := filepath.Join(root, directory, "team", "workflows")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "main.yml"), []byte("name: "+directory+"\njobs: {main: {steps: [{argv: [true]}]}}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, DefinitionsDir: "automation"})
	const ref = "workflows/team/workflows/main.yml"
	definitions, err := e.Load(ref)
	if err != nil || definitions[ref] == nil || definitions[ref].Name != "automation" {
		t.Fatalf("custom definitions root lost to workspace decoy: %+v %v", definitions, err)
	}
	discovered, err := e.Discover()
	if err != nil || len(discovered) != 1 || discovered[0].Ref != ref || discovered[0].Name != "automation" || discovered[0].Error != "" {
		t.Fatalf("discovery changed custom-root interpretation: %+v %v", discovered, err)
	}
}

func TestInheritedImagePinsConfiguredPolicyAndRejectsMissingConfiguration(t *testing.T) {
	e, _, _ := containerExecutor(t)
	source := `jobs: {job: {container: {}, steps: [{argv: [true]}]}}`
	definition, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(restored); err != nil {
		t.Fatal("inherited image did not round-trip", err)
	}
	writeWorkflow(t, e.WorkspaceDir, "inherit.yml", source)
	if _, err := e.Run(t.Context(), RunRequest{Ref: "workflows/inherit.yml"}); err == nil || !strings.Contains(err.Error(), "configured container execution image") {
		t.Fatal(err)
	}
	e.CommandExecution = runtimeexec.Policy{Mode: "container", Image: "fixture/tools", Runtime: "docker", NetworkMode: "bridge", Writable: true}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/inherit.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := e.Store.(Journal).Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	impl := snap.Definitions["workflows/inherit.yml"].Jobs["job"].Container.Implementation
	if impl == nil || impl.Image.ID == "" || impl.Policy.Image != impl.Image.ID {
		t.Fatal(impl)
	}
}

func TestAdmittedFingerprintExcludesBundleOwnershipAndBindsConfiguration(t *testing.T) {
	impl := &ContainerImplementation{Bundle: &HelperBundle{OriginID: "first", ID: "content"}}
	w := &Workflow{Jobs: map[string]Job{"helper": {Container: &JobContainer{Implementation: impl}, Steps: []Step{{Argv: []string{"true"}}}}}}
	all := map[string]*Workflow{"workflows/main.yml": w}
	original := admittedFingerprint(all, json.RawMessage(`{"model":"first"}`), runtimeexec.Policy{})
	impl.Bundle.OriginID = "second"
	if got := admittedFingerprint(all, json.RawMessage(`{"model":"first"}`), runtimeexec.Policy{}); got != original {
		t.Fatal("ownership changed identity")
	}
	if got := admittedFingerprint(all, json.RawMessage(`{"model":"second"}`), runtimeexec.Policy{}); got == original {
		t.Fatal("configuration not bound")
	}
	impl.Bundle.ID = "changed"
	if got := admittedFingerprint(all, json.RawMessage(`{"model":"first"}`), runtimeexec.Policy{}); got == original {
		t.Fatal("helper not bound")
	}
	w.Jobs["caller"] = Job{Uses: "workflows/child.yml", With: map[string]any{"exact": json.Number("9007199254740992")}}
	precise := admittedFingerprint(all, nil, runtimeexec.Policy{})
	w.Jobs["caller"].With["exact"] = json.Number("9007199254740993")
	if admittedFingerprint(all, nil, runtimeexec.Policy{}) == precise {
		t.Fatal("authored integer precision lost in execution fingerprint")
	}
}

func TestExpressionAgentTurnsResolveBeforeDispatchAndRemainRetained(t *testing.T) {
	for _, bound := range []any{5, json.Number("5"), nil, true, "", "5", 0, 1.5, 2147483648} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) {
			// Input type is deliberately unrestricted here, to test the dispatch boundary.
			e, store, fake := agentTaskFixture(t, `jobs: {native: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work, turns: '${{ inputs.bound }}'}}]}}
`, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "done"}, nil })
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"bound": bound}})
			if bound != 5 && bound != json.Number("5") {
				if err == nil || len(fake.requests) != 0 {
					t.Fatal("invalid allowance dispatched", run, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			workers, err := e.Workers.List(t.Context(), run.ID)
			if err != nil || len(workers) != 1 || workers[0].TaskTurns != 5 {
				t.Fatal(workers, err)
			}
			attempts, err := store.Attempts(t.Context(), run.ID)
			if err != nil || len(attempts) != 1 || attempts[0].AgentTask.Input.Turns != 5 {
				t.Fatal(attempts, err)
			}
		})
	}
}

func TestExpressionAgentTurnsRejectMissingBoundsAndPreserveLiteralDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, input, needs, turns string
		valid                     bool
	}{
		{name: "expression-null", turns: ", turns: '${{ null }}'"},
		{name: "optional-number-missing", input: "on: {workflow_call: {inputs: {bound: {type: number}}}}\n", turns: ", turns: '${{ inputs.bound }}'"},
		{name: "input-missing", turns: ", turns: '${{ inputs.bound }}'"},
		{name: "output-missing", needs: "  prepare: {steps: [{argv: [true]}]}\n", turns: ", turns: '${{ needs.prepare.outputs.missing.bound }}'"},
		{name: "omitted", valid: true},
		{name: "literal-null", turns: ", turns: null", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := ""
			if tc.needs != "" {
				needs = "needs: [prepare], "
			}
			e, store, fake := agentTaskFixture(t, tc.input+"jobs:\n"+tc.needs+"  native: {"+needs+"steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work"+tc.turns+"}}]}\n", func(SessionRequest) (map[string]any, error) {
				return map[string]any{"text": "done"}, nil
			})
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml"})
			workers, listErr := e.Workers.List(t.Context(), run.ID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if !tc.valid {
				if err == nil || len(fake.requests) != 0 || len(workers) != 0 {
					t.Fatal("invalid expression allowance dispatched", run, err, workers)
				}
				attempts, loadErr := store.Attempts(t.Context(), run.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				for _, attempt := range attempts {
					if attempt.AgentTask != nil {
						t.Fatal("invalid allowance retained a dispatch intent", attempt)
					}
				}
				return
			}
			if err != nil || len(fake.requests) != 1 || len(workers) != 1 || workers[0].TaskTurns != e.Workers.Limits.MaxTurns {
				t.Fatal("legacy default changed", run, err, workers)
			}
		})
	}
}

func TestExpressionConcurrencyPinsResolvedBoundBeforeItems(t *testing.T) {
	e, s := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": "done"}, nil
	}))
	source := strings.Replace(iterationSource, "      key: item.id", "      key: item.id\n      concurrency: '${{ inputs.bound }}'", 1)
	writeWorkflow(t, e.WorkspaceDir, "main.yml", source)
	req := iterationRequest("first", "second")
	req.Inputs["bound"] = 2
	run, err := e.Run(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.IterationPlan(t.Context(), run.ID, "review")
	if err != nil || plan.ResolvedBound != 2 {
		t.Fatal(plan, err)
	}
	req.Inputs["bound"] = 1.5
	if _, err = e.Run(t.Context(), req); err == nil {
		t.Fatal("fractional concurrency accepted")
	}
}

func TestNestedExampleReferencesKeepWorkspaceAndDefinitionFences(t *testing.T) {
	root := t.TempDir()
	ref := "examples/pr-fix/workflows/main.yml"
	target := filepath.Join(root, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("jobs: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := (Resolver{WorkspaceDir: root}).ResolveLocal(ref)
	if err != nil || resolved.Canonical != ref || resolved.Path != target {
		t.Fatal(resolved, err)
	}
	for _, unsafe := range []string{"examples/../pr-fix/workflows/main.yml", "examples/pr-fix/workflows/../main.yml", "examples/pr-fix/main.yml"} {
		if _, err := (Resolver{WorkspaceDir: root}).ResolveLocal(unsafe); err == nil {
			t.Fatal("accepted", unsafe)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escaped")); err != nil {
		t.Skip(err)
	}
	if _, err := (Resolver{WorkspaceDir: root}).ResolveLocal("escaped/pr-fix/workflows/main.yml"); err == nil {
		t.Fatal("nested definition root escaped workspace")
	}
}

func TestCanceledReusableRecoveryAllowsOnlyUnadmittedChildren(t *testing.T) {
	e, _ := durableExecutor(t, "jobs: {noop: {steps: [{argv: [true]}]}}", nil)
	main, err := Parse([]byte("jobs: {child: {uses: workflows/child.yml}}"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := Parse([]byte("jobs: {noop: {repeat: {max-attempts: '${{ inputs.bound }}', until: true}, steps: [{argv: [true]}]}}"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Definitions: map[string]*Workflow{"workflows/main.yml": main, "workflows/child.yml": child}, Hashes: map[string]string{}}
	for ref, definition := range snapshot.Definitions {
		snapshot.Hashes[ref] = actionHash(definition)
	}
	run := &Run{ID: "fixture-parent", WorkflowRef: "workflows/main.yml", Jobs: map[string]JobExecution{
		"child": {ID: "child", Status: RunStatusInterrupted, ChildRunID: "admitted-child", Iterations: []JobIteration{{Number: 1, Status: RunStatusInterrupted}}},
	}}
	if err := e.validateIterationRecovery(t.Context(), run, snapshot, nil); err == nil {
		t.Fatal("missing admitted child accepted")
	}
	result := run.Jobs["child"]
	result.ChildRunID = ""
	run.Jobs["child"] = result
	if err := e.validateIterationRecovery(t.Context(), run, snapshot, nil); err != nil {
		t.Fatal("unadmitted cancellation blocked", err)
	}
	for _, variant := range []string{"terminal", "admitted", "steps", "earlier", "status", "child-jobs", "outputs", "iteration-outputs", "parent-steps"} {
		t.Run("failed/"+variant, func(t *testing.T) {
			result := JobExecution{ID: "child", Status: RunStatusFailed, Iterations: []JobIteration{{Number: 1, Status: RunStatusFailed}}}
			switch variant {
			case "admitted":
				result.ChildRunID = "admitted-child"
			case "steps":
				result.Iterations[0].Steps = map[string]StepExecution{"effect": {Status: RunStatusSucceeded}}
			case "earlier":
				result.Iterations = append(result.Iterations, JobIteration{Number: 2, Status: RunStatusSkipped})
			case "status":
				result.Status = RunStatusSucceeded
			case "child-jobs":
				result.ChildJobs = map[string]JobExecution{"admitted": {Status: RunStatusSucceeded}}
			case "outputs":
				result.Outputs = map[string]any{"value": "admitted"}
			case "iteration-outputs":
				result.Iterations[0].Outputs = map[string]any{"value": "admitted"}
			case "parent-steps":
				result.Steps = map[string]StepExecution{"effect": {Status: RunStatusSucceeded}}
			}
			run.Jobs["child"] = result
			err := e.validateIterationRecovery(t.Context(), run, snapshot, nil)
			if (variant == "terminal") != (err == nil) {
				t.Fatal(variant, err)
			}
		})
	}
}

func TestIntegerControlNumericAtomsKeepExactPrecision(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		atom string
		high int
		want int
	}{
		{"1.0000000000000001", 100, 0},
		{"0x1.0000000000000001p0", 100, 0},
		{"100.000000000000001", 100, 0},
		{"2147483647.0000001", 2147483647, 0},
		{"3", 100, 3}, {"3.0", 100, 3}, {"3e1", 100, 30},
		{"+3", 100, 3}, {"03", 100, 3}, {"3.", 100, 3},
		{fmt.Sprint(maxInt), maxInt, maxInt},
		{fmt.Sprint(uint64(maxInt) + 1), maxInt, 0},
		{"'3'", 100, 0}, {"true", 100, 0}, {"null", 100, 0}, {"1/1", 100, 0},
	} {
		t.Run(tc.atom, func(t *testing.T) {
			got, err := resolveInteger("${{ "+tc.atom+" }}", expressionContext{}, 1, tc.high)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("accepted noninteger or invalid atom %s as %d", tc.atom, got)
				}
			} else if err != nil || got != tc.want {
				t.Fatal(got, err, tc.want)
			}
		})
	}
	for _, value := range []json.Number{json.Number(fmt.Sprint(maxInt)), "3.0", "3e1"} {
		if _, err := resolveInteger("${{ inputs.bound }}", expressionContext{Inputs: map[string]any{"bound": value}}, 1, maxInt); err != nil {
			t.Fatal(value, err)
		}
	}
	// General DSL evaluation retains its established floating-point semantics.
	if value, err := renderString("${{ 1.0000000000000001 }}", expressionContext{}); err != nil || value != float64(1) {
		t.Fatal(value, err)
	}
	if value, err := renderString("${{ 0x3p0 }}", expressionContext{}); err != nil || value != float64(3) {
		t.Fatal(value, err)
	}
	if value, err := resolveInteger("${{ inputs.bound }}", expressionContext{Inputs: map[string]any{"bound": float64(3)}}, 1, 100); err != nil || value != 3 {
		t.Fatal(value, err)
	}
	for literal, want := range map[string]int{"0x10": 16, "1_0": 10} {
		source := fmt.Sprintf("jobs: {repeat: {uses: workflows/child.yml, repeat: {max-attempts: %s, until: true}}, each: {uses: workflows/child.yml, for-each: {items: [], key: item.id, concurrency: %s}}}", literal, literal)
		w, err := Parse([]byte(source))
		if err != nil {
			t.Fatal(literal, err)
		}
		if err := Validate(w); err != nil || w.Jobs["repeat"].Repeat.MaxAttempts != want || *w.Jobs["each"].ForEach.Concurrency != want {
			t.Fatal("legacy literal decoding changed", literal, w, err)
		}
	}
}

func TestExpressionRepeatRecoveryRetriesFailedUnadmittedCall(t *testing.T) {
	for _, bound := range []string{"2", "'${{ 2 }}'"} {
		t.Run(bound, func(t *testing.T) {
			source := fmt.Sprintf(`on: {workflow_call: {secrets: {token: {required: false}}}}
jobs: {child: {uses: workflows/child.yml, secrets: {token: '${{ secrets.token }}'}, repeat: {max-attempts: %s, until: true}}}
`, bound)
			e, store := durableExecutor(t, source, nil)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {secrets: {token: {required: true}}}}
jobs: {read: {steps: [{uses: action/capture, with: {value: observed}}]}}
`)
			var calls atomic.Int32
			registerTestAction(t, e, readSpec(&calls))
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || !strings.Contains(err.Error(), "secret token must be a string") {
				t.Fatal(run, err)
			}
			result := run.Jobs["child"]
			if calls.Load() != 0 || result.Status != RunStatusFailed || result.ChildRunID != "" || len(result.Iterations) != 1 || len(result.Iterations[0].Steps) != 0 {
				t.Fatal("not a proven pre-admission failure", result, calls.Load())
			}
			if _, _, err := store.Load(t.Context(), reusableChildID(run.ID, "child", 1)); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("unexpected child admission", err)
			}
			if strings.Contains(bound, "${{") {
				plan, err := store.IterationPlan(t.Context(), run.ID, "$repeat/child")
				if err != nil || plan.ResolvedBound != 2 {
					t.Fatal("lost admitted bound", plan, err)
				}
			}
			run, err = e.Resume(t.Context(), run.ID, map[string]string{"token": "restored"}, false)
			if err != nil || run.Status != RunStatusSucceeded || calls.Load() != 1 || run.Jobs["child"].ChildRunID == "" {
				t.Fatal("unadmitted failure did not retry", run, err, calls.Load())
			}
		})
	}
}

func TestIntegerControlFractionalAtomsRejectBeforeDispatch(t *testing.T) {
	for name, source := range map[string]string{
		"repeat":      `jobs: {call: {uses: workflows/child.yml, repeat: {max-attempts: '${{ 100.000000000000001 }}', until: true}}}`,
		"concurrency": `jobs: {call: {uses: workflows/child.yml, for-each: {items: [{id: a}], key: item.id, concurrency: '${{ 1.0000000000000001 }}'}}}`,
		"turns":       `jobs: {native: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work, turns: '${{ 1.0000000000000001 }}'}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, _, fake := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "done"}, nil })
			writeWorkflow(t, e.WorkspaceDir, "child.yml", `on: {workflow_call: {}}
jobs: {native: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work}}]}}
`)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run == nil || len(fake.requests) != 0 {
				t.Fatal("fractional atom dispatched", run, err)
			}
			workers, err := e.Workers.List(t.Context(), run.ID)
			if err != nil || len(workers) != 0 {
				t.Fatal("fractional atom admitted a worker", workers, err)
			}
		})
	}
}
