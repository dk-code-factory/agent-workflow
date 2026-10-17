package runtimeexec

import (
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
