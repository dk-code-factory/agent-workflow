package clirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestConfigRejectsAdmittedNetworkReferenceAsCredential(t *testing.T) {
	for _, mode := range []string{"local", "container"} {
		t.Run(mode, func(t *testing.T) {
			execution := Execution{Mode: mode}
			if mode == "container" {
				execution.Image = "fixture"
			}
			agent := Agent{Provider: "codex-app-server", Execution: execution, Authentication: Authentication{Mode: "api-key", CredentialEnv: "OPENAI_API_KEY"}}
			cfg := Config{Agents: map[string]Agent{"fixture": agent}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("ordinary provider credential rejected: %v", err)
			}
			agent.Authentication.CredentialEnv = network.ReferenceEnvironment
			cfg.Agents["fixture"] = agent
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "controller credential/environment reference cannot be forwarded") {
				t.Fatalf("admitted runtime reference accepted as provider credential: %v", err)
			}
		})
	}
}

func TestNativeRuntimeSelectorAdmissionAndLegacyRoles(t *testing.T) {
	cfg := Config{Agents: map[string]Agent{"codex": {Provider: "codex-app-server"}, "selected": {Provider: "codex-app-server"}}}
	legacy, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("AgentRuntime")) {
		t.Fatal("legacy selector must remain omitted")
	}
	current := &Runner{Config: Config{AgentRuntime: "selected"}}
	restored, err := current.RunnerForRoles(legacy)
	if err != nil {
		t.Fatal(err)
	}
	runner := restored.(*Runner)
	if runner.NativeTaskRole() != "codex" {
		t.Fatal("legacy Roles followed current selector")
	}
	roundtrip, err := json.Marshal(runner.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacy, roundtrip) {
		t.Fatal("legacy Roles representation changed")
	}
	for _, selected := range []string{"", "selected"} {
		cfg.AgentRuntime = selected
		fresh, err := (&Runner{Config: cfg}).AdmittedRoles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var admitted Config
		if err := json.Unmarshal(fresh, &admitted); err != nil {
			t.Fatal(err)
		}
		if admitted.NativeTaskAdmission != 1 {
			t.Fatal("fresh output admission policy not pinned")
		}
		if admitted.AgentRuntime != cfg.NativeRole() {
			t.Fatalf("selector not pinned: %q", admitted.AgentRuntime)
		}
		if cfg.AgentRuntime != selected {
			t.Fatal("fresh admission mutated source config")
		}
	}
}

func TestConfigRejectsInvalidNativeRuntimeSelector(t *testing.T) {
	for _, selected := range []string{"missing", "legacy"} {
		cfg := Config{AgentRuntime: selected, Agents: map[string]Agent{"legacy": {Provider: "claude"}}}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "agent-runtime") {
			t.Fatalf("selector accepted: %s: %v", selected, err)
		}
	}
}

func TestNativeOutputPolicyRetainsLegacyAdmission(t *testing.T) {
	contract := &workflows.AgentOutputContract{Format: "json", RepairAttempts: 1}
	legacy := &Runner{Config: Config{Agents: map[string]Agent{"codex": {Provider: "codex-app-server"}}}}
	if err := legacy.ValidateNativeOutput(contract); err != nil {
		t.Fatalf("legacy admission changed: %v", err)
	}
	roles, err := legacy.AdmittedRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := legacy.RunnerForRoles(roles)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.(*Runner).ValidateNativeOutput(contract); err == nil {
		t.Fatal("restored fresh admission lost unsupported repair denial")
	}
}

func TestLoadConfigNativeRuntimeSelector(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-conversation-only", true: "selected-claude"}[explicit], func(t *testing.T) {
			root := t.TempDir()
			source := "agents:\n  task:\n    provider: claude-sdk\n    bridge: bridge.mjs\n    authentication: {mode: native, credential-env: LOCAL_TOKEN}\n"
			if explicit {
				source = "agent-runtime: task\n" + source
			}
			path := filepath.Join(root, "agents.yml")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := "codex"
			if explicit {
				expected = "task"
			}
			if cfg.NativeRole() != expected {
				t.Fatalf("unexpected native role: %q", cfg.NativeRole())
			}
		})
	}
}

func TestOmittedRuntimeKeepsUnusedCodexRoleConfiguration(t *testing.T) {
	cfg := Config{Agents: map[string]Agent{"codex": {Provider: "codex-app-server", Tools: []string{}}, "conversation": {Provider: "claude-cli"}}}
	if _, err := (&Runner{Config: cfg}).AdmittedRoles(context.Background()); err != nil {
		t.Fatalf("unused legacy Codex role rejected before workflow admission: %v", err)
	}
	if _, err := (&Runner{Config: cfg}).AdmitNativeTask(&workflows.AgentDefinition{Name: "fixture"}); err == nil {
		t.Fatal("default native role lost capability restriction")
	}
}

// Use the same public configuration loader as the CLI, with an exact engine
// grant that a native task policy must remove from the invocation ceiling.
func nativePolicyFixtureConfig(t *testing.T, root, bridge, endpoint, grants string) string {
	t.Helper()
	source := fmt.Sprintf(`agent-runtime: native
agents:
  native:
    provider: claude-sdk
    bridge: %q
    model: claude-sonnet-4-6
    tools: [Read, Write, WebFetch]
    mcp-grants: %s
    authentication: {mode: gateway, endpoint: %q, credential-env: READONLY_DENIAL_KEY}
`, bridge, grants, endpoint)
	path := filepath.Join(root, "agents.yml")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigNativePolicyRole(t *testing.T) {
	for _, variant := range []struct{ name, grants, rejection string }{
		{"exact-engine-grant", "[{server: engine, tool: worker_status}]", ""},
		{"duplicate-grant", "[{server: engine, tool: worker_status}, {server: engine, tool: worker_status}]", "unique exact MCP"},
		{"wildcard-grant", "[{server: engine, tool: '*'}]", "unique exact MCP"},
		{"unknown-server", "[{server: missing, tool: worker_status}]", "unknown MCP server"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			root := t.TempDir()
			// Admission and rejection need metadata only; the gated gateway test
			// separately runs the actual pinned SDK and maintained bridge.
			metadata := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk/package.json")
			if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metadata, []byte(`{"version":"0.3.322"}`), 0600); err != nil {
				t.Fatal(err)
			}
			bridge := filepath.Join(root, "bridge.mjs")
			cfg, err := LoadConfig(nativePolicyFixtureConfig(t, root, bridge, "http://127.0.0.1:1", variant.grants))
			if variant.rejection != "" {
				if err == nil || !strings.Contains(err.Error(), variant.rejection) {
					t.Fatal("invalid MCP configuration accepted or misclassified", err)
				}
				return
			}
			if err != nil {
				t.Fatal("policy-capable role rejected before task admission", err)
			}
			role := cfg.Agents["native"]
			if strings.Join(role.Tools, ",") != "Read,Write,WebFetch" || len(role.MCPGrants) != 1 || role.MCPGrants[0] != (workflows.ToolGrant{Server: "engine", Tool: "worker_status"}) {
				t.Fatal("configuration silently narrowed the role before task admission", role)
			}
			runner := &Runner{Workspace: root, Config: cfg}
			admitted, err := runner.AdmitNativeTask(&workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only"})
			if err != nil || admitted.NativePolicy == nil || strings.Join(admitted.NativePolicy.NativeTools, ",") != "Read" {
				t.Fatal("loaded role lost read-only task admission", admitted, err)
			}
			probe := &nativePolicyBrokerProbe{}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err = runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "native", Tools: &workflows.InvocationTools{Binding: probe}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "direct native tools with managed broker") || probe.calls.Load() != 0 {
				t.Fatal("loaded configuration admitted genuine mixed execution", err, probe.calls.Load())
			}
		})
	}
}
