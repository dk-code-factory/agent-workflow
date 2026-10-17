package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func TestBridgeContracts(t *testing.T) {
	cases := []struct {
		name, payload string
		wantError     bool
	}{{"complete", `{"type":"session","session_id":"s"}
{"type":"progress","data":{"type":"assistant"}}
{"type":"result","session_id":"s","status":"succeeded","outputs":{"text":"done"}}
`, false}, {"partial", `{"type":"progress"}` + "\n", true}, {"failed", `{"type":"result","status":"failed","error":"gateway refused"}` + "\n", true}, {"malformed", "{bad}\n", true}, {"after_terminal", `{"type":"result","status":"succeeded"}` + "\n" + `{"type":"progress"}` + "\n", true}, {"unknown", `{"type":"future"}` + "\n" + `{"type":"result","status":"succeeded","outputs":{"text":"ok"}}` + "\n", false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := 0
			result, err := parseBridge(strings.NewReader(tc.payload), func(workflows.ProviderEvent) { events++ })
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.name == "complete" && (result.SessionID != "s" || events != 2) {
				t.Fatal(result, events)
			}
		})
	}
}
func TestGatewayReferencesAndEnvironmentIsolation(t *testing.T) {
	t.Setenv("TEST_GATEWAY_KEY", "gateway-secret")
	t.Setenv("GH_TOKEN", "controller-only")
	t.Setenv("SSH_AUTH_SOCK", "controller-socket")
	env := providerEnvironment("TEST_GATEWAY_KEY")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "controller-only") || strings.Contains(joined, "controller-socket") {
		t.Fatal("controller credentials leaked")
	}
	if !strings.Contains(joined, "gateway-secret") {
		t.Fatal("explicit provider credential missing")
	}
}
func TestCodexSessionFakeAppServer(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_FAKE_APP_SERVER") == "1" {
		scanner := bufio.NewScanner(os.Stdin)
		encoder := json.NewEncoder(os.Stdout)
		for scanner.Scan() {
			var frame struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				os.Exit(2)
			}
			switch frame.Method {
			case "initialize":
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{}})
			case "thread/start", "thread/resume":
				if frame.Params["approvalPolicy"] != "never" {
					os.Exit(3)
				}
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}}})
			case "turn/start":
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-1"}}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "background-thread", "turn": map[string]string{"id": "background-turn", "status": "completed"}}})
				_ = encoder.Encode(map[string]any{"method": "future/event", "params": map[string]any{}})
				_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]string{"type": "agentMessage", "text": "done"}}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": "completed"}}})
			}
		}
		os.Exit(0)
	}
	// A small executable wrapper selects this test inside the supervised process.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fake := filepath.Join(root, "fake-codex")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestCodexSessionFakeAppServer$\n"
	if err = os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_WORKFLOW_FAKE_APP_SERVER", "1")
	// Native auth intentionally passes only known auth environment. Use a wrapper
	// that sets this test-only selector; it is not part of the production env.
	script = "#!/bin/sh\nexport AGENT_WORKFLOW_FAKE_APP_SERVER=1\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestCodexSessionFakeAppServer$\n"
	_ = os.WriteFile(fake, []byte(script), 0700)
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {Provider: "codex-app-server", Command: fake, Authentication: Authentication{Mode: "native"}}}}}
	result, err := runner.RunTurn(context.Background(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "a", Prompt: "task"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != workflows.RunStatusSucceeded || result.SessionID != "thread-1" || result.Outputs["text"] != "done" {
		t.Fatal(result)
	}
	result, err = runner.RunTurn(context.Background(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "a", Prompt: "follow up"}, SessionID: result.SessionID}, nil)
	if err != nil || result.SessionID != "thread-1" {
		t.Fatal(result, err)
	}
}

func TestNonzeroClaudeRetainsStdoutDiagnostics(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "claude")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":true,\"result\":\"401 invalid OAuth token\"}'\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {Provider: "claude-cli", Command: fake}}}}
	out, err := r.RunAgent(context.Background(), workflows.AgentRequest{AgentID: "a", Prompt: "hello"})
	if err == nil || !strings.Contains(err.Error(), "401 invalid OAuth token") || out["stdout"] == nil {
		t.Fatal("stdout failure was lost", out, err)
	}
}

func TestNativeCompatibilityEnvironmentExcludesControllerCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "controller-gh")
	t.Setenv("GITHUB_TOKEN", "controller-github")
	t.Setenv("SSH_AUTH_SOCK", "controller-agent")
	t.Setenv("ANTHROPIC_API_KEY", "provider-native")
	joined := strings.Join(nativeEnvironment(), "\n")
	for _, value := range []string{"controller-gh", "controller-github", "controller-agent"} {
		if strings.Contains(joined, value) {
			t.Fatal("controller authority forwarded to native worker")
		}
	}
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=provider-native") {
		t.Fatal("provider-owned native authentication removed")
	}
}
