package clirunner

import (
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
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
