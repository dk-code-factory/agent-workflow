package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConciseResultPathsAndCollisions(t *testing.T) {
	step := StepExecution{ID: "review", Status: RunStatusSucceeded, Outputs: map[string]any{
		"text": "raw reply", "structured": map[string]any{
			"approved": true, "nested": map[string]any{"count": json.Number("3")},
			"outputs": "schema outputs", "status": "schema status", "error": "schema error", "text": "schema text",
		},
	}}
	job := JobExecution{Status: RunStatusSucceeded, Steps: map[string]StepExecution{"review": step}}
	ctx := expressionContext{Steps: job.Steps, Needs: map[string]JobExecution{"build": job}, Jobs: map[string]JobExecution{"build": job}}
	for path, want := range map[string]any{
		"steps.review.approved":                              true,
		"steps.review.nested.count":                          json.Number("3"),
		"steps.review.text":                                  "raw reply",
		"steps.review.status":                                RunStatusSucceeded,
		"steps.review.error":                                 "",
		"steps.review.outputs.structured.outputs":            "schema outputs",
		"steps.review.outputs.structured.status":             "schema status",
		"steps.review.outputs.structured.error":              "schema error",
		"steps.review.outputs.structured.text":               "schema text",
		"needs.build.steps.review.approved":                  true,
		"needs.build.steps.review.outputs.structured.status": "schema status",
		"jobs.build.steps.review.text":                       "raw reply",
	} {
		t.Run(path, func(t *testing.T) {
			got, err := lookupPath(path, ctx)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, want)
			}
		})
	}
	// Existing whole-result expressions preserve their exact metadata shape.
	got, err := lookupPath("steps.review", ctx)
	want := map[string]any{"outputs": step.Outputs, "status": step.Status, "error": step.Error}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy object changed: %#v, %v", got, err)
	}
	cloned, err := renderString("${{ steps.review.nested }}", ctx)
	if err != nil {
		t.Fatal(err)
	}
	cloned.(map[string]any)["count"] = json.Number("999")
	if original, _ := lookupPath("steps.review.nested.count", ctx); original != json.Number("3") {
		t.Fatal("alias mutation changed the retained result")
	}
	for _, path := range []string{"steps.review.missing", "steps.review.nested.missing", "steps.review.nested.count.x", "needs.other.steps.review.text", "needs.build.steps.missing.text", "needs.build.steps.review.missing", "jobs.build.steps"} {
		if _, err := lookupPath(path, ctx); err == nil {
			t.Errorf("accepted unavailable result %s", path)
		}
	}
	if got, err := lookupPath("steps.review.outputs.missing", ctx); err != nil || got != nil {
		t.Fatalf("legacy missing outputs changed: %#v, %v", got, err)
	}
	for _, status := range []string{RunStatusSkipped, RunStatusFailed, RunStatusCanceled, RunStatusRunning} {
		step.Status = status
		ctx.Steps["review"] = step
		if _, err := lookupPath("steps.review.approved", ctx); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("exposed %s result: %v", status, err)
		}
		if got, err := lookupPath("steps.review.status", ctx); err != nil || got != status {
			t.Fatalf("lost legacy metadata: %#v %v", got, err)
		}
	}
	ctx.Steps["review"] = StepExecution{ID: "review", Status: RunStatusSucceeded}
	if _, err := lookupPath("steps.review.text", ctx); err == nil {
		t.Fatal("accepted absent text result")
	}
}

func TestConciseReferenceAdmissionScope(t *testing.T) {
	const source = `on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.producer.steps.review.text }}'}
jobs:
  producer:
    outputs: {alias: '${{ steps.review.text }}'}
    repeat: {max-attempts: 2, until: steps.review.approved}
    steps:
      - id: review
        agent: reviewer
        prompt: Review
      - id: use
        agent: consumer
        prompt: '${{ steps.review.text }}'
  consumer:
    needs: [producer]
    steps:
      - id: use
        agent: consumer
        prompt: '${{ needs.producer.steps.review.approved }}'
`
	for _, test := range []struct{ name, old, replacement, diagnostic string }{
		{"valid", "", "", ""},
		{"forward", "prompt: Review", "prompt: '${{ steps.use.text }}'", "unavailable"},
		{"forward unicode field", "prompt: Review", "prompt: '${{ steps.use.é }}'", "unavailable"},
		{"forward symbol field", "prompt: Review", "prompt: '${{ steps.use.$value }}'", "unavailable"},
		{"forward numeric field", "prompt: Review", "prompt: '${{ steps.use.0 }}'", "unavailable"},
		{"forward spaced field", "prompt: Review", "prompt: '${{ steps.use.with space }}'", "unavailable"},
		{"forward metadata prefix", "prompt: Review", "prompt: '${{ steps.use.outputsé }}'", "unavailable"},
		{"forward comparison operand", "prompt: Review", "prompt: '${{ \"literal\" == steps.use.$value == \"literal\" }}'", "unavailable"},
		{"forward logical operand", "prompt: Review", "prompt: '${{ true and not steps.use.é }}'", "unavailable"},
		{"unicode dependency field", "needs.producer.steps.review.approved", "needs.producer.steps.review.é", ""},
		{"symbol dependency field", "needs.producer.steps.review.approved", "needs.producer.steps.review.$value", ""},
		{"numeric dependency field", "needs.producer.steps.review.approved", "needs.producer.steps.review.0", ""},
		{"spaced dependency field", "needs.producer.steps.review.approved", "needs.producer.steps.review.with space", ""},
		{"self", "prompt: Review", "prompt: '${{ steps.review.text }}'", "unavailable"},
		{"missing", "steps.review.text", "steps.absent.text", "producer step not found"},
		{"undeclared", "needs: [producer]", "needs: []", "not a declared needs dependency"},
		{"wrong root", "needs.producer.steps.review.approved", "jobs.producer.steps.review.approved", "only available in workflow_call outputs"},
		{"workflow local", "jobs.producer.steps.review.text", "steps.review.text", "local step results are unavailable"},
		{"workflow needs", "jobs.producer.steps.review.text", "needs.producer.steps.review.text", "not a declared needs dependency"},
		{"unknown workflow producer", "jobs.producer.steps.review.text", "jobs.absent.steps.review.text", "producer job not found"},
		{"incomplete", "needs.producer.steps.review.approved", "needs.producer.steps.review", "step id and result field"},
		{"quoted", "prompt: Review", "prompt: '${{ \"steps.missing.text\" }}'", ""},
		{"quoted unicode", "prompt: Review", "prompt: '${{ \"steps.missing.é\" }}'", ""},
		{"quoted comparison operands", "prompt: Review", "prompt: '${{ \"steps.missing.é == steps.use.$value\" }}'", "unavailable"},
		{"data path", "prompt: Review", "prompt: '${{ inputs.config.steps.missing.$value }}'", ""},
		{"literal", "prompt: Review", "prompt: steps.missing.text", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := source
			if test.old != "" {
				text = strings.Replace(text, test.old, test.replacement, 1)
			}
			w, err := Parse([]byte(text))
			if err != nil {
				t.Fatal(err)
			}
			err = Validate(w)
			if test.diagnostic == "" && err != nil || test.diagnostic != "" && (err == nil || !strings.Contains(err.Error(), test.diagnostic)) {
				t.Fatalf("got %v; want diagnostic %q", err, test.diagnostic)
			}
		})
	}
	for _, producer := range []string{"uses: workflows/child.yml", "uses: workflows/child.yml\n    for-each: {items: ['${{ steps.missing.text }}'], key: item}"} {
		w, err := Parse([]byte("jobs:\n  producer:\n    " + producer + "\n  consumer:\n    needs: [producer]\n    steps:\n      - agent: consumer\n        prompt: '${{ needs.producer.steps.private.text }}'\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err = Validate(w); err == nil || !strings.Contains(err.Error(), "steps are private") {
			t.Fatalf("exposed reusable internals: %v", err)
		}
	}
	w, err := Parse([]byte("jobs:\n  producer:\n    uses: workflows/child.yml\n    for-each: {items: ['${{ steps.missing.text }}'], key: item}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(w); err != nil {
		t.Fatalf("literal item data was interpreted as a reference: %v", err)
	}
}

func TestConciseResultsPreserveLegacyEmptyPathComponents(t *testing.T) {
	step := StepExecution{ID: "review", Status: RunStatusSkipped, Outputs: map[string]any{
		"text": "legacy reply", "structured": map[string]any{
			"": map[string]any{"outputs": map[string]any{"text": "different"}},
		},
	}}
	ctx := expressionContext{Steps: map[string]StepExecution{"review": step}}
	metadata := map[string]any{"outputs": step.Outputs, "status": step.Status, "error": step.Error}
	for path, want := range map[string]any{
		"steps.review..outputs.text": "legacy reply",
		"steps.review...status":      RunStatusSkipped,
		"steps.review.":              metadata,
		"steps.review..":             metadata,
		"steps.review..text":         nil,
	} {
		got, err := lookupPath(path, ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("legacy path %s: got %#v, %v; want %#v", path, got, err, want)
		}
		if newResultReference(strings.Split(path, ".")) {
			t.Errorf("legacy path %s acquired concise admission rules", path)
		}
	}
	for _, condition := range []string{"steps.later..status", "false and steps.later.outputs.text", "false and steps.later..outputs.text"} {
		w := &Workflow{Jobs: map[string]Job{"main": {
			If: condition, Steps: []Step{{ID: "later", Argv: []string{"echo", "ready"}}},
		}}}
		if err := Validate(w); err != nil {
			t.Fatalf("legacy metadata reference %q rejected: %v", condition, err)
		}
	}
}

func TestConciseReferencesInNamedSchemas(t *testing.T) {
	for _, test := range []struct{ name, expression, diagnostic string }{
		{"preceding", "steps.first.text", ""},
		{"self", "steps.consumer.text", "unavailable"},
		{"forward", "steps.last.text", "unavailable"},
		{"missing", "steps.absent.text", "producer step not found"},
		{"declared dependency", "needs.producer.steps.result.text", ""},
		{"undeclared dependency", "needs.other.steps.result.text", "not a declared needs dependency"},
		{"workflow root", "jobs.producer.steps.result.text", "only available in workflow_call outputs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := &Workflow{Schemas: map[string]map[string]any{"Result": {"type": "string", "enum": []any{"${{ " + test.expression + " }}"}}}, Jobs: map[string]Job{
				"producer": {Steps: []Step{{ID: "result", Uses: "agent/reviewer", With: map[string]any{"prompt": "Produce"}}}},
				"other":    {Steps: []Step{{ID: "result", Uses: "agent/reviewer", With: map[string]any{"prompt": "Produce"}}}},
				"main": {Needs: []string{"producer"}, Steps: []Step{
					{ID: "first", Uses: "agent/reviewer", With: map[string]any{"prompt": "Produce"}},
					{ID: "consumer", Uses: "agent/reviewer", With: map[string]any{"prompt": "Consume", "output": map[string]any{"schema": "Result"}}},
					{ID: "last", Uses: "agent/reviewer", With: map[string]any{"prompt": "Later"}},
				}},
			}}
			err := Validate(w)
			if test.diagnostic == "" && err != nil || test.diagnostic != "" && (err == nil || !strings.Contains(err.Error(), test.diagnostic)) {
				t.Fatalf("got %v; want diagnostic %q", err, test.diagnostic)
			}
			if w.Jobs["main"].Steps[1].With["output"].(map[string]any)["schema"] != "Result" {
				t.Fatal("admission mutated the authored named reference")
			}
		})
	}
}

func TestConciseResultsExecuteAcrossJobsAndResume(t *testing.T) {
	const source = `on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.consumer.steps.combine.text }}'}
jobs:
  producer:
    steps:
      - id: review
        agent: reviewer
        prompt: Review
        with:
          output:
            format: json
            schema:
              type: object
              required: [approved]
              properties: {approved: {type: boolean}}
      - id: local
        agent: consumer
        prompt: 'local=${{ steps.review.approved }}'
  consumer:
    needs: [producer]
    steps:
      - id: combine
        agent: consumer
        prompt: 'decision=${{ needs.producer.steps.review.approved }}, local=${{ needs.producer.steps.local.text }}'
`
	ctx := context.Background()
	calls := map[string]int{}
	e, store := durableExecutor(t, source, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		if req.Output != nil {
			req.Prompt = "Review"
		}
		calls[req.Prompt]++
		switch req.Prompt {
		case "Review":
			return map[string]any{"text": `{"approved":true}`}, nil
		case "local=true":
			return map[string]any{"text": "local result"}, nil
		case "decision=true, local=local result":
			if calls[req.Prompt] == 1 {
				return nil, errors.New("interrupted consumer")
			}
			return map[string]any{"text": "combined"}, nil
		default:
			return nil, errors.New("unexpected prompt: " + req.Prompt)
		}
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Jobs["producer"].Status != RunStatusSucceeded {
		t.Fatalf("expected consumer failure: %#v %v", run, err)
	}
	// Resume uses the admitted source and retained structured results after restart.
	writeWorkflow(t, e.WorkspaceDir, "main.yml", "jobs: {}")
	restarted := &Executor{WorkspaceDir: e.WorkspaceDir, Agents: e.Agents, Store: store}
	run, err = restarted.Resume(ctx, run.ID, nil, true)
	if err != nil || run.Outputs["answer"] != "combined" || calls["Review"] != 1 || calls["local=true"] != 1 || calls["decision=true, local=local result"] != 2 {
		t.Fatalf("resume lost result identity: run=%#v calls=%v err=%v", run, calls, err)
	}
}

func TestInvalidConciseReferenceHasNoProviderEffects(t *testing.T) {
	for name, expression := range map[string]string{
		"ordinary":        "${{ steps.later.text }}",
		"quoted braces":   `${{ "}}" == "}}" and steps.later.text }}`,
		"escaped operand": `'\' == steps.later.text'`,
	} {
		t.Run(name, func(t *testing.T) {
			definition := Workflow{Jobs: map[string]Job{"x": {Steps: []Step{
				{Uses: "agent/reviewer", With: map[string]any{"prompt": "before"}},
				{Uses: "agent/reviewer", If: expression, With: map[string]any{"prompt": "consume"}},
				{ID: "later", Uses: "agent/reviewer", With: map[string]any{"prompt": "later"}},
			}}}}
			source, err := json.Marshal(definition)
			if err != nil {
				t.Fatal(err)
			}
			e, _ := durableExecutor(t, string(source), agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				t.Error("invalid reference reached provider")
				return nil, errors.New("invalid reference reached provider")
			}))
			if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("bad admission error: %v", err)
			}
		})
	}
}

func TestConciseAdmissionMatchesConditionOperands(t *testing.T) {
	ctx := expressionContext{Steps: map[string]StepExecution{"review": {
		Status: RunStatusSucceeded, Outputs: map[string]any{"text": "ready", "structured": map[string]any{"text'": `\`}},
	}}}
	for _, test := range []struct{ expression, path string }{
		{`${{ "}}" == "}}" and steps.review.text }}`, "steps.review.text"},
		{`'\' == steps.review.text'`, "steps.review.text'"},
	} {
		if allowed, err := evalIf(test.expression, ctx); err != nil || !allowed {
			t.Fatalf("runner does not evaluate %q: %v, %v", test.expression, allowed, err)
		}
		var paths []string
		if err := walkResultReferences(test.expression, true, func(parts []string) error {
			if newResultReference(parts) {
				paths = append(paths, strings.Join(parts, "."))
			}
			return nil
		}); err != nil || !reflect.DeepEqual(paths, []string{test.path}) {
			t.Fatalf("lost runner operand in %q: %v, %v", test.expression, paths, err)
		}
	}
}

func TestLongConciseResultConditions(t *testing.T) {
	const helper = "WORKFLOW_LONG_RESULT_CONDITION_TEST"
	if os.Getenv(helper) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLongConciseResultConditions$")
		command.Env = append(os.Environ(), helper+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("bounded condition helper failed: %v: %s", err, output)
		}
		return
	}
	for _, prefix := range []string{"not ", "true and ", "true == ", "true != ", "true >= ", "true <= ", "true > ", "true < ", "'quoted and text' == "} {
		for _, suffix := range []string{"true", "steps.later.text"} {
			condition := "false and " + strings.Repeat(prefix, 100_000) + suffix
			w := &Workflow{Jobs: map[string]Job{"main": {If: condition, Steps: []Step{
				{ID: "later", Argv: []string{"echo", "skipped"}},
			}}}}
			err := Validate(w)
			if suffix == "true" && err != nil || suffix != "true" && (err == nil || !strings.Contains(err.Error(), "unavailable")) {
				t.Fatalf("long %q condition: %v", prefix, err)
			}
			if allowed, err := evalIf(condition, expressionContext{}); err != nil || allowed {
				t.Fatalf("lost runtime short circuit for %q: %v, %v", prefix, allowed, err)
			}
		}
	}
	steps := make([]Step, 1_000)
	for index := range steps {
		steps[index] = Step{Argv: []string{"echo", "skipped"}}
	}
	condition := "false and " + strings.Repeat("needs.p.steps.step_1000.status and ", 100_000) + "true"
	needs := make([]string, 1_000)
	jobs := make(map[string]Job, len(needs)+1)
	for index := range needs {
		needs[index] = fmt.Sprintf("dependency_%d", index)
		jobs[needs[index]] = Job{Steps: []Step{{Argv: []string{"echo", "skipped"}}}}
	}
	needs[len(needs)-1] = "p"
	jobs["p"] = Job{Steps: steps}
	jobs["consumer"] = Job{Needs: needs, If: condition, Steps: []Step{{Argv: []string{"echo", "skipped"}}}}
	if err := Validate(&Workflow{Jobs: jobs}); err != nil {
		t.Fatalf("reference-heavy condition: %v", err)
	}
	if allowed, err := evalIf(condition, expressionContext{}); err != nil || allowed {
		t.Fatalf("lost reference-heavy runtime short circuit: %v, %v", allowed, err)
	}
}

func TestConciseRepeatedResultsExposeFinalAttempt(t *testing.T) {
	const source = `on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.consumer.steps.use.text }}'}
jobs:
  producer:
    repeat: {max-attempts: 2, until: "steps.review.text == '2'"}
    steps:
      - id: review
        agent: reviewer
        prompt: 'attempt=${{ iteration.number }}'
  consumer:
    needs: [producer]
    steps:
      - id: use
        agent: consumer
        prompt: '${{ needs.producer.steps.review.text }}'
`
	var prompts []string
	e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, request AgentRequest) (map[string]any, error) {
		prompts = append(prompts, request.Prompt)
		return map[string]any{"text": strings.TrimPrefix(request.Prompt, "attempt=")}, nil
	}))
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Outputs["answer"] != "2" || !reflect.DeepEqual(prompts, []string{"attempt=1", "attempt=2", "2"}) {
		t.Fatalf("final repeated result lost: prompts=%v run=%#v err=%v", prompts, run, err)
	}
}

func TestUnavailableConciseResultsStopConsumer(t *testing.T) {
	for _, scenario := range []string{"skipped", "failed", "missing field"} {
		t.Run(scenario, func(t *testing.T) {
			const source = `jobs:
  producer:
    steps:
      - id: review
        agent: reviewer
        prompt: review
        continue-on-error: true
      - id: consumer
        agent: consumer
        prompt: 'decision=${{ steps.review.approved }}'
`
			definition := source
			if scenario == "skipped" {
				definition = strings.Replace(source, "prompt: review", "prompt: review\n        if: 'false'", 1)
			}
			calls := 0
			e, _ := durableExecutor(t, definition, agentFunc(func(_ context.Context, request AgentRequest) (map[string]any, error) {
				calls++
				if request.AgentID != "reviewer" {
					t.Error("consumer called with unavailable result")
					return nil, errors.New("consumer called with unavailable result")
				}
				if scenario == "failed" {
					return map[string]any{"structured": map[string]any{"approved": true}}, errors.New("provider failed")
				}
				return map[string]any{"text": "unstructured"}, nil
			}))
			_, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), "steps.review.approved") || (scenario == "skipped" && calls != 0) || (scenario != "skipped" && calls != 1) {
				t.Fatalf("unavailable result accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}
