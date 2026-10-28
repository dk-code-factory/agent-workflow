package clirunner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func TestGlobalNetworkSelectionCannotBeOverriddenByLocalRole(t *testing.T) {
	for _, role := range []string{"", "    execution: {mode: local}\n", "    execution: {network-runtime: shared}\n"} {
		path := filepath.Join(t.TempDir(), "agents.yml")
		data := "execution: {network-runtime: shared}\nagents:\n  test:\n    provider: codex-cli\n    sandbox: read-only\n" + role
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Agents["test"].Execution.NetworkRuntime != "shared" || cfg.Agents["test"].Sandbox != "read-only" {
			t.Fatal(cfg)
		}
	}
	path := filepath.Join(t.TempDir(), "agents.yml")
	_ = os.WriteFile(path, []byte("execution: {network-runtime: shared}\nagents:\n  test:\n    provider: codex-cli\n    execution: {mode: local, network-runtime: other}\n"), 0600)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("global selection overridden")
	}
}
func TestSelectedEndpointSurvivesProviderShellFilteringAndSnapshot(t *testing.T) {
	ref := &network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 2, Endpoint: network.Endpoint("shared", strings.Repeat("a", 32))}
	original := Config{Execution: Execution{NetworkRuntime: "shared", Network: ref}, Agents: map[string]Agent{"codex": {Provider: "codex-cli", Sandbox: "workspace-write", Execution: Execution{NetworkRuntime: "shared", Network: ref}}}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := (&Runner{Workspace: t.TempDir()}).RunnerForRoles(data)
	if err != nil {
		t.Fatal(err)
	}
	restored := runner.(*Runner).Config
	if *restored.Execution.Network != *ref || *restored.Agents["codex"].Execution.Network != *ref {
		t.Fatal(restored)
	}
	args := strings.Join(networkShellArgs(restored.Agents["codex"].Execution), " ")
	for _, key := range []string{"shell_environment_policy.set", "DOCKER_HOST", "CONTAINER_HOST", ref.Endpoint} {
		if !strings.Contains(args, key) {
			t.Fatal(args)
		}
	}
	if restored.Agents["codex"].Sandbox != "workspace-write" || strings.Contains(args, "approval") || strings.Contains(args, "sandbox") {
		t.Fatal("network policy changed provider authority")
	}
}

func TestSelectedEndpointWinsEffectiveCodexShellConfiguration(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 with tomllib is required for the independent fake-provider config parser")
	}
	if err := exec.Command(python, "-c", "import tomllib").Run(); err != nil {
		t.Skip("Python tomllib unavailable")
	}
	ref := &network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 2, Endpoint: network.Endpoint("shared", strings.Repeat("a", 32))}
	// This fake provider parses actual TOML, applies Codex's ordered dotted-path
	// overrides, and launches a child with the resulting shell environment.
	// The ordering follows upstream config/src/overrides.rs and the filter order
	// follows protocol/src/shell_environment.rs (also checked with Codex 0.164.0
	// config/read without auth or a model request).
	const fake = `
import fnmatch, json, os, subprocess, sys, tomllib
config = {}
args = iter(sys.argv[1:])
for arg in args:
    if arg in ("-c", "--config"):
        override = next(args)
    elif arg.startswith("--config="):
        override = arg[len("--config="):]
    else:
        continue
    key, value = override.split("=", 1)
    value = tomllib.loads("value=" + value)["value"]
    table = config
    parts = key.strip().split(".")
    for part in parts[:-1]:
        if not isinstance(table.get(part), dict):
            table[part] = {}
        table = table[part]
    table[parts[-1]] = value
policy = config["shell_environment_policy"]
env = {} if policy.get("inherit") == "none" else dict(os.environ)
for name in list(env):
    if any(fnmatch.fnmatchcase(name.upper(), p.upper()) for p in policy.get("exclude", [])):
        del env[name]
env.update(policy.get("set", {}))
if policy.get("include_only"):
    env = {k:v for k,v in env.items() if any(fnmatch.fnmatchcase(k.upper(), p.upper()) for p in policy["include_only"])}
child = subprocess.check_output([sys.executable, "-c", "import os,json; print(json.dumps(dict(os.environ)))"], env=env, text=True)
print(json.dumps({"config":config,"environment":json.loads(child)}))
`
	cases := [][]string{
		{"--config", `shell_environment_policy.set={DOCKER_HOST="unix:///host.sock",CONTAINER_HOST="unix:///host.sock",AGENT_WORKFLOW_NETWORK_RUNTIME="host",KEEP="explicit"}`, "-c", `shell_environment_policy.inherit="none"`, "-c", `shell_environment_policy.exclude=["*HOST*","KEEP"]`},
		{"--config=" + `shell_environment_policy={inherit="none",set={DOCKER_HOST="unix:///host.sock",KEEP="explicit"},exclude=["*HOST*","KEEP"]}`, "-c", `shell_environment_policy.set.CONTAINER_HOST="unix:///other.sock"`},
	}
	for i, raw := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			raw = append(raw, "-c", `approval_policy="on-request"`, "-c", `sandbox_mode="read-only"`)
			agent := Agent{Provider: "codex-cli", Args: raw, Execution: Execution{NetworkRuntime: ref.Name, Network: ref}}
			cmd := exec.Command(python, append([]string{"-c", fake}, providerArgs(agent)...)...)
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fake provider: %v: %s", err, data)
			}
			var result struct {
				Config struct {
					Shell struct {
						Inherit string            `json:"inherit"`
						Exclude []string          `json:"exclude"`
						Set     map[string]string `json:"set"`
					} `json:"shell_environment_policy"`
					Approval string `json:"approval_policy"`
					Sandbox  string `json:"sandbox_mode"`
				} `json:"config"`
				Environment map[string]string `json:"environment"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			for k, want := range map[string]string{"DOCKER_HOST": ref.Endpoint, "CONTAINER_HOST": ref.Endpoint, "AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name, "KEEP": "explicit"} {
				if result.Environment[k] != want || result.Config.Shell.Set[k] != want {
					t.Fatalf("effective %s: config=%q child=%q want=%q", k, result.Config.Shell.Set[k], result.Environment[k], want)
				}
			}
			if result.Config.Shell.Inherit != "none" || !reflect.DeepEqual(result.Config.Shell.Exclude, []string{"*HOST*", "KEEP"}) || result.Config.Approval != "on-request" || result.Config.Sandbox != "read-only" {
				t.Fatalf("unrelated provider policy changed: %+v", result.Config)
			}
		})
	}
}

func TestProviderArgsPreserveUnselectedAndContainerConfiguration(t *testing.T) {
	raw := []string{"--config", `shell_environment_policy.set={KEEP="explicit",DOCKER_HOST="unix:///explicit.sock"}`, "--sandbox", "workspace-write"}
	ref := &network.Reference{Name: "shared", Endpoint: "unix:///selected.sock"}
	for _, agent := range []Agent{
		{Provider: "codex-cli", Args: raw},
		{Provider: "codex-cli", Args: raw, Execution: Execution{Mode: "container", Network: ref}},
		{Provider: "claude-cli", Args: raw, Execution: Execution{Network: ref}},
	} {
		if got := providerArgs(agent); !reflect.DeepEqual(got, raw) {
			t.Fatalf("explicit args changed: %v", got)
		}
	}
}
