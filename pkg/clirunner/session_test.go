package clirunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestBridgeContracts(t *testing.T) {
	cases := []struct {
		name, payload string
		wantError     bool
	}{{"complete", `{"type":"session","session_id":"s"}
{"type":"progress","data":{"type":"assistant"}}
{"type":"result","session_id":"s","status":"succeeded","outputs":{"text":"done"}}
`, false}, {"partial", `{"type":"progress"}` + "\n", true}, {"failed", `{"type":"result","status":"failed","error":"gateway refused"}` + "\n", true}, {"malformed", "{bad}\n", true}, {"after_terminal", `{"type":"result","status":"succeeded"}` + "\n" + `{"type":"progress"}` + "\n", true}, {"unknown", `{"type":"future"}` + "\n" + `{"type":"result","status":"succeeded","outputs":{"text":"ok"}}` + "\n", true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := 0
			var frames bytes.Buffer
			_ = json.NewEncoder(&frames).Encode(map[string]any{"version": BridgeProtocolVersion, "type": "hello", "provider_version": "claude-agent-sdk@" + ClaudeSDKVersion})
			for _, line := range strings.Split(strings.TrimSpace(tc.payload), "\n") {
				var frame map[string]any
				if json.Unmarshal([]byte(line), &frame) != nil {
					frames.WriteString(line + "\n")
					continue
				}
				frame["version"] = BridgeProtocolVersion
				_ = json.NewEncoder(&frames).Encode(frame)
			}
			var control bytes.Buffer
			result, err := bridgeTurn(context.Background(), &frames, &bridgeWriter{writer: &control}, map[string]any{}, nil, func(workflows.ProviderEvent) { events++ }, func() {})
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.name == "complete" && (result.SessionID != "s" || events != 2) {
				t.Fatal(result, events)
			}
		})
	}
}

func TestBridgeRejectsVersionMismatchBeforeSendingTask(t *testing.T) {
	for _, hello := range []string{`{"version":1,"type":"hello"}`, `{"version":2,"type":"hello","provider_version":"unrecognized-sdk"}`, `{"version":2,"type":"progress"}`} {
		var control bytes.Buffer
		_, err := bridgeTurn(context.Background(), strings.NewReader(hello+"\n"), &bridgeWriter{writer: &control}, map[string]any{"prompt": "MUST_NOT_SEND"}, nil, nil, func() {})
		if err == nil || control.Len() != 0 {
			t.Fatalf("mismatch allowed task effects: %s %v", control.String(), err)
		}
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
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"userAgent": "codex_cli_rs/0.164.0 (fixture)"}})
			case "account/login/start":
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{}})
			case "thread/start", "thread/resume":
				if frame.Params["approvalPolicy"] != "never" {
					os.Exit(3)
				}
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}}})
			case "turn/start":
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{"turn": map[string]string{"id": "turn-1"}}})
				if os.Getenv("AGENT_WORKFLOW_FAKE_APP_SERVER_INTERRUPT") != "" {
					_ = encoder.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1"}}})
					_ = encoder.Encode(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "thread-1", "tokenUsage": map[string]any{"total": map[string]int{"inputTokens": 10, "outputTokens": 2}}}})
					continue
				}
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "background-thread", "turn": map[string]string{"id": "background-turn", "status": "completed"}}})
				_ = encoder.Encode(map[string]any{"method": "future/event", "params": map[string]any{}})
				_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]string{"type": "agentMessage", "text": "done"}}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": "completed"}}})
			case "turn/interrupt":
				_ = os.WriteFile("interrupt-observed", []byte("yes"), 0600)
				_ = encoder.Encode(map[string]any{"id": frame.ID, "result": map[string]any{}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-1", "turn": map[string]string{"id": "turn-1", "status": "interrupted"}}})
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
	authFile := filepath.Join(root, "provider-auth.json")
	if err = os.WriteFile(authFile, []byte(`{"tokens":{"access_token":"FAKE_NATIVE_ACCESS_TOKEN","account_id":"fixture-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {Provider: "codex-app-server", Command: fake, Authentication: Authentication{Mode: "native", CredentialFile: authFile}}}}}
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
	t.Setenv("EXTERNAL_MCP_TOKEN", "broker-only")
	joined := strings.Join(nativeEnvironment(), "\n")
	for _, value := range []string{"controller-gh", "controller-github", "controller-agent", "broker-only"} {
		if strings.Contains(joined, value) {
			t.Fatal("controller authority forwarded to native worker")
		}
	}
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=provider-native") {
		t.Fatal("provider-owned native authentication removed")
	}
}

func TestCodexGracefulInterruptionRetainsPartialUsage(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(root, "fake-codex")
	script := "#!/bin/sh\nexport AGENT_WORKFLOW_FAKE_APP_SERVER=1\nexport AGENT_WORKFLOW_FAKE_APP_SERVER_INTERRUPT=1\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestCodexSessionFakeAppServer$\n"
	if err = os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(root, "auth.json")
	if err = os.WriteFile(authFile, []byte(`{"tokens":{"access_token":"fixture-access","account_id":"fixture-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {Provider: "codex-app-server", Command: fake, Authentication: Authentication{Mode: "native", CredentialFile: authFile}}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel only after the usage observation, so the asynchronous reader has
	// already learned the active turn and retained its partial counters.
	finished := make(chan struct{})
	var result workflows.SessionResult
	var runErr error
	go func() {
		defer close(finished)
		result, runErr = runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "a", Prompt: "wait"}}, func(event workflows.ProviderEvent) {
			if event.Type == "thread/tokenUsage/updated" {
				cancel()
			}
		})
	}()
	select {
	case <-finished:
	case <-time.After(7 * time.Second):
		t.Fatal("graceful interruption stalled")
	}
	if !errors.Is(runErr, context.Canceled) || result.SessionID != "thread-1" || result.Outputs["usage"] == nil {
		t.Fatalf("interruption lost identity or usage: %+v %v", result, runErr)
	}
	if _, err = os.Stat(filepath.Join(root, "interrupt-observed")); err != nil {
		t.Fatal("provider interrupt handshake was not sent", err)
	}
}
