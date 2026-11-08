package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestClaudeSDKAuthenticationRoutes(t *testing.T) {
	for _, execution := range []Execution{{}, {Mode: "container", Image: "fixture"}} {
		for _, auth := range []Authentication{
			{Mode: "native"},
			{Mode: "native", CredentialEnv: "MY_CLAUDE_TOKEN"},
			{Mode: "gateway", Endpoint: "https://api.anthropic.com", CredentialEnv: "MY_API_KEY"},
		} {
			agent := Agent{Provider: "claude-sdk", Bridge: "bridge.mjs", Execution: execution, Authentication: auth}
			if err := (Config{Agents: map[string]Agent{"role": agent}}).Validate(); err != nil {
				t.Fatalf("rejected explicit authentication route %+v: %v", auth, err)
			}
		}
	}
	for _, auth := range []Authentication{
		{}, {Mode: "api-key"}, {Mode: "native", Endpoint: "https://other.invalid"},
		{Mode: "native", CredentialFile: "host-settings.json"},
		{Mode: "gateway", CredentialEnv: "KEY"}, {Mode: "gateway", Endpoint: "https://api.anthropic.com"},
		{Mode: "gateway", Endpoint: "https://api.anthropic.com", CredentialEnv: "KEY", CredentialFile: "host.json"},
		{Mode: "native", CredentialEnv: "GH_TOKEN"},
	} {
		if err := (Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Bridge: "bridge.mjs", Authentication: auth}}}).Validate(); err == nil {
			t.Fatalf("accepted conflicting or implicit route %+v", auth)
		}
	}
}

func TestClaudeSDKAuthenticationDoesNotFallBackToAmbientCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "ambient-subscription")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-api-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ambient.invalid")
	t.Setenv("GH_TOKEN", "controller-secret")
	t.Setenv("MY_SUBSCRIPTION", "selected-subscription")
	t.Setenv("MY_API_KEY", "selected-api-key")
	for _, tc := range []struct {
		auth            Authentication
		want, forbidden []string
	}{
		{Authentication{Mode: "native"}, []string{"CLAUDE_CODE_OAUTH_TOKEN=ambient-subscription"}, []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_BASE_URL=", "controller-secret"}},
		{Authentication{Mode: "native", CredentialEnv: "MY_SUBSCRIPTION"}, []string{"CLAUDE_CODE_OAUTH_TOKEN=selected-subscription"}, []string{"ambient-subscription", "ANTHROPIC_API_KEY=", "ANTHROPIC_BASE_URL=", "controller-secret", "MY_SUBSCRIPTION="}},
		{Authentication{Mode: "gateway", Endpoint: "https://explicit.invalid", CredentialEnv: "MY_API_KEY"}, []string{"ANTHROPIC_API_KEY=selected-api-key", "ANTHROPIC_BASE_URL=https://explicit.invalid"}, []string{"CLAUDE_CODE_OAUTH_TOKEN=", "ambient-api-key", "ambient.invalid", "controller-secret"}},
	} {
		env, err := claudeSDKEnvironment(tc.auth)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, "\n")
		for _, want := range tc.want {
			if !strings.Contains(joined, want) {
				t.Fatalf("selected credential route missing %s", strings.Split(want, "=")[0])
			}
		}
		for _, forbidden := range tc.forbidden {
			if strings.Contains(joined, forbidden) {
				t.Fatal("unselected credential or endpoint leaked")
			}
		}
	}
	t.Setenv("MY_SUBSCRIPTION", "")
	if _, err := claudeSDKEnvironment(Authentication{Mode: "native", CredentialEnv: "MY_SUBSCRIPTION"}); err == nil {
		t.Fatal("missing explicit token silently fell back to ambient authentication")
	}
}

func TestClaudeSDKNativeTokenLaunch(t *testing.T) {
	if os.Getenv("WORKFLOW_TEST_NATIVE_SDK_BRIDGE") == "1" {
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "fixture-subscription" || os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_BASE_URL") != "" || os.Getenv("GH_TOKEN") != "" || os.Getenv("SSH_AUTH_SOCK") != "" || os.Getenv("CLAUDE_CONFIG_DIR") == "/host-settings" || os.Getenv("HOME") != filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "home") {
			os.Exit(21)
		}
		encoder := json.NewEncoder(os.Stdout)
		_ = encoder.Encode(map[string]any{"version": BridgeProtocolVersion, "type": "hello", "provider_version": "claude-agent-sdk@" + ClaudeSDKVersion})
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			os.Exit(22)
		}
		var frame map[string]any
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame["type"] != "turn_start" {
			os.Exit(23)
		}
		_ = encoder.Encode(map[string]any{"version": BridgeProtocolVersion, "type": "result", "status": "succeeded", "session_id": "native-session", "outputs": map[string]any{"text": "native-ok"}})
		os.Exit(0)
	}
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(root, "bridge-launcher")
	script := "#!/bin/sh\nexport WORKFLOW_TEST_NATIVE_SDK_BRIDGE=1\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestClaudeSDKNativeTokenLaunch$\n"
	if err = os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	bridge := filepath.Join(root, "bridge.mjs")
	if err = os.WriteFile(bridge, nil, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json")
	if err = os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(metadata, []byte(`{"version":"`+ClaudeSDKVersion+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SUBSCRIPTION_TOKEN", "fixture-subscription")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "unselected-subscription")
	t.Setenv("ANTHROPIC_API_KEY", "unselected-api-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://unselected.invalid")
	t.Setenv("GH_TOKEN", "controller-secret")
	t.Setenv("SSH_AUTH_SOCK", "/controller-socket")
	t.Setenv("CLAUDE_CONFIG_DIR", "/host-settings")
	runner := Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"role": {Provider: "claude-sdk", Command: command, Bridge: bridge, Authentication: Authentication{Mode: "native", CredentialEnv: "TEST_SUBSCRIPTION_TOKEN"}}}}}
	if err = runner.Preflight(context.Background(), []string{"role"}); err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunTurn(context.Background(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "role", Prompt: "fixture"}}, nil)
	if err != nil || result.SessionID != "native-session" || result.Outputs["text"] != "native-ok" {
		t.Fatalf("native-token bridge launch failed: %+v %v", result, err)
	}
}
