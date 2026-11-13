package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type conversationContractRunner struct {
	runner *Runner
	calls  int
}

func (r *conversationContractRunner) Preflight(context.Context, []string) error { return nil }
func (r *conversationContractRunner) HasAgent(id string) bool                   { return r.runner.HasAgent(id) }
func (r *conversationContractRunner) ValidateConversationOutput(id string, schema map[string]any) error {
	return r.runner.ValidateConversationOutput(id, schema)
}
func (r *conversationContractRunner) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (r *conversationContractRunner) RunAgent(context.Context, workflows.AgentRequest) (map[string]any, error) {
	r.calls++
	return nil, fmt.Errorf("unexpected agent call")
}
func (r *conversationContractRunner) RunTurn(context.Context, workflows.SessionRequest, func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	r.calls++
	return workflows.SessionResult{}, fmt.Errorf("unexpected participant call")
}

func TestSDKConversationRenderedContractAdmissionBeforeParticipants(t *testing.T) {
	for _, tc := range []struct{ name, preceding, expression string }{
		{"input", "", "inputs.property"},
		{"preceding-result", "      - {id: collect, argv: [printf, __proto__]}\n", "steps.collect.outputs.stdout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Mkdir(filepath.Join(workspace, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			source := "on: {workflow_call: {inputs: {property: {type: string}}}}\njobs:\n  solve:\n    steps:\n" + tc.preceding + "      - conversation: [builder, reviewer]\n        finishBy: builder\n        prompt: Work\n        with:\n          output:\n            schema: {type: object, required: ['${{ " + tc.expression + " }}']}\n"
			if err := os.WriteFile(filepath.Join(workspace, "workflows", "main.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := workflows.OpenSQLite(filepath.Join(workspace, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			t.Setenv("SDK_RENDERED_CONTRACT_KEY", "fake-only")
			agent := Agent{Provider: "claude-sdk", Authentication: Authentication{Mode: "gateway", Endpoint: "http://127.0.0.1:1", CredentialEnv: "SDK_RENDERED_CONTRACT_KEY"}}
			runner := &conversationContractRunner{runner: &Runner{Workspace: workspace, Config: Config{Agents: map[string]Agent{"builder": agent, "reviewer": agent}}}}
			ready := false
			executor := &workflows.Executor{WorkspaceDir: workspace, Agents: runner, Store: store, Ready: func(context.Context) error { ready = true; return nil }}
			run, err := executor.Run(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"property": "__proto__"}})
			if err == nil || !strings.Contains(err.Error(), "cannot declare __proto__") || !ready || runner.calls != 0 || run.Status != workflows.RunStatusFailed {
				t.Fatalf("rendered contract not rejected before participants: calls=%d ready=%v run=%+v err=%v", runner.calls, ready, run, err)
			}
			if tc.preceding != "" && run.Jobs["solve"].Steps["collect"].Status != workflows.RunStatusSucceeded {
				t.Fatal("preceding result was not produced")
			}
		})
	}
}

func TestSDKConversationContractAdmissionBeforeEffects(t *testing.T) {
	for _, schema := range []string{
		"{type: object, properties: {__proto__: {type: boolean}}}",
		"{type: object, required: [__proto__]}",
		"{type: object, properties: {nested: {type: object, properties: {__proto__: {type: string}}}}}",
		"{type: object, properties: {enum: {type: object, required: [__proto__]}}}",
		"{type: object, properties: {values: {type: array, items: {type: object, required: [__proto__]}}}}",
		"{type: object, additionalProperties: {type: object, required: [__proto__]}}",
		"{type: object, enum: [{__proto__: true}]}",
	} {
		t.Run(schema, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Mkdir(filepath.Join(workspace, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			source := "schemas: {Completion: " + schema + "}\njobs: {solve: {steps: [{argv: [echo, effect]}, {conversation: [builder, reviewer], finishBy: builder, prompt: task, with: {output: {schema: Completion}}}]}}"
			if err := os.WriteFile(filepath.Join(workspace, "workflows", "main.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := workflows.OpenSQLite(filepath.Join(workspace, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			agent := Agent{Provider: "claude-sdk"}
			runner := &conversationContractRunner{runner: &Runner{Workspace: workspace, Config: Config{Agents: map[string]Agent{"builder": agent, "reviewer": agent}}}}
			ready := false
			executor := &workflows.Executor{WorkspaceDir: workspace, Agents: runner, Store: store, Ready: func(context.Context) error { ready = true; return nil }}
			_, err = executor.Run(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), "cannot declare __proto__") || ready {
				t.Fatalf("unsupported contract was not rejected before effects: ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestSDKConversationContractAcceptsEnumData(t *testing.T) {
	runner := &Runner{Config: Config{Agents: map[string]Agent{"builder": {Provider: "claude-sdk"}}}}
	for _, value := range []any{
		map[string]any{"required": []any{"__proto__"}},
		map[string]any{"properties": map[string]any{"required": []any{"__proto__"}}},
	} {
		schema := map[string]any{"type": "object", "enum": []any{value}}
		if err := runner.ValidateConversationOutput("builder", schema); err != nil {
			t.Fatalf("transportable enum data rejected: value=%v err=%v", value, err)
		}
	}
}

func TestSDKConversationAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; local fake API only")
	}
	for _, tc := range []struct {
		name, schema string
		first        any
		invalid      bool
	}{
		{"invalid-type", "{type: object, properties: {answer: {type: string}}, required: [answer]}", map[string]any{"answer": 42}, true},
		{"invalid-extra-property", "{type: object, properties: {answer: {type: string}}, required: [answer], additionalProperties: false}", map[string]any{"answer": "approved", "extra": true}, true},
		{"normalized-prototype-property", "{type: object, properties: {answer: {type: string}}, required: [answer], additionalProperties: false}", map[string]any{"answer": "approved", "__proto__": true}, false},
		{"normalized-array", "{}", []any{}, false},
		{"normalized-scalar", "{}", "invalid", false},
		{"normalized-null", "{}", nil, false},
		{"invalid-size", "{type: object, properties: {answer: {type: string}}, required: [answer], additionalProperties: false}", map[string]any{"answer": 42, "padding": strings.Repeat("x", 1<<20)}, true},
		{"annotation-only", "{description: Completion}", map[string]any{"answer": "approved"}, false},
		{"object-enum", "{type: object, enum: [{answer: approved}]}", map[string]any{"answer": "approved"}, false},
		{"object-enum-required-data", "{type: object, enum: [{required: [__proto__]}]}", map[string]any{"required": []any{"__proto__"}}, false},
		{"unconstrained-json", "{}", map[string]any{"answer": "approved", "nested": map[string]any{"retained": true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := workflows.ToolDefinition{Server: "workflow-conversation", Name: "finish"}
			qualified := "mcp__broker__" + workflows.ToolWireName(definition)
			var requests atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "count_tokens") {
					_, _ = w.Write([]byte(`{"input_tokens":16}`))
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				model, _ := payload["model"].(string)
				number := requests.Add(1)
				if tc.invalid && number > 1 {
					history, _ := json.Marshal(payload["messages"])
					if !strings.Contains(string(history), "invalid structured output") {
						t.Errorf("invalid completion did not reach controller: %s", history)
					}
				}
				if number == 1 || (tc.invalid && number == 2) {
					input := tc.first
					id := "invalid-or-first"
					if number == 2 {
						input = map[string]any{"answer": "approved"}
						id = "attempted-repair"
					}
					writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": id, "name": qualified, "input": input}}, "tool_use")
					return
				}
				writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "SDK_CONVERSATION_OK"}}, "end_turn")
			}))
			defer gateway.Close()
			bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge", "claude", "bridge.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SDK_CONVERSATION_KEY", "fake-gateway-only")
			workspace := t.TempDir()
			if err := os.Mkdir(filepath.Join(workspace, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			source := "jobs: {solve: {steps: [{id: solution, conversation: [builder, reviewer], finishBy: builder, prompt: task, with: {output: {schema: " + tc.schema + "}}}]}}"
			if err := os.WriteFile(filepath.Join(workspace, "workflows", "main.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := workflows.OpenSQLite(filepath.Join(workspace, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			agent := Agent{Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-5", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_CONVERSATION_KEY"}}
			runner := &Runner{Workspace: workspace, Config: Config{Agents: map[string]Agent{"builder": agent, "reviewer": agent}}}
			executor := &workflows.Executor{WorkspaceDir: workspace, Agents: runner, Store: store, Slots: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			run, err := executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "invalid structured output") || run.Status != workflows.RunStatusFailed || requests.Load() != 3 {
					t.Fatalf("invalid completion was repaired or hidden: run=%+v requests=%d err=%v", run, requests.Load(), err)
				}
				return
			}
			if err != nil || run.Status != workflows.RunStatusSucceeded || requests.Load() != 2 {
				t.Fatalf("completion contract failed: run=%+v requests=%d err=%v", run, requests.Load(), err)
			}
			usage, usageErr := store.Usage(ctx, run.ID)
			if usageErr != nil || usage.Summary.InputTokens == nil || *usage.Summary.InputTokens < 16 || usage.Summary.OutputTokens == nil || !usage.Summary.Complete || usage.Summary.CostUSD == nil {
				t.Fatalf("SDK conversation observations did not reach durable accounting: %+v %v", usage, usageErr)
			}
			got, _ := json.Marshal(run.Jobs["solve"].Steps["solution"].Outputs["structured"])
			expected := tc.first
			switch tc.name {
			case "normalized-prototype-property":
				expected = map[string]any{"answer": "approved"}
			case "normalized-array", "normalized-null":
				expected = map[string]any{}
			case "normalized-scalar":
				expected = map[string]any{"0": "i", "1": "n", "2": "v", "3": "a", "4": "l", "5": "i", "6": "d"}
			}
			want, _ := json.Marshal(expected)
			if string(got) != string(want) {
				t.Fatalf("SDK changed completion arguments: got=%s want=%s", got, want)
			}
		})
	}
}
