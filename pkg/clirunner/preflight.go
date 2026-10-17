package clirunner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

func (r *Runner) Preflight(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		a, ok := r.Config.Agents[id]
		if !ok {
			return fmt.Errorf("agent %s is not configured", id)
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
		if a.Execution.Mode == "container" {
			command = a.Execution.Runtime
			if command == "" {
				command = "podman"
			}
		}
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("agent %s backend: %w", id, err)
		}
		if a.Provider == "claude-sdk" {
			if _, err := os.Stat(a.Bridge); err != nil {
				return fmt.Errorf("agent %s bridge: %w", id, err)
			}
			u, err := url.Parse(a.Authentication.Endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
				return fmt.Errorf("agent %s: invalid credential gateway endpoint", id)
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
		if a.Execution.Mode == "container" && !strings.Contains(a.Execution.Image, "@sha256:") && !strings.HasPrefix(a.Execution.Image, "sha256:") {
			return fmt.Errorf("agent %s container image must be pinned by digest", id)
		}
	}
	return nil
}
