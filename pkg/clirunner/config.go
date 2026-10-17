package clirunner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/development"
	"gopkg.in/yaml.v3"
)

var roleIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

type Execution struct {
	runID        string
	invocationID string
	ownerRoot    string
	Mode         string `yaml:"mode"`
	Image        string `yaml:"image"`
	Runtime      string `yaml:"runtime"`
	Writable     bool   `yaml:"writable"`
}

type Authentication struct {
	CredentialFile string `yaml:"credential-file"`
	Mode           string `yaml:"mode"`
	Endpoint       string `yaml:"endpoint"`
	CredentialEnv  string `yaml:"credential-env"`
}

type Limits struct {
	MaxWorkers  int    `yaml:"max-workers"`
	MaxDepth    int    `yaml:"max-depth"`
	MaxQueued   int    `yaml:"max-queued-messages"`
	MaxTurns    int    `yaml:"max-turns"`
	TurnTimeout string `yaml:"turn-timeout"`
	MaxOutput   int    `yaml:"max-output-bytes"`
}

type Agent struct {
	Limits          Limits         `yaml:"limits"`
	Execution       Execution      `yaml:"execution"`
	Instructions    []string       `yaml:"instructions"`
	InstructionText string         `yaml:"-"`
	Tools           []string       `yaml:"tools,omitempty"`
	Authentication  Authentication `yaml:"authentication"`
	Bridge          string         `yaml:"bridge"`
	Provider        string         `yaml:"provider"`
	Model           string         `yaml:"model"`
	Command         string         `yaml:"command"`
	Args            []string       `yaml:"args"`
	Workspace       string         `yaml:"workspace"`
	Sandbox         string         `yaml:"sandbox"`
	PermissionMode  string         `yaml:"permission-mode"`
}

type Config struct {
	Publication development.PublicationPolicy `yaml:"publication"`
	Agents      map[string]Agent              `yaml:"agents"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	if len(data) > 1<<20 {
		return Config{}, fmt.Errorf("config exceeds 1 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	var cfg Config
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("expected one config document")
	}
	for id, agent := range cfg.Agents {
		for _, resource := range agent.Instructions {
			if !filepath.IsAbs(resource) {
				resource = filepath.Join(filepath.Dir(path), resource)
			}
			b, err := os.ReadFile(resource)
			if err != nil {
				return cfg, fmt.Errorf("agent %s instructions: %w", id, err)
			}
			if len(b) > 1<<20 {
				return cfg, fmt.Errorf("instruction resource exceeds 1 MiB")
			}
			agent.InstructionText += string(b) + "\n"
		}
		if agent.Bridge != "" && !filepath.IsAbs(agent.Bridge) {
			agent.Bridge = filepath.Join(filepath.Dir(path), agent.Bridge)
		}
		if agent.Authentication.CredentialFile != "" && !filepath.IsAbs(agent.Authentication.CredentialFile) {
			agent.Authentication.CredentialFile = filepath.Join(filepath.Dir(path), agent.Authentication.CredentialFile)
		}
		cfg.Agents[id] = agent
		if agent.Workspace != "" && !filepath.IsAbs(agent.Workspace) {
			agent.Workspace = filepath.Join(filepath.Dir(path), agent.Workspace)
			cfg.Agents[id] = agent
		}
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	for id, agent := range c.Agents {
		if !roleIDPattern.MatchString(id) {
			return fmt.Errorf("invalid role id")
		}
		if agent.Limits.MaxWorkers < 0 || agent.Limits.MaxDepth < 0 || agent.Limits.MaxQueued < 0 || agent.Limits.MaxTurns < 0 || agent.Limits.MaxOutput < 0 {
			return fmt.Errorf("agent %s: negative limits are invalid", id)
		}
		if agent.Limits.TurnTimeout != "" {
			d, err := time.ParseDuration(agent.Limits.TurnTimeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("agent %s: turn-timeout must be positive", id)
			}
		}
		if agent.Authentication.CredentialEnv == "GH_TOKEN" || agent.Authentication.CredentialEnv == "GITHUB_TOKEN" || agent.Authentication.CredentialEnv == "SSH_AUTH_SOCK" || agent.Authentication.CredentialEnv == "DOCKER_HOST" {
			return fmt.Errorf("agent %s: controller credential/environment reference cannot be forwarded to a provider", id)
		}
		if agent.Execution.Mode != "" && agent.Execution.Mode != "local" && agent.Execution.Mode != "container" {
			return fmt.Errorf("agent %s: invalid execution mode", id)
		}
		if agent.Execution.Mode == "container" {
			if agent.Provider != "claude-sdk" && agent.Provider != "codex-app-server" {
				return fmt.Errorf("agent %s: container execution requires a session backend", id)
			}
			if agent.Execution.Image == "" {
				return fmt.Errorf("agent %s: container image required", id)
			}
			if agent.Execution.Runtime != "" && agent.Execution.Runtime != "podman" && agent.Execution.Runtime != "docker" {
				return fmt.Errorf("agent %s: unsupported container runtime", id)
			}
		}
		legacy := agent.Provider == "claude" || agent.Provider == "claude-cli" || agent.Provider == "codex" || agent.Provider == "codex-cli"
		if legacy {
			if len(agent.Instructions) > 0 || agent.InstructionText != "" || agent.Tools != nil || agent.Bridge != "" {
				return fmt.Errorf("agent %s: managed instructions/tools/bridge require a session backend", id)
			}
			if agent.Authentication.Mode != "" && agent.Authentication.Mode != "native" || agent.Authentication.Endpoint != "" || agent.Authentication.CredentialEnv != "" || agent.Authentication.CredentialFile != "" {
				return fmt.Errorf("agent %s: native CLI authentication is provider-owned; use a session backend for explicit credential routes", id)
			}
		}
		if agent.Execution.Mode != "container" && (agent.Execution.Image != "" || agent.Execution.Runtime != "" || agent.Execution.Writable) {
			return fmt.Errorf("agent %s: container parameters require execution.mode: container", id)
		}
		switch agent.Provider {
		case "claude", "claude-cli":
			if agent.Sandbox != "" {
				return fmt.Errorf("agent %s: sandbox is a Codex option", id)
			}
			if agent.PermissionMode != "" && agent.PermissionMode != "dontAsk" && agent.PermissionMode != "acceptEdits" && agent.PermissionMode != "plan" {
				return fmt.Errorf("agent %s: unsupported Claude permission mode", id)
			}
		case "codex", "codex-cli", "codex-app-server":
			if agent.Provider == "codex-app-server" {
				if agent.Authentication.Mode != "native" && agent.Authentication.Mode != "api-key" {
					return fmt.Errorf("agent %s: explicit native or api-key authentication required", id)
				}
				if agent.Authentication.Mode == "api-key" && agent.Authentication.CredentialEnv == "" {
					return fmt.Errorf("agent %s: credential-env required", id)
				}
				if agent.Authentication.Endpoint != "" {
					return fmt.Errorf("agent %s: app-server custom endpoint is unsupported", id)
				}
			}
			if agent.PermissionMode != "" {
				return fmt.Errorf("agent %s: permission-mode is a Claude option", id)
			}
			if agent.Sandbox != "" && agent.Sandbox != "read-only" && agent.Sandbox != "workspace-write" {
				return fmt.Errorf("agent %s: unsupported Codex sandbox", id)
			}
		case "claude-sdk":
			for _, tool := range agent.Tools {
				normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(tool))
				switch normalized {
				case "agent", "task", "skill", "toolsearch", "teamcreate", "teamdelete", "sendmessage":
					return fmt.Errorf("agent %s: native delegation/discovery tools are disabled", id)
				}
			}
			if agent.Sandbox != "" || agent.PermissionMode != "" || len(agent.Args) > 0 {
				return fmt.Errorf("agent %s: SDK roles use explicit tools/container policy rather than CLI flags", id)
			}
			if agent.Bridge == "" {
				return fmt.Errorf("agent %s: claude-sdk requires pinned bridge path", id)
			}
			if agent.Authentication.Mode != "gateway" || agent.Authentication.Endpoint == "" || agent.Authentication.CredentialEnv == "" {
				return fmt.Errorf("agent %s: claude-sdk requires explicit gateway endpoint and credential-env", id)
			}
		default:
			return fmt.Errorf("agent %s: unknown provider %q", id, agent.Provider)
		}
	}
	return nil
}
