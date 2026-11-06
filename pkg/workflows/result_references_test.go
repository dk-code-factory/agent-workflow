package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
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
		{"self", "prompt: Review", "prompt: '${{ steps.review.text }}'", "unavailable"},
		{"missing", "steps.review.text", "steps.absent.text", "producer step not found"},
		{"undeclared", "needs: [producer]", "needs: []", "not a declared needs dependency"},
		{"wrong root", "needs.producer.steps.review.approved", "jobs.producer.steps.review.approved", "only available in workflow_call outputs"},
		{"workflow local", "jobs.producer.steps.review.text", "steps.review.text", "local step results are unavailable"},
		{"workflow needs", "jobs.producer.steps.review.text", "needs.producer.steps.review.text", "not a declared needs dependency"},
		{"unknown workflow producer", "jobs.producer.steps.review.text", "jobs.absent.steps.review.text", "producer job not found"},
		{"incomplete", "needs.producer.steps.review.approved", "needs.producer.steps.review", "step id and result field"},
		{"quoted", "prompt: Review", "prompt: '${{ \"steps.missing.text\" }}'", ""},
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
	e, _ := durableExecutor(t, "jobs:\n  x:\n    steps:\n      - agent: reviewer\n        prompt: '${{ steps.later.text }}'\n      - id: later\n        agent: reviewer\n        prompt: later\n", agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		t.Error("invalid reference reached provider")
		return nil, errors.New("invalid reference reached provider")
	}))
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("bad admission error: %v", err)
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
