package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/clirunner"
	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func doctorCommand(ctx context.Context, root, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = root
	var output doctorOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	cleanup, err := process.Supervise(cmd)
	if err != nil {
		return "", err
	}
	defer cleanup()
	err = cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return strings.TrimSpace(output.text.String()), err
}

type doctorOutput struct{ text strings.Builder }

func (b *doctorOutput) Write(p []byte) (int, error) {
	n := len(p)
	space := (1 << 20) - b.text.Len()
	if len(p) > space {
		p = p[:space]
	}
	_, _ = b.text.Write(p)
	return n, nil
}
func doctor(ctx context.Context, root string, cfg clirunner.Config, out io.Writer) error {
	runner := &clirunner.Runner{Config: cfg, Workspace: root}
	secrets := runner.SecretValues()
	report := map[string]any{"workspace": root, "backends": map[string]any{}, "native_authentication": "cached status only; actual model acceptance requires a separate budgeted live probe"}
	backends := report["backends"].(map[string]any)
	var failures error
	for id, a := range cfg.Agents {
		item := map[string]any{"provider": a.Provider, "authentication_route": a.Authentication.Mode}
		backends[id] = item
		caps, err := runner.Capabilities(id)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		item["capabilities"] = caps
		if err = runner.Preflight(ctx, []string{id}); err != nil {
			item["error"] = workflows.Redact(err.Error(), secrets)
			failures = errors.Join(failures, err)
			continue
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
			runtime := a.Execution.Runtime
			if runtime == "" {
				runtime = "podman"
			}
			version, err := doctorCommand(ctx, root, runtime, "version", "--format", "{{.Server.Version}}")
			if err != nil {
				item["error"] = "container runtime unavailable"
				failures = errors.Join(failures, fmt.Errorf("agent %s container runtime: %w", id, err))
				continue
			}
			item["container_runtime_version"] = version
			image, err := doctorCommand(ctx, root, runtime, "image", "inspect", "--format", "{{.Id}}", a.Execution.Image)
			if err != nil {
				item["error"] = "pinned image unavailable"
				failures = errors.Join(failures, fmt.Errorf("agent %s image: %w", id, err))
				continue
			}
			item["image_id"] = image
		} else {
			version, err := doctorCommand(ctx, root, command, "--version")
			if err != nil {
				failures = errors.Join(failures, fmt.Errorf("agent %s version: %w", id, err))
				item["error"] = "version probe failed"
				continue
			}
			if len(version) > 512 {
				version = version[:512]
			}
			item["version"] = workflows.Redact(version, secrets)
		}
		if a.Authentication.Mode == "gateway" {
			endpoint := strings.TrimRight(a.Authentication.Endpoint, "/")
			if strings.HasSuffix(endpoint, "/v1") {
				endpoint += "/models"
			} else {
				endpoint += "/v1/models"
			}
			req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
			if err != nil {
				failures = errors.Join(failures, err)
				continue
			}
			credential := os.Getenv(a.Authentication.CredentialEnv)
			req.Header.Set("x-api-key", credential)
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err != nil {
				item["gateway"] = "unreachable"
				failures = errors.Join(failures, fmt.Errorf("agent %s gateway unreachable", id))
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			item["gateway_http_status"] = resp.StatusCode
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				failures = errors.Join(failures, fmt.Errorf("agent %s gateway models endpoint returned HTTP %d", id, resp.StatusCode))
			}
		} else if a.Execution.Mode != "container" && (a.Provider == "claude" || a.Provider == "claude-cli") {
			status, err := doctorCommand(ctx, root, command, "auth", "status", "--json")
			var auth struct {
				LoggedIn bool `json:"loggedIn"`
			}
			parseErr := json.Unmarshal([]byte(status), &auth)
			item["native_cached_login"] = err == nil && parseErr == nil && auth.LoggedIn
			if err != nil || parseErr != nil || !auth.LoggedIn {
				failures = errors.Join(failures, fmt.Errorf("agent %s native authentication status unavailable", id))
			}
		} else if a.Execution.Mode != "container" && (a.Authentication.Mode == "native" || a.Provider == "codex" || a.Provider == "codex-cli") {
			_, err := doctorCommand(ctx, root, command, "login", "status")
			item["native_cached_login"] = err == nil
			if err != nil {
				failures = errors.Join(failures, fmt.Errorf("agent %s native authentication status unavailable", id))
			}
		}
	}
	head, err := doctorCommand(ctx, root, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		failures = errors.Join(failures, fmt.Errorf("Git baseline: %w", err))
	}
	report["git_head"] = head
	dir := root + string(os.PathSeparator) + ".agent-workflow"
	if err = os.MkdirAll(dir, 0700); err != nil {
		failures = errors.Join(failures, err)
	} else {
		f, err := os.CreateTemp(dir, "doctor-permission-*")
		if err != nil {
			failures = errors.Join(failures, err)
		} else {
			_ = f.Close()
			_ = os.Remove(f.Name())
			report["state_write_permission"] = true
		}
	}
	if err = json.NewEncoder(out).Encode(report); err != nil {
		return errors.Join(failures, err)
	}
	return failures
}
