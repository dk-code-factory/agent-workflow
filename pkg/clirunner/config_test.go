package clirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
