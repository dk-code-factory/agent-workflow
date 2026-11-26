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

// Actual locked SDK/CLI with synthetic credentials and an explicit loopback
// endpoint. These assertions concern native loading/events, not model adherence.
func TestSkillSDKAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("requires installed locked SDK; local fake API only")
	}
	for _, scenario := range []string{"selected", "selected-resume", "wrong-arguments", "report-only", "blocked-native-child", "missing-plugin", "corrupt-plugin"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			plugin := filepath.Join(root, "admitted")
			resource := filepath.Join(plugin, "skills/probe/reference.txt")
			for name, text := range map[string]string{".claude-plugin/plugin.json": `{"name":"aw-probe","version":"1.0.0"}`, "skills/probe/SKILL.md": "---\nname: probe\ndescription: Admitted fixture.\n---\nADMITTED_SKILL_NATIVE_139\n", "skills/probe/reference.txt": "ADMITTED_RESOURCE_NATIVE_139\n"} {
				p := filepath.Join(plugin, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Same-name repository skill is outside the explicit selected plugin.
			hostile := filepath.Join(root, ".claude/skills/probe/SKILL.md")
			if err := os.MkdirAll(filepath.Dir(hostile), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hostile, []byte("---\nname: probe\ndescription: HOSTILE_CATALOG_NATIVE_139\n---\nHOSTILE_BODY_NATIVE_139\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-plugin" {
				if err := os.RemoveAll(plugin); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt-plugin" {
				if err := os.WriteFile(filepath.Join(plugin, ".claude-plugin/plugin.json"), []byte("{invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			var sawSkill, sawResource, deniedChild atomic.Bool
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "count_tokens") {
					fmt.Fprint(w, `{"input_tokens":16}`)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					fmt.Fprint(w, `{"data":[],"has_more":false}`)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				n := calls.Add(1)
				raw, _ := json.Marshal(payload)
				if strings.Contains(string(raw), "HOSTILE_") {
					t.Error("unadmitted skill entered model request")
				}
				if strings.Contains(string(raw), "ADMITTED_SKILL_NATIVE_139") {
					sawSkill.Store(true)
				}
				if strings.Contains(string(raw), "ADMITTED_RESOURCE_NATIVE_139") {
					sawResource.Store(true)
				}
				messages, _ := payload["messages"].([]any)
				for _, raw := range messages {
					message, _ := raw.(map[string]any)
					content, _ := message["content"].([]any)
					for _, raw := range content {
						item, _ := raw.(map[string]any)
						if item["type"] == "tool_result" && item["tool_use_id"] == "blocked-child-call" && item["is_error"] == true {
							deniedChild.Store(true)
						}
					}
				}
				tools, _ := payload["tools"].([]any)
				for _, raw := range tools {
					tool, _ := raw.(map[string]any)
					if tool["name"] == "Agent" || tool["name"] == "Task" {
						t.Error("native children exposed")
					}
				}
				model, _ := payload["model"].(string)
				if n > 6 {
					w.WriteHeader(429)
					fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"fixture call limit"}}`)
					return
				}
				if (n == 1 || n == 4 && scenario == "selected-resume") && scenario == "blocked-native-child" {
					writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": "blocked-child-call", "name": "Agent", "input": map[string]string{"subagent_type": "general-purpose", "description": "Forbidden native child", "prompt": "Return CHILD_STARTED"}}}, "tool_use")
				} else if (n == 1 || n == 4 && scenario == "selected-resume") && scenario != "report-only" {
					argument := "exact"
					if scenario == "wrong-arguments" {
						argument = "changed"
					}
					writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": fmt.Sprintf("skill-original-call-%d", n), "name": "Skill", "input": map[string]string{"skill": "aw-probe:probe", "args": argument}}}, "tool_use")
				} else if (n == 2 || n == 5) && (scenario == "selected" || scenario == "selected-resume") {
					writeSDKMessage(w, model, []map[string]any{{"type": "tool_use", "id": fmt.Sprintf("read-original-call-%d", n), "name": "Read", "input": map[string]string{"file_path": resource}}}, "tool_use")
				} else {
					writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "REPORT_ONLY_DONE"}}, "end_turn")
				}
			}))
			defer gateway.Close()
			bridge, err := filepath.Abs(filepath.Join("..", "..", "bridge/claude/bridge.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SKILL_NATIVE_KEY", "synthetic-loopback-only")
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Tools: []string{"Read"}, Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SKILL_NATIVE_KEY"}}}}}
			plan := &workflows.SkillInvocation{Version: 1, ID: "fixture", Binding: workflows.SkillBinding{Version: 1, Name: "probe", Entry: "skills/probe/SKILL.md", Assets: &workflows.AssetBundle{}}, Arguments: "exact", Model: "claude-sonnet-4-6", Profile: claudeSkillProfile}
			ledger := &workflows.SkillEvidence{}
			var observations int
			var observationErr error
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			observe := func(event workflows.ProviderEvent) {
				if event.Type == "skill_evidence" {
					data, _ := json.Marshal(event.Data)
					var observation workflows.SkillObservation
					if e := json.Unmarshal(data, &observation); e != nil {
						observationErr = e
						return
					}
					observations++
					if e := ledger.Observe(plan, observation); e != nil {
						observationErr = e
					}
				}
			}
			request := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "native", Prompt: "Use the admitted skill with exact arguments, then read its reference.", Skill: plan, SkillRoot: plugin}}
			result, err := runner.RunTurn(ctx, request, observe)
			if err == nil && scenario == "selected-resume" {
				originalSession := result.SessionID
				request.SessionID = originalSession
				result, err = runner.RunTurn(ctx, request, observe)
				if originalSession == "" || result.SessionID != originalSession {
					t.Fatalf("resume changed original session: %q -> %q", originalSession, result.SessionID)
				}
			}
			if scenario == "missing-plugin" || scenario == "corrupt-plugin" {
				if err == nil || calls.Load() != 0 || observations != 0 {
					t.Fatalf("bad plugin crossed prompt gate: result=%+v err=%v calls=%d", result, err, calls.Load())
				}
				return
			}
			if err != nil || observationErr != nil {
				t.Fatal(result, err, observationErr)
			}
			if scenario == "selected" || scenario == "selected-resume" {
				expectedCalls, expectedObservations := int32(3), 2
				if scenario == "selected-resume" {
					expectedCalls, expectedObservations = 6, 4
				}
				if !sawSkill.Load() || !sawResource.Load() || calls.Load() != expectedCalls || observations != expectedObservations || ledger.Complete(plan) != nil {
					t.Fatalf("missing original skill/results: skill=%v resource=%v calls=%d ledger=%+v", sawSkill.Load(), sawResource.Load(), calls.Load(), ledger)
				}
			} else if ledger.Complete(plan) == nil || observations != 0 || sawSkill.Load() {
				t.Fatalf("report/wrong invocation completed: %+v", ledger)
			}
			if scenario == "blocked-native-child" && (!deniedChild.Load() || calls.Load() != 2) {
				t.Fatalf("native child did not return a denied tool result: calls=%d denied=%v", calls.Load(), deniedChild.Load())
			}
		})
	}
}
