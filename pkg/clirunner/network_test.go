package clirunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestGlobalNetworkSelectionCannotBeOverriddenByLocalRole(t *testing.T) {
	for _, role := range []string{"", "    execution: {mode: local}\n", "    execution: {network-runtime: shared}\n"} {
		path := filepath.Join(t.TempDir(), "agents.yml")
		data := "execution: {network-runtime: shared}\nagents:\n  test:\n    provider: codex-cli\n    sandbox: read-only\n" + role
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Agents["test"].Execution.NetworkRuntime != "shared" || cfg.Agents["test"].Sandbox != "read-only" {
			t.Fatal(cfg)
		}
	}
	path := filepath.Join(t.TempDir(), "agents.yml")
	_ = os.WriteFile(path, []byte("execution: {network-runtime: shared}\nagents:\n  test:\n    provider: codex-cli\n    execution: {mode: local, network-runtime: other}\n"), 0600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("global selection overridden")
	}
}
func TestSelectedReferenceSurvivesSnapshot(t *testing.T) {
	ref := &network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 2, Endpoint: network.Endpoint("shared", strings.Repeat("a", 32))}
	original := Config{Execution: Execution{NetworkRuntime: "shared", Network: ref}, Agents: map[string]Agent{"codex": {Provider: "codex-cli", Sandbox: "workspace-write", Args: []string{"--profile", "native", "--config", `shell_environment_policy.inherit="none"`}, Execution: Execution{NetworkRuntime: "shared", Network: ref}}}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := (&Runner{Workspace: t.TempDir()}).RunnerForRoles(data)
	if err != nil {
		t.Fatal(err)
	}
	restored := runner.(*Runner).Config
	if *restored.Execution.Network != *ref || *restored.Agents["codex"].Execution.Network != *ref {
		t.Fatal(restored)
	}
	if restored.Agents["codex"].Sandbox != "workspace-write" || !reflect.DeepEqual(restored.Agents["codex"].Args, original.Agents["codex"].Args) {
		t.Fatal("snapshot changed provider authority")
	}
}

func TestCodexCLIForwardsOriginalArgumentsAndSandbox(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "argv")
	binary := filepath.Join(root, "provider")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf 'codex 0.164.0\\n'; exit 0; fi\nprintf '%s\\0' \"$@\" > '" + strings.ReplaceAll(output, "'", "'\\''") + "'\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"fixture\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":0}}'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []string{"--profile", "native profile", "--config", `shell_environment_policy={inherit="none",include_only=["PATH"]}`, "--config=approval_policy=\"on-request\""}
	runner := &Runner{Workspace: t.TempDir(), Config: Config{Agents: map[string]Agent{"fixture": {Provider: "codex-cli", Command: binary, Args: raw, Sandbox: "workspace-write"}}}}
	if _, err := runner.RunAgent(context.Background(), workflows.AgentRequest{AgentID: "fixture", Prompt: "fixture"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := append([]string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--color", "never", "--sandbox", "workspace-write"}, raw...)
	want = append(want, "-")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider arguments changed: got %q want %q", got, want)
	}
}
