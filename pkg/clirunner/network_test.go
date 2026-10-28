package clirunner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
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
func TestSelectedEndpointSurvivesProviderShellFilteringAndSnapshot(t *testing.T) {
	ref := &network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 2, Endpoint: network.Endpoint("shared", strings.Repeat("a", 32))}
	original := Config{Execution: Execution{NetworkRuntime: "shared", Network: ref}, Agents: map[string]Agent{"codex": {Provider: "codex-cli", Sandbox: "workspace-write", Execution: Execution{NetworkRuntime: "shared", Network: ref}}}}
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
	args := strings.Join(networkShellArgs(restored.Agents["codex"].Execution), " ")
	for _, key := range []string{"shell_environment_policy.set", "DOCKER_HOST", "CONTAINER_HOST", ref.Endpoint} {
		if !strings.Contains(args, key) {
			t.Fatal(args)
		}
	}
	if restored.Agents["codex"].Sandbox != "workspace-write" || strings.Contains(args, "approval") || strings.Contains(args, "sandbox") {
		t.Fatal("network policy changed provider authority")
	}
}
