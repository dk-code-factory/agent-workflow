package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestConciseAgentNormalizesToLegacyStep(t *testing.T) {
	legacy := `jobs:
  review:
    steps:
      - id: review
        name: Review changes
        uses: agent/reviewer
        if: inputs.enabled
        continue-on-error: true
        with:
          prompt: |
            Review this task:
            ${{ inputs.task }}
          model: small
          message: fallback
          workspace: /selected
          output:
            format: json
            repair-attempts: 0
            schema: {type: object, properties: {approved: {type: boolean}}}
`
	concise := strings.Replace(legacy, "uses: agent/reviewer", "agent: reviewer\n        prompt: |\n          Review this task:\n          ${{ inputs.task }}", 1)
	concise = strings.Replace(concise, "          prompt: |\n            Review this task:\n            ${{ inputs.task }}\n", "", 1)
	var definitions []*Workflow
	for _, source := range []string{legacy, concise} {
		w, err := Parse([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(w); err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, w)
	}
	if !reflect.DeepEqual(definitions[0], definitions[1]) {
		t.Fatalf("authoring forms differ: legacy=%#v concise=%#v", definitions[0].Jobs["review"].Steps[0], definitions[1].Jobs["review"].Steps[0])
	}
}

func TestConciseAgentRejectsInvalidDeclarations(t *testing.T) {
	for _, tc := range []struct {
		step string
		want string
	}{
		{`agent: reviewer, uses: agent/reviewer, prompt: hi`, "agent cannot combine with uses"},
		{`agent: reviewer, uses: agent/other, prompt: hi`, "agent cannot combine with uses"},
		{`agent: reviewer, uses: '', prompt: hi`, "agent cannot combine with uses"},
		{`agent: reviewer, uses: null, prompt: hi`, "agent cannot combine with uses"},
		{`agent: reviewer, prompt: hi, with: {prompt: hi}`, "prompt cannot combine with with.prompt"},
		{`agent: reviewer, prompt: hi, with: {prompt: null}`, "prompt cannot combine with with.prompt"},
		{`uses: agent/reviewer, prompt: hi`, "prompt requires agent"},
		{`prompt: hi`, "prompt requires agent"},
		{`agent: 42, prompt: hi`, "agent must be a string"},
		{`agent: true, prompt: hi`, "agent must be a string"},
		{`agent: null, prompt: hi`, "agent must be a string"},
		{`agent: [reviewer], prompt: hi`, "agent must be a string"},
		{`agent: {name: reviewer}, prompt: hi`, "agent must be a string"},
		{`agent: reviewer, prompt: 42`, "prompt must be a string"},
		{`agent: reviewer, prompt: true`, "prompt must be a string"},
		{`agent: reviewer, prompt: null`, "prompt must be a string"},
		{`agent: reviewer, prompt: [hi]`, "prompt must be a string"},
		{`agent: reviewer, prompt: {text: hi}`, "prompt must be a string"},
		{`agent: '', prompt: hi`, "agent requires a valid role name"},
		{`agent: ' reviewer ', prompt: hi`, "agent requires a valid role name"},
		{`agent: agent/reviewer, prompt: hi`, "agent requires a valid role name"},
		{`agent: '${{ inputs.role }}', prompt: hi`, "agent requires a valid role name"},
		{`agent: reviewer, prompt: hi, argv: [echo, hi]`, "commands cannot have uses or with"},
		{`agent: reviewer, prompt: hi, run: echo, shell: sh`, "commands cannot have uses or with"},
		{`agent: reviewer`, "requires prompt or message"},
		{`agent: reviewer, prompt: hi, with: {unsupported: true}`, `unsupported parameter "unsupported"`},
	} {
		t.Run(tc.step, func(t *testing.T) {
			w, err := Parse([]byte("jobs: {review: {steps: [{" + tc.step + "}]}}"))
			if err == nil {
				err = Validate(w)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
	for _, step := range []string{
		`agent: reviewer, with: {prompt: hi}`,
		`agent: reviewer, with: {message: hi}`,
		`agent: 'true', prompt: '42'`,
	} {
		w, err := Parse([]byte("jobs: {review: {steps: [{" + step + "}]}}"))
		if err == nil {
			err = Validate(w)
		}
		if err != nil {
			t.Errorf("valid declaration %q: %v", step, err)
		}
	}
}

func TestConciseAgentGoValidation(t *testing.T) {
	agent, prompt := "reviewer", "hi"
	for _, step := range []Step{
		{Agent: &agent, Uses: "agent/reviewer", Prompt: &prompt},
		{Agent: &agent, Prompt: &prompt, With: map[string]any{"prompt": "other"}},
		{Prompt: &prompt, Uses: "agent/reviewer"},
	} {
		if err := Validate(&Workflow{Jobs: map[string]Job{"review": {Steps: []Step{step}}}}); err == nil {
			t.Fatalf("accepted conflicting Go declaration: %#v", step)
		}
	}
	parameters := map[string]any{"model": "small"}
	w := &Workflow{Jobs: map[string]Job{"review": {Steps: []Step{{Agent: &agent, Prompt: &prompt, With: parameters}}}}}
	if err := Validate(w); err != nil {
		t.Fatal(err)
	}
	if len(parameters) != 1 {
		t.Fatal("validation mutated caller-owned parameters")
	}
}

func TestConciseAgentExecutionPreservesResultPathsAndLiteralInputs(t *testing.T) {
	for _, concise := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "concise"}[concise], func(t *testing.T) {
			source := `on:
  workflow_call:
    inputs: {task: {type: string, required: true}}
    outputs: {result: {value: '${{ jobs.review.outputs.result }}'}}
jobs:
  review:
    outputs: {result: '${{ steps.report.outputs.text }}'}
    steps:
      - id: review
        uses: agent/reviewer
        with:
          prompt: |
            Review:
            ${{ inputs.task }}
          model: small
          workspace: /selected
          output:
            format: json
            repair-attempts: 0
            schema: {type: object, required: [approved], properties: {approved: {type: boolean}}}
      - id: report
        uses: agent/reporter
        with: {prompt: 'Approved: ${{ steps.review.outputs.structured.approved }}'}
`
			if concise {
				source = strings.Replace(source, "uses: agent/reviewer", "agent: reviewer\n        prompt: |\n          Review:\n          ${{ inputs.task }}", 1)
				source = strings.Replace(source, "          prompt: |\n            Review:\n            ${{ inputs.task }}\n", "", 1)
			}
			var calls []AgentRequest
			root := t.TempDir()
			writeWorkflow(t, root, "main.yml", source)
			e := &Executor{WorkspaceDir: root, Agents: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
				calls = append(calls, req)
				if req.AgentID == "reviewer" {
					return map[string]any{"text": `{"approved":true}`}, nil
				}
				return map[string]any{"text": req.Prompt}, nil
			})}
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"task": "${{ secrets.unavailable }}"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || calls[0].AgentID != "reviewer" || calls[0].Model != "small" || calls[0].Workspace != "/selected" || !strings.HasPrefix(calls[0].Prompt, "Review:\n${{ secrets.unavailable }}\n") || calls[0].Output == nil || calls[0].Output.RepairAttempts != 0 || calls[1].Prompt != "Approved: true" {
				t.Fatalf("unexpected requests: %#v", calls)
			}
			if run.Status != RunStatusSucceeded || run.Outputs["result"] != "Approved: true" {
				t.Fatalf("unexpected result: %#v", run)
			}
		})
	}
}

func TestConciseAgentResumeRetainsAdmittedDefinition(t *testing.T) {
	ctx := context.Background()
	var prompts []string
	e, store := durableExecutor(t, `jobs:
  build:
    steps:
      - id: edit
        agent: builder
        prompt: edit
      - id: check
        agent: builder
        prompt: check
`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		prompts = append(prompts, req.Prompt)
		if req.Prompt == "check" && len(prompts) == 2 {
			return nil, errors.New("lost result")
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run == nil {
		t.Fatalf("expected recorded failed run, got %#v, %v", run, err)
	}
	_, snapshot, err := store.Load(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := snapshot.Definitions["workflows/main.yml"].Jobs["build"].Steps[0]
	if step.Agent != nil || step.Prompt != nil || step.Uses != "agent/builder" || step.With["prompt"] != "edit" {
		t.Fatalf("snapshot was not canonical: %#v", step)
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", "jobs: {changed: {steps: [{agent: other, prompt: changed}]}}")
	if _, err := e.Resume(ctx, run.ID, nil, false); err == nil {
		t.Fatal("resume repeated a failed effect without acknowledgement")
	}
	if len(prompts) != 2 {
		t.Fatalf("unguarded resume ran an agent: %v", prompts)
	}
	run, err = e.Resume(ctx, run.ID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunStatusSucceeded || !reflect.DeepEqual(prompts, []string{"edit", "check", "check"}) {
		t.Fatalf("admitted definition or completed results changed: run=%#v prompts=%v", run, prompts)
	}
	attempts, err := store.Attempts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 || attempts[0].StepID != "edit" || attempts[2].StepID != "check" || attempts[2].Number != 2 {
		t.Fatalf("unexpected attempt history: %#v", attempts)
	}
}
