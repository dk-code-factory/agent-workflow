package workflows

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAgentSelectionPinsAllDefinitionsAndRetainsSession(t *testing.T) {
	e, _, fake := agentTaskFixture(t, `on: {workflow_call: {inputs: {role: {type: string}}}}
jobs:
  review:
    steps:
      - id: first
        uses: agent/run
        with:
          agent: {select: '${{ inputs.role }}', definitions: {review: reviewer.yaml, alternate: alternate.yaml}}
          prompt: first
      - id: followup
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: next}
`, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "done"}, nil })
	other := filepath.Join(e.WorkspaceDir, "workflows", "alternate.yaml")
	if err := os.WriteFile(other, []byte("name: 'alternate'\ndescription: 'Alternate'\ninstructions: 'Pinned alternate instructions.'\nmodel: 'selected-model'\nreasoning-effort: 'high'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"role": "alternate"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{other, filepath.Join(e.WorkspaceDir, "workflows", "reviewer.yaml")} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	run, err := e.Resume(t.Context(), prepared.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	steps := run.Jobs["review"].Steps
	if steps["first"].Outputs["agent"] != steps["followup"].Outputs["agent"] {
		t.Fatal("continuation changed the selected agent", steps)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 2 || fake.requests[1].SessionID == "" {
		t.Fatal(fake.requests)
	}
	for _, request := range fake.requests {
		if request.Model != "selected-model" || request.Definition.Name != "alternate" || request.Definition.Model != "selected-model" || request.Definition.ReasoningEffort != "high" || request.Definition.DeveloperInstructions != "Pinned alternate instructions." {
			t.Fatal(request)
		}
	}
}

func TestNativeAgentSelectionRejectsUnknownNameBeforeDispatch(t *testing.T) {
	for _, selection := range []any{"unknown", "reviewer.yaml", "agent_00000000000000000000000000000000", true, 1, nil} {
		t.Run(fmt.Sprint(selection), func(t *testing.T) {
			e, _, fake := agentTaskFixture(t, `on: {workflow_call: {inputs: {role: {type: object}}}}
jobs: {review: {steps: [{uses: agent/run, with: {agent: {select: '${{ inputs.role.name }}', definitions: {review: reviewer.yaml}}, prompt: work}}]}}
`, nil)
			_, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"role": map[string]any{"name": selection}}})
			if err == nil || !strings.Contains(err.Error(), "unknown admitted agent definition") {
				t.Fatal(err)
			}
			if len(fake.requests) != 0 {
				t.Fatal("unknown selection dispatched", fake.requests)
			}
		})
	}
}

func TestNativeAgentSelectionAdmitsUnselectedDefinitions(t *testing.T) {
	e, _, fake := agentTaskFixture(t, `jobs: {review: {steps: [{uses: agent/run, with: {agent: {select: review, definitions: {review: reviewer.yaml, missing: missing.yaml}}, prompt: work}}]}}`, nil)
	if err := os.Remove(filepath.Join(e.WorkspaceDir, "workflows", "missing.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Fatal("unavailable unselected definition admitted", err)
	}
	if len(fake.requests) != 0 {
		t.Fatal(fake.requests)
	}
}

func TestNativeAgentSelectionRejectsDynamicAndMalformedCatalogs(t *testing.T) {
	oversized := make(map[string]any, 33)
	for i := range 33 {
		oversized[strings.Repeat("r", i+1)] = "reviewer.yaml"
	}
	if _, err := agentDefinitionChoices(map[string]any{"select": "r", "definitions": oversized}); err == nil {
		t.Fatal("oversized definition catalog accepted")
	}
	for _, value := range []string{
		`{select: review, definitions: {}}`,
		`{select: review, definitions: {review: '${{ inputs.path }}'}}`,
		`{select: review, definitions: '${{ inputs.definitions }}'}`,
		`{select: 1, definitions: {review: reviewer.yaml}}`,
		`{select: missing, definitions: {review: reviewer.yaml}}`,
		`{select: review, definitions: {review: reviewer.yaml}, extra: true}`,
	} {
		t.Run(value, func(t *testing.T) {
			workflow, err := Parse([]byte(`jobs: {review: {steps: [{uses: agent/run, with: {agent: ` + value + `, prompt: work}}]}}`))
			if err == nil {
				err = Validate(workflow)
			}
			if err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
}
