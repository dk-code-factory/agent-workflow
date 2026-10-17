package clirunner

import (
	"context"
	"fmt"
	"github.com/dkropachev/agent-workflow/pkg/runtimeexec"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

func (r *Runner) Preflight(ctx context.Context, ids []string) error {
	if len(r.Config.ToolCatalog) > 0 {
		if err := r.VerifyToolCatalog(ctx); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		a, ok := r.Config.Agents[id]
		if !ok {
			return fmt.Errorf("agent %s is not configured", id)
		}
		if a.Provider == "codex-app-server" && (len(a.MCPGrants) > 0 || a.Tools != nil) {
			return fmt.Errorf("agent %s: Codex managed tool policy is not certified for this upstream version", id)
		}
		command := a.Command
		if command == "" {
			switch a.Provider {
			case "claude", "claude-cli":
				command = "claude"
			case "claude-sdk":
				command = "node"
			default:
				command = "codex"
			}
		}
		if err := a.Execution.policy().Validate(); err != nil {
			return err
		}
		if a.Execution.Mode == "container" {
			command = a.Execution.Runtime
			if command == "" {
				command = "podman"
			}
		}
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("agent %s backend: %w", id, err)
		}
		if a.Provider == "codex-app-server" && a.Execution.Mode != "container" {
			if err := verifyCodexCLI(ctx, command); err != nil {
				return fmt.Errorf("agent %s: %w", id, err)
			}
		}
		if a.Provider == "claude-sdk" && a.Execution.Mode != "container" {
			if _, err := os.Stat(a.Bridge); err != nil {
				return fmt.Errorf("agent %s bridge: %w", id, err)
			}
			if err := verifySDKBridge(a); err != nil {
				return fmt.Errorf("agent %s: %w", id, err)
			}
			u, err := url.Parse(a.Authentication.Endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
				return fmt.Errorf("agent %s: invalid credential gateway endpoint", id)
			}
		}
		if a.Provider == "codex-app-server" && a.Authentication.Mode == "native" {
			if _, err := nativeCodexCredentials(a); err != nil {
				return fmt.Errorf("agent %s: %w", id, err)
			}
		}
		if a.Authentication.Mode == "gateway" || a.Authentication.Mode == "api-key" {
			if value, ok := os.LookupEnv(a.Authentication.CredentialEnv); !ok || value == "" {
				return fmt.Errorf("agent %s credential reference is unavailable", id)
			}
		}
		if a.Execution.Mode == "container" && a.Authentication.Mode == "native" {
			if a.Authentication.CredentialFile == "" {
				return fmt.Errorf("agent %s native container authentication requires credential-file", id)
			}
			info, err := os.Stat(a.Authentication.CredentialFile)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("agent %s native credential reference unavailable", id)
			}
		}
		if a.Execution.Mode == "container" {
			probe, cancel := runtimeexec.ProbeContext(ctx)
			expected := map[string]string{}
			if a.Provider == "codex-app-server" {
				expected["agent-workflow.codex-cli"] = CodexProtocolVersion
			}
			err := runtimeexec.CheckImage(probe, a.Execution.policy(), a.Provider == "claude-sdk", expected)
			cancel()
			if err != nil {
				return fmt.Errorf("agent %s: %w", id, err)
			}
		}
		if a.Execution.Mode == "container" && !strings.Contains(a.Execution.Image, "@sha256:") && !strings.HasPrefix(a.Execution.Image, "sha256:") {
			return fmt.Errorf("agent %s container image must be pinned by digest", id)
		}
	}
	return nil
}
