package clirunner

import (
	"fmt"
	"os"
)

// Native SDK authentication selects an explicit subscription-token reference;
// it does not load host settings, login caches, plugins or discovery sources.
func claudeSDKCredentialReference(auth Authentication) (string, error) {
	switch auth.Mode {
	case "native":
		if auth.Endpoint != "" || auth.CredentialFile != "" {
			return "", fmt.Errorf("native Claude SDK authentication uses a token environment reference, without endpoint or credential-file")
		}
		if auth.CredentialEnv == "" {
			return "CLAUDE_CODE_OAUTH_TOKEN", nil
		}
	case "gateway":
		if auth.Endpoint == "" || auth.CredentialEnv == "" {
			return "", fmt.Errorf("Claude SDK gateway authentication requires endpoint and credential-env")
		}
		if auth.CredentialFile != "" {
			return "", fmt.Errorf("Claude SDK gateway authentication uses credential-env, not credential-file")
		}
	default:
		return "", fmt.Errorf("claude-sdk requires explicit native or gateway authentication")
	}
	return auth.CredentialEnv, nil
}

func claudeSDKEnvironment(auth Authentication) ([]string, error) {
	reference, err := claudeSDKCredentialReference(auth)
	if err != nil {
		return nil, err
	}
	credential, ok := os.LookupEnv(reference)
	if !ok || credential == "" {
		return nil, fmt.Errorf("Claude SDK credential reference is unavailable")
	}
	env := providerEnvironment("")
	if auth.Mode == "native" {
		return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+credential), nil
	}
	return append(env, "ANTHROPIC_BASE_URL="+auth.Endpoint, "ANTHROPIC_API_KEY="+credential), nil
}
