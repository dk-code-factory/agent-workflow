package workflows

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const namedReviewWorkflow = `schemas:
  Review:
    type: object
    required: [approved]
    additionalProperties: false
    properties:
      approved: {type: boolean}
jobs:
  review:
    steps:
      - id: first
        uses: agent/reviewer
        with:
          prompt: Review
          output: {format: json, schema: Review, repair-attempts: 0}
      - id: second
        uses: agent/reviewer
        with:
          prompt: Review again
          output: {schema: Review}
`

func TestNamedSchemasReuseAndInlineEquivalence(t *testing.T) {
	w, err := Parse([]byte(namedReviewWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if err = Validate(w); err != nil {
		t.Fatal(err)
	}
	for _, repair := range []map[string]any{nil, {"repair-attempts": 0}, {"repair_attempts": 2}, {"repairAttempts": 2}} {
		named := map[string]any{"format": "json", "schema": "Review"}
		inline := map[string]any{"format": "json", "schema": w.Schemas["Review"]}
		for key, value := range repair {
			named[key], inline[key] = value, value
		}
		resolved, err := resolveNamedOutput(w, named)
		if err != nil {
			t.Fatal(err)
		}
		left, err := ParseAgentOutputContract(resolved)
		if err != nil {
			t.Fatal(err)
		}
		right, err := ParseAgentOutputContract(inline)
		if err != nil || !reflect.DeepEqual(left, right) {
			t.Fatalf("contracts differ: %+v %+v %v", left, right, err)
		}
		for _, text := range []string{`{"approved":true}`, `{"approved":"yes"}`, `{}`, `{"approved":true,"extra":1}`} {
			if !reflect.DeepEqual(ValidateAgentStructuredOutput(text, left), ValidateAgentStructuredOutput(text, right)) {
				t.Fatalf("validation differs for %s", text)
			}
		}
	}
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", namedReviewWorkflow)
	var calls atomic.Int32
	e := Executor{WorkspaceDir: root, Agents: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		index := calls.Add(1)
		if req.Output == nil || len(req.Output.Schema) == 0 || (index == 1 && req.Output.RepairAttempts != 0) || (index == 2 && req.Output.RepairAttempts != 1) {
			return nil, errors.New("named contract was not supplied to runner")
		}
		return map[string]any{"text": `{"approved":true}`}, nil
	})}
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil || calls.Load() != 2 || run.Jobs["review"].Steps["second"].Outputs["structured"].(map[string]any)["approved"] != true {
		t.Fatalf("named execution: %+v %v calls=%d", run, err, calls.Load())
	}
}

func TestNamedSchemasRejectInvalidBeforeEffects(t *testing.T) {
	for _, tc := range []struct{ old, replacement, diagnostic string }{
		{"schema: Review", "schema: Missing", "with.output: schema"},
		{"schema: Review", "schema: '${{ inputs.contract }}'", "unknown workflow-local schema"},
		{"type: boolean", "type: unknown", "schemas.Review.properties.approved.type"},
		{"approved: {type: boolean}", "approved: {$ref: 'https://example.invalid/schema'}", "schemas.Review.properties.approved.$ref"},
		{"approved: {type: boolean}", "approved: {type: string, pattern: '['}", "schemas.Review"},
		{"required: [approved]", "required: wrong", "schemas.Review.required"},
		{"approved: {type: boolean}", "approved: false", "schemas.Review.properties.approved"},
		{"type: object", "type: array", "requires type: object"},
		{"additionalProperties: false", "allOf: []", "schemas.Review.allOf"},
		{"additionalProperties: false", "additionalProperties: {type: unknown}", "schemas.Review.additionalProperties.type"},
		{"approved: {type: boolean}", "approved: {type: string, maxLength: 0}", "schemas.Review.properties.approved.maxLength"},
		{"approved: {type: boolean}", "approved: {type: array, items: {type: invalid}}", "schemas.Review.properties.approved.items.type"},
		{"approved: {type: boolean}", "approved: {type: number, minimum: 'NaN'}", "schemas.Review.properties.approved.minimum"},
		{"approved: {type: boolean}", "approved: {type: number, minimum: '-Inf'}", "schemas.Review.properties.approved.minimum"},
		{"approved: {type: boolean}", "approved: {type: array, items: {type: number, minimum: 'Infinity'}}", "schemas.Review.properties.approved.items.minimum"},
	} {
		t.Run(tc.replacement, func(t *testing.T) {
			root := t.TempDir()
			writeWorkflow(t, root, "main.yml", strings.Replace(namedReviewWorkflow, tc.old, tc.replacement, 1))
			var calls atomic.Int32
			_, err := (&Executor{WorkspaceDir: root, Agents: agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{}, nil
			})}).Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) || calls.Load() != 0 {
				t.Fatalf("want %q before effects, got %v calls=%d", tc.diagnostic, err, calls.Load())
			}
		})
	}
	// An unused declaration must still be admitted before a provider can run.
	w, err := Parse([]byte(strings.Replace(namedReviewWorkflow, "  Review:", "  Unused: {type: invalid}\n  Review:", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err = Validate(w); err == nil || !strings.Contains(err.Error(), "schemas.Unused.type") {
		t.Fatal(err)
	}
}

func TestNamedSchemasReusableScope(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", "schemas: {Review: {type: boolean}}\njobs: {child: {uses: workflows/child.yml}}")
	child := "on: {workflow_call: {}}\n" + namedReviewWorkflow
	writeWorkflow(t, root, "child.yml", child)
	var calls atomic.Int32
	e := Executor{WorkspaceDir: root, Agents: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		calls.Add(1)
		if req.Output.Schema["type"] != "object" {
			return nil, errors.New("caller schema shadowed callee")
		}
		return map[string]any{"text": `{"approved":true}`}, nil
	})}
	if _, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"}); err != nil || calls.Load() != 2 {
		t.Fatal(err, calls.Load())
	}
	writeWorkflow(t, root, "child.yml", "on: {workflow_call: {}}\n"+namedReviewWorkflow[strings.Index(namedReviewWorkflow, "jobs:"):])
	if _, err := e.Load("workflows/main.yml"); err == nil || !strings.Contains(err.Error(), "unknown workflow-local schema") {
		t.Fatal("callee inherited caller schema", err)
	}
}

func TestNamedSchemasResumeKeepsAdmittedContract(t *testing.T) {
	var calls atomic.Int32
	e, store := durableExecutor(t, namedReviewWorkflow, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		index := calls.Add(1)
		if req.Output.Schema["type"] != "object" {
			return nil, errors.New("contract changed on resume")
		}
		if index == 2 {
			return nil, errors.New("provider unavailable")
		}
		return map[string]any{"text": `{"approved":true}`}, nil
	}))
	ctx := context.Background()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status != RunStatusFailed || calls.Load() != 2 {
		t.Fatal(run, err, calls.Load())
	}
	writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.Replace(namedReviewWorkflow, "type: object", "type: string", 1))
	// A fresh executor reads the durable definition snapshot, not the changed file.
	fresh := &Executor{WorkspaceDir: e.WorkspaceDir, Store: store, Agents: e.Agents}
	done, err := fresh.Resume(ctx, run.ID, nil, true)
	if err != nil || done.Status != RunStatusSucceeded || calls.Load() != 3 {
		t.Fatalf("resume: %+v %v calls=%d", done, err, calls.Load())
	}
}
