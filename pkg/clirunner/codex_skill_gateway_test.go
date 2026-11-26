package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// This uses the stock pinned native loader and public app-server events against
// a loopback API. Synthetic model responses exercise controls, not adherence.
func TestCodexSkillAgainstFakeGateway(t *testing.T) {
	for _, scenario := range []string{"native", "socket-cut", "stream-retry-control", "request-retry-control", "websocket-control", "unbounded-control", "inherited-url-control", "project-url-control", "auth-control", "header-control", "name-control"} {
		t.Run(scenario, func(t *testing.T) { testCodexSkillGateway(t, scenario) })
	}
}

func testCodexSkillGateway(t *testing.T, scenario string) {
	if os.Getenv("AGENT_WORKFLOW_CODEX_TEST") != "1" {
		t.Skip("enable with installed pinned Codex")
	}
	binary := os.Getenv("AGENT_WORKFLOW_CODEX_BINARY")
	if binary == "" {
		var err error
		binary, err = filepath.Abs("../../images/codex/node_modules/.bin/codex")
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal("installed pinned Codex required", err)
	}
	root := t.TempDir()
	req := codexSkillFixture(t)
	req.AgentID = "native"
	req.Prompt = "$hostile $probe Complete fixture."
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, ".agents/skills/probe/SKILL.md"), filepath.Join(state, "skills/hostile/SKILL.md")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(filepath.Dir(path))
		if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: HOSTILE_CATALOG_139\n---\nHOSTILE_BODY_139"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	var sawDenied atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("nonfixture API path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		n := calls.Add(1)
		if scenario == "socket-cut" {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			_ = conn.Close()
			return
		}
		if strings.Contains(string(body), "HOSTILE_BODY_139") || strings.Contains(string(body), "HOSTILE_CATALOG_139") {
			t.Error("unadmitted skill loaded")
		}
		if !strings.Contains(string(body), "EXACT_NATIVE_SKILL") {
			t.Error("admitted native bytes absent")
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		tools, _ := request["tools"].([]any)
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			for _, name := range []string{"spawn_agent", "send_input", "resume_agent", "wait_agent", "close_agent", "spawn_agents_on_csv"} {
				if tool["name"] == name {
					t.Errorf("delegation tool exposed: %s", name)
				}
			}
		}
		item := map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprintf("msg_fixture_%d", n), "content": []any{map[string]any{"type": "output_text", "text": "NATIVE_SKILL_OK"}}}
		if n == 1 {
			item = map[string]any{"type": "function_call", "id": "fc_forbidden_child", "call_id": "forbidden-child-139", "name": "spawn_agent", "arguments": `{"message":"MUST_NOT_DISPATCH"}`}
		}
		if n == 2 {
			if strings.Contains(string(body), "unsupported call: spawn_agent") {
				sawDenied.Store(true)
				for _, input := range request["input"].([]any) {
					value, _ := input.(map[string]any)
					if value["type"] == "function_call_output" && value["call_id"] == "forbidden-child-139" {
						original, _ := json.Marshal(value)
						t.Logf("original denied native delegation: %s", original)
					}
				}
			} else {
				t.Error("forced native delegation lacked original denial", string(body))
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("resp_%d", n)}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%d", n), "usage": map[string]any{"input_tokens": 0, "input_tokens_details": nil, "output_tokens": 0, "output_tokens_details": nil, "total_tokens": 0}}},
		} {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	defer gateway.Close()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := filepath.Join(root, "codex-loopback")
	script := "#!/bin/sh\nexec " + quote(binary) + " \"$@\" --config " + quote("model_providers."+codexSkillProvider+".base_url="+fmt.Sprintf("%q", gateway.URL)) + "\n"
	if scenario == "inherited-url-control" || scenario == "project-url-control" {
		script = "#!/bin/sh\nexec " + quote(binary) + " \"$@\"\n"
		config := fmt.Sprintf("[model_providers.%s]\nbase_url=%q\n", codexSkillProvider, gateway.URL)
		location := filepath.Join(state, "config.toml")
		if scenario == "project-url-control" {
			for _, d := range []string{".git/objects", ".git/refs"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, ".git/HEAD"), []byte("ref: refs/heads/fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(location, []byte(fmt.Sprintf("[projects.%q]\ntrust_level=\"trusted\"\n", root)), 0600); err != nil {
				t.Fatal(err)
			}
			location = filepath.Join(root, ".codex/config.toml")
			if err := os.MkdirAll(filepath.Dir(location), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(location, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		req.SkillRoot = filepath.Join(root, "missing-root-no-provider-fallback")
	}
	switch scenario {
	case "auth-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".env_key=\"UNADMITTED_CREDENTIAL\"'\n"
	case "header-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".http_headers.unadmitted=\"foreign\"'\n"
	case "name-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".name=\"foreign\"'\n"
	case "stream-retry-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".stream_max_retries=1'\n"
	case "request-retry-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".request_max_retries=1'\n"
	case "websocket-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'model_providers." + codexSkillProvider + ".supports_websockets=true'\n"
	case "unbounded-control":
		script = strings.TrimSuffix(script, "\n") + " --config 'features.unbounded_connection_retries=true'\n"
	}
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SKILL_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "codex-app-server", Command: command, Authentication: Authentication{Mode: "api-key", CredentialEnv: "CODEX_SKILL_FIXTURE_KEY"}}}}}
	ledger := &workflows.SkillEvidence{}
	observations := 0
	observe := func(event workflows.ProviderEvent) {
		if event.Type != "skill_evidence" {
			return
		}
		data, _ := json.Marshal(event.Data)
		var observation workflows.SkillObservation
		if err := json.Unmarshal(data, &observation); err != nil {
			t.Error(err)
			return
		}
		if err := ledger.Observe(req.Skill, observation); err != nil {
			t.Error(err)
		}
		t.Logf("original native skill observation: %s", data)
		observations++
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	first, err := runner.RunTurn(ctx, req, observe)
	if strings.HasSuffix(scenario, "-control") {
		expected := "retry/transport"
		if scenario == "inherited-url-control" {
			expected = "endpoint was not supplied"
		}
		if scenario == "project-url-control" {
			expected = "skill isolation"
		} // Native loader leaves this project layer inactive; deliberately absent root prevents any fallback turn.
		if scenario == "auth-control" {
			expected = "unadmitted control env_key"
		}
		if scenario == "header-control" {
			expected = "routing headers changed"
		}
		if err == nil || !strings.Contains(err.Error(), expected) || calls.Load() != 0 {
			t.Fatalf("ineffective retry controls reached API: result=%+v error=%v calls=%d", first, err, calls.Load())
		}
		if scenario == "project-url-control" {
			t.Log("project URL not effective in this native fixture; absent retained root blocks turn, API=0 (not proof of an active project endpoint rejection)")
		} else {
			t.Logf("%s: effective control mismatch rejected before API (0)", scenario)
		}
		return
	}
	if scenario == "socket-cut" {
		if err == nil || calls.Load() != 1 || observations != 1 || first.Status == workflows.RunStatusSucceeded {
			t.Fatalf("consumed native request replaced/settled: result=%+v error=%v calls=%d observations=%d", first, err, calls.Load(), observations)
		}
		t.Logf("consumed-request socket cut: attempts=%d original native consumption=%d terminal=%s error=%v", calls.Load(), observations, first.ProviderStatus, err)
		return
	}
	if err != nil || first.Outputs["text"] != "NATIVE_SKILL_OK" || !sawDenied.Load() || observations != 1 || ledger.Continuation(req.Skill, first.SessionID) != nil {
		t.Fatalf("initial result=%+v error=%v calls=%d observations=%d denied=%v", first, err, calls.Load(), observations, sawDenied.Load())
	}
	// A fresh native process cannot emit raw events on thread/resume in this pin.
	// Continuation retains the original receipt, not a synthetic second event.
	req.SessionID = first.SessionID
	if _, err := runner.RunTurn(ctx, req, observe); err == nil || calls.Load() != 2 {
		t.Fatalf("missing original receipt dispatched continuation: %v calls=%d", err, calls.Load())
	}
	req.SkillEvidence = ledger
	resumed, err := runner.RunTurn(ctx, req, observe)
	if err != nil || resumed.SessionID != first.SessionID || calls.Load() != 3 || observations != 1 {
		t.Fatalf("resume=%+v error=%v calls=%d observations=%d", resumed, err, calls.Load(), observations)
	}
	// Registering a missing root must fail effective isolation before a turn.
	req.SessionID = ""
	req.SkillRoot = filepath.Join(root, "missing-retained-root")
	_, err = runner.RunTurn(ctx, req, observe)
	if err == nil || !strings.Contains(err.Error(), "skill isolation") || calls.Load() != 3 {
		t.Fatalf("missing root dispatched: %v calls=%d", err, calls.Load())
	}
	t.Logf("loopback calls=%d original observations=%d initial session=%s continued session=%s missing-ledger extra API=0 missing-root extra API=0", calls.Load(), observations, first.SessionID, resumed.SessionID)
}
