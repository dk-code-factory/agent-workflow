package clirunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerNetworkConfigurationInheritance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.yml")
	data := `execution: {mode: container, image: fixture, runtime: docker, network: bridge}
agents:
  inherited: {provider: codex-app-server, authentication: {mode: api-key, credential-env: OPENAI_API_KEY}}
  isolated: {provider: codex-app-server, authentication: {mode: api-key, credential-env: OPENAI_API_KEY}, execution: {network: none}}
  host: {provider: codex-cli, execution: {mode: local}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CommandExecutionPolicy().NetworkMode != "bridge" || cfg.Agents["inherited"].Execution.policy().NetworkMode != "bridge" || cfg.Agents["isolated"].Execution.policy().NetworkMode != "none" {
		t.Fatalf("network mode inheritance/overrides changed: %+v", cfg)
	}
	if cfg.Agents["host"].Execution.Mode != "local" || cfg.Agents["host"].Execution.NetworkMode != "" {
		t.Fatalf("local role inherited container policy: %+v", cfg.Agents["host"])
	}
}

func TestContainerNetworkCannotBypassGlobalManagedRuntime(t *testing.T) {
	for _, mode := range []string{"none", "bridge"} {
		path := filepath.Join(t.TempDir(), "agents.yml")
		data := "execution: {network-runtime: team}\nagents:\n  codex: {provider: codex-app-server, execution: {mode: container, image: fixture, network: " + mode + "}}\n"
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "cannot override network-runtime") {
			t.Fatalf("managed route overridden with %q: %v", mode, err)
		}
	}
}
