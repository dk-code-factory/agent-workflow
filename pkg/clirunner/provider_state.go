package clirunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func stateComponent(value string) string {
	if len(value) == 32 && strings.Trim(value, "0123456789abcdef") == "" {
		return value
	}
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:16])
}
func providerStateDir(root, provider string, req workflows.SessionRequest) (string, error) {
	origin := req.RunID
	owner := req.ParentWorkerID
	if origin == "" {
		origin = "unowned"
		owner = req.AgentID
	} else {
		origin = stateComponent(origin)
		if owner == "" {
			owner = req.InvocationID
		}
		if owner == "" {
			return "", fmt.Errorf("managed provider state requires an invocation owner")
		}
	}
	path := filepath.Join(root, ".agent-workflow", "providers", provider, origin, stateComponent(owner))
	if err := os.MkdirAll(filepath.Join(path, "home"), 0700); err != nil {
		return "", err
	}
	return path, nil
}

func nativeCodexCredentialFile(a Agent) (string, error) {
	if a.Authentication.CredentialFile != "" {
		return a.Authentication.CredentialFile, nil
	}
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "auth.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// Read provider-owned file authentication without copying native configuration,
// memory, plugins or transcripts into the engine-owned writable CODEX_HOME.
// Keychain-only native authentication is an explicit unsupported prerequisite.
func nativeCodexCredentials(a Agent) (map[string]any, error) {
	path, err := nativeCodexCredentialFile(a)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("native Codex file authentication unavailable; provide authentication.credential-file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("native Codex authentication file is invalid")
	}
	var auth struct {
		APIKey string `json:"OPENAI_API_KEY"`
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	if err = json.NewDecoder(f).Decode(&auth); err != nil {
		return nil, fmt.Errorf("native Codex authentication document is invalid")
	}
	if auth.APIKey != "" {
		return map[string]any{"api_key": auth.APIKey, "access_token": auth.Tokens.AccessToken, "account_id": auth.Tokens.AccountID, "refresh_token": auth.Tokens.RefreshToken, "id_token": auth.Tokens.IDToken}, nil
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.AccountID == "" {
		return nil, fmt.Errorf("native Codex file authentication has no supported token/account identity")
	}
	return map[string]any{"access_token": auth.Tokens.AccessToken, "account_id": auth.Tokens.AccountID, "refresh_token": auth.Tokens.RefreshToken, "id_token": auth.Tokens.IDToken}, nil
}

func referenceNativeCodexAuth(state string, a Agent) error {
	if _, err := nativeCodexCredentials(a); err != nil {
		return err
	}
	if a.Execution.Mode == "container" {
		return nil
	} // launcher mounts the explicit file read-only
	source, err := nativeCodexCredentialFile(a)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	target := filepath.Join(state, "auth.json")
	if _, err = os.Lstat(target); err == nil {
		link, linkErr := os.Readlink(target)
		if linkErr != nil || link != source {
			return fmt.Errorf("provider state has a conflicting native authentication reference")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(source, target)
}
