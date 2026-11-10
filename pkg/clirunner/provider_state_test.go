package clirunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestNativeTurnRetainsProviderStateFilesystemFailure(t *testing.T) {
	for _, provider := range []string{"claude-sdk", "codex-app-server"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".agent-workflow"), 0700); err != nil {
				t.Fatal(err)
			}
			obstruction := filepath.Join(root, ".agent-workflow", "providers")
			if err := os.WriteFile(obstruction, []byte("obstructed"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FIXTURE_CLAUDE_STATE_TOKEN", "fixture-only")
			runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {
				Provider: provider, Command: filepath.Join(root, "must-not-start"),
				Authentication: Authentication{Mode: "native", CredentialEnv: "FIXTURE_CLAUDE_STATE_TOKEN"},
			}}}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{
				AgentID: "a", RunID: "run", InvocationID: "turn", ParentWorkerID: "worker",
			}}, nil)
			var storageErr *runtimeexec.StorageError
			var pathErr *os.PathError
			if !errors.As(err, &storageErr) || !errors.As(err, &pathErr) || pathErr.Op != "mkdir" {
				t.Fatal("provider state failure lost its storage or filesystem identity", err)
			}
			if storageErr.Error() != pathErr.Error() {
				t.Fatal("provider state storage marker changed its diagnostic", err)
			}
		})
	}
}

func TestProviderStatePartitionsConversationSessions(t *testing.T) {
	root := t.TempDir()
	request := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: "11111111111111111111111111111111", InvocationID: "first-turn", SessionOwnerID: "conversation-one-builder", AgentID: "builder"}}
	first, err := providerStateDir(root, "claude", request)
	if err != nil {
		t.Fatal(err)
	}
	request.InvocationID = "second-turn"
	resumed, err := providerStateDir(root, "claude", request)
	if err != nil || first != resumed {
		t.Fatal("conversation lost provider session state", first, resumed, err)
	}
	for _, owner := range []string{"conversation-one-reviewer", "conversation-two-builder"} {
		request.SessionOwnerID = owner
		other, err := providerStateDir(root, "claude", request)
		if err != nil || first == other {
			t.Fatal("participant state shared", owner, other, err)
		}
	}
	request.SessionOwnerID = "conversation-one-builder"
	request.RunID = "22222222222222222222222222222222"
	other, err := providerStateDir(root, "claude", request)
	if err != nil || first == other {
		t.Fatal("origin state shared", other, err)
	}
}

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

func TestNativeCodexReferenceRetainsStateFilesystemFailure(t *testing.T) {
	for _, operation := range []string{"lstat", "symlink"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			authFile := filepath.Join(root, "auth.json")
			raw := []byte(`{"OPENAI_API_KEY":"fixture-key"}`)
			if err := os.WriteFile(authFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(root, "state")
			if operation == "lstat" {
				if err := os.WriteFile(state, []byte("obstructed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := referenceNativeCodexAuth(state, Agent{Authentication: Authentication{Mode: "native", CredentialFile: authFile}})
			var storageErr *runtimeexec.StorageError
			if !errors.As(err, &storageErr) {
				t.Fatal("authentication reference lost its state filesystem identity", err)
			}
			var cause error
			if operation == "lstat" {
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) || pathErr.Op != operation {
					t.Fatal("state lookup lost its filesystem cause", err)
				}
				cause = pathErr
			} else {
				var linkErr *os.LinkError
				if !errors.As(err, &linkErr) || linkErr.Op != operation {
					t.Fatal("state reference lost its filesystem cause", err)
				}
				cause = linkErr
			}
			if storageErr.Error() != cause.Error() || workflows.FailureClass(err) != workflows.FailureClass(cause) {
				t.Fatal("state reference storage marker changed its diagnostic", err)
			}
			if current, err := os.ReadFile(authFile); err != nil || string(current) != string(raw) {
				t.Fatal("state preparation changed provider-owned authentication", err)
			}
		})
	}
}

func TestNativeCodexReferenceRetainsDisappearingSourceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(`{"OPENAI_API_KEY":"fixture-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	agent := Agent{Authentication: Authentication{Mode: "native", CredentialFile: path}}
	if _, err := nativeCodexCredentials(agent); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := resolveNativeCodexAuthSource(agent)
	var storageErr *runtimeexec.StorageError
	var pathErr *os.PathError
	if !errors.As(err, &storageErr) || !errors.As(err, &pathErr) || pathErr.Op != "lstat" || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disappearing authentication source lost its filesystem admission identity", err)
	}
	if storageErr.Error() != pathErr.Error() || workflows.FailureClass(err) != workflows.FailureClass(pathErr) {
		t.Fatal("authentication source marker changed the diagnostic", err)
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
