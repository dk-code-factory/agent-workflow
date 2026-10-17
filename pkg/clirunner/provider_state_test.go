package clirunner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func TestProviderStatePartitionsOriginsAndWorkerSessions(t *testing.T) {
	root := t.TempDir()
	request := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: "11111111111111111111111111111111", InvocationID: "first-turn", ParentWorkerID: "stable-worker", AgentID: "role"}}
	one, err := providerStateDir(root, "claude", request)
	if err != nil {
		t.Fatal(err)
	}
	request.InvocationID = "second-turn"
	two, err := providerStateDir(root, "claude", request)
	if err != nil || one != two {
		t.Fatal("worker lost its session state owner", one, two, err)
	}
	request.RunID = "22222222222222222222222222222222"
	other, err := providerStateDir(root, "claude", request)
	if err != nil || one == other {
		t.Fatal("origin state was shared", other, err)
	}
	request.RunID = "../../caller-files"
	confined, err := providerStateDir(root, "codex", request)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(root, ".agent-workflow", "providers"), confined)
	if err != nil || rel == ".." {
		t.Fatal("provider state escaped its ownership tree")
	}
}

func TestNativeCodexReferencePreservesProviderOwnedAuthentication(t *testing.T) {
	provider := t.TempDir()
	authFile := filepath.Join(provider, "auth.json")
	initial := []byte(`{"tokens":{"access_token":"fixture-access","account_id":"fixture-account"}}`)
	if err := os.WriteFile(authFile, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(provider, "config.toml"), []byte("user_configuration_must_not_be_copied=true"), 0600); err != nil {
		t.Fatal(err)
	}
	a := Agent{Authentication: Authentication{Mode: "native", CredentialFile: authFile}}
	state, err := providerStateDir(t.TempDir(), "codex", workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: "11111111111111111111111111111111", InvocationID: "owned-step"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = referenceNativeCodexAuth(state, a); err != nil {
		t.Fatal(err)
	}
	if err = referenceNativeCodexAuth(state, a); err != nil {
		t.Fatal("reference is not idempotent", err)
	}
	if _, err = os.Stat(filepath.Join(state, "config.toml")); !os.IsNotExist(err) {
		t.Fatal("native configuration copied into writable provider state")
	}
	// Simulate the pinned provider's ordinary file-store update, with fake
	// fixture credentials only. No login or authentication request is performed.
	updated := []byte(`{"tokens":{"access_token":"fixture-updated","account_id":"fixture-account"}}`)
	if err = os.WriteFile(filepath.Join(state, "auth.json"), updated, 0600); err != nil {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(authFile); err != nil || string(bytes) != string(updated) {
		t.Fatal("native auth reference became an independent credential copy", err)
	}
	if err = os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(authFile); err != nil || string(bytes) != string(updated) {
		t.Fatal("engine cleanup removed provider-owned authentication", err)
	}
}

func TestNativeCodexCredentialsIncludeEveryTokenForRedaction(t *testing.T) {
	for _, api := range []string{"", "fixture-api-key"} {
		path := filepath.Join(t.TempDir(), "auth.json")
		raw := `{"OPENAI_API_KEY":"` + api + `","tokens":{"access_token":"fixture-access","account_id":"fixture-account","refresh_token":"fixture-refresh","id_token":"fixture-id"}}`
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		values, err := nativeCodexCredentials(Agent{Authentication: Authentication{CredentialFile: path}})
		if err != nil {
			t.Fatal(err)
		}
		if values["refresh_token"] != "fixture-refresh" || values["id_token"] != "fixture-id" {
			t.Fatal("native token omitted from redaction inputs")
		}
	}
}
