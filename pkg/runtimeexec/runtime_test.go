package runtimeexec

import (
	"context"
	"encoding/json"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
	"os"
	"strings"
	"testing"
)

func TestStrictMacOSCleanupRequiresContainers(t *testing.T) {
	if err := (Policy{Cleanup: "strict"}).ValidateFor("darwin"); err == nil {
		t.Fatal("native strict cleanup advertised on macOS")
	}
	if err := (Policy{Cleanup: "process-group"}).ValidateFor("darwin"); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{Mode: "container", Image: "fixture", Cleanup: "strict"}).ValidateFor("darwin"); err != nil {
		t.Fatal(err)
	}
	if err := (Policy{Cleanup: "strict"}).ValidateFor("linux"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderHomeCannotChangeRuntimeIdentity(t *testing.T) {
	original := os.Getenv("HOME")
	env := runtimeEnvironment([]string{"HOME=/provider-private", "CODEX_HOME=/provider-codex", "ANTHROPIC_API_KEY=explicit-fixture", "EXTERNAL_MCP_TOKEN=must-not-forward"}, "")
	var home string
	for _, assignment := range env {
		key, value, _ := strings.Cut(assignment, "=")
		if key == "HOME" {
			home = value
		}
	}
	if home != original {
		t.Fatal("provider HOME changed host runtime store", home)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/provider-codex") || strings.Contains(joined, "must-not-forward") || !strings.Contains(joined, "explicit-fixture") {
		t.Fatal("host runtime environment forwarding is incorrect")
	}
}

func TestNetworkPolicyIsIndependentAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"", "local", "container"} {
		p := Policy{Mode: mode, NetworkRuntime: "team"}
		if mode == "container" {
			p.Image = "fixture"
		}
		if err := p.ValidateFor("linux"); err != nil {
			t.Fatal(mode, err)
		}
		if err := p.ValidateFor("darwin"); err == nil {
			t.Fatal("unsupported platform accepted")
		}
		p.Runtime = "docker"
		if err := p.ValidateFor("linux"); err == nil {
			t.Fatal("Docker containment accepted")
		}
	}
	if err := CheckPolicy(context.Background(), Policy{NetworkRuntime: "team"}); err == nil {
		t.Fatal("unresolved selection silently joined host")
	}
	p := Policy{NetworkRuntime: "team", Network: &network.Reference{Name: "team", Identity: strings.Repeat("a", 32), Generation: 7, Endpoint: network.Endpoint("team", strings.Repeat("a", 32))}}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var saved Policy
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if *saved.Network != *p.Network {
		t.Fatal("admitted runtime identity changed", saved)
	}
	// A missing retained generation cannot be resolved as another current runtime.
	if _, err = PinPolicy(context.Background(), saved); err == nil {
		t.Fatal("unavailable retained runtime accepted")
	}
	if _, err = Reconcile(context.Background(), Record{ID: NewID(), State: "launching", Network: p.Network}); err == nil {
		t.Fatal("unconfirmed runtime cleanup treated as absent")
	}
}
