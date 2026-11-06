package clirunner

import (
	"context"
	"encoding/json"
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

func TestSDKConversationAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires locked SDK dependencies; local fake API only")
	}
	for _, tc := range []struct {
		name, schema string
		first        map[string]any
		invalid      bool
	}{
		{"invalid-type", "{type: object, properties: {answer: {type: string}}, required: [answer]}", map[string]any{"answer": 42}, true},
		{"invalid-extra-property", "{type: object, properties: {answer: {type: string}}, required: [answer], additionalProperties: false}", map[string]any{"answer": "approved", "extra": true}, true},
		{"annotation-only", "{description: Completion}", map[string]any{"answer": "approved"}, false},
		{"object-enum", "{type: object, enum: [{answer: approved}]}", map[string]any{"answer": "approved"}, false},
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
			got, _ := json.Marshal(run.Jobs["solve"].Steps["solution"].Outputs["structured"])
			want, _ := json.Marshal(tc.first)
			if string(got) != string(want) {
				t.Fatalf("SDK changed completion arguments: got=%s want=%s", got, want)
			}
		})
	}
}
