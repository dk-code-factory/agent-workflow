package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func doctorFixtureCommand(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func unavailableDoctorNetwork() *network.Reference {
	const name = "doctor-test-missing"
	identity := strings.Repeat("e", 32)
	return &network.Reference{Name: name, Identity: identity, Generation: 987654321, Endpoint: network.Endpoint(name, identity)}
}

func TestDoctorPinsWholeConfigurationBeforeExecutableEffects(t *testing.T) {
	for _, scope := range []string{"global", "agent", "retained"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "effects")
			t.Setenv("DOCTOR_EFFECTS", marker)
			command := doctorFixtureCommand(t, root, "fake-provider", `echo called >> "$DOCTOR_EFFECTS"`)
			// A command-container diagnostic and an unrelated local agent must not
			// run before every selected agent/runtime has passed admission.
			doctorFixtureCommand(t, root, "podman", `echo called >> "$DOCTOR_EFFECTS"`)
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := clirunner.Config{Execution: clirunner.Execution{Mode: "container", Image: "fixture@sha256:" + strings.Repeat("a", 64)}, Agents: map[string]clirunner.Agent{
				"local":    {Provider: "codex-cli", Command: command},
				"selected": {Provider: "codex-cli", Command: command},
			}}
			if scope == "global" {
				cfg.Execution.NetworkRuntime = unavailableDoctorNetwork().Name
			} else {
				a := cfg.Agents["selected"]
				a.Execution.NetworkRuntime = unavailableDoctorNetwork().Name
				if scope == "retained" {
					a.Execution.Network = unavailableDoctorNetwork()
				}
				cfg.Agents["selected"] = a
			}
			var out bytes.Buffer
			if err := doctor(context.Background(), root, cfg, &out); err == nil {
				t.Fatal("unavailable selection admitted")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("configured executable ran before pinning: %v", err)
			}
			var report map[string]any
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || report["execution_error"] == nil {
				t.Fatalf("missing admission diagnostic: %s (%v)", out.String(), err)
			}
		})
	}
}

func TestDoctorSelectedProbesRefuseHostFallback(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "effects")
	t.Setenv("DOCTOR_EFFECTS", marker)
	command := doctorFixtureCommand(t, root, "podman", `echo called >> "$DOCTOR_EFFECTS"`)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, ref := range []*network.Reference{nil, unavailableDoctorNetwork()} {
		policy := runtimeexec.Policy{NetworkRuntime: unavailableDoctorNetwork().Name, Network: ref}
		for _, args := range [][]string{{"--version"}, {"auth", "status", "--json"}, {"login", "status"}} {
			if _, err := doctorCommand(context.Background(), root, policy, command, args...); err == nil {
				t.Fatal("selected executable fell back to host")
			}
		}
		policy.Mode, policy.Image = "container", "fixture"
		for _, args := range [][]string{{"version", "--format", "{{.Server.Version}}"}, {"image", "inspect", "--format", "{{.Id}}", "fixture"}} {
			if _, err := doctorContainerCommand(context.Background(), root, policy, args...); err == nil {
				t.Fatal("selected metadata fell back to host")
			}
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host runtime executed: %v", err)
	}
}

func TestDoctorUnselectedConfiguredVersionAndCachedAuth(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "effects")
	t.Setenv("DOCTOR_EFFECTS", marker)
	t.Setenv("DOCTOR_TEST_KEY", "doctor-secret")
	provider := doctorFixtureCommand(t, root, "fake-provider", `
 echo "$*" >> "$DOCTOR_EFFECTS"
 case "$*" in
 --version) echo "fake-provider $DOCTOR_TEST_KEY";;
 'auth status --json') echo '{"loggedIn":true}';;
 'login status') exit 0;;
 *) exit 3;;
 esac
 `)
	doctorFixtureCommand(t, root, "git", `echo host-controller`)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := clirunner.Config{Agents: map[string]clirunner.Agent{
		"codex":  {Provider: "codex-cli", Command: provider, Authentication: clirunner.Authentication{Mode: "native", CredentialEnv: "DOCTOR_TEST_KEY"}},
		"claude": {Provider: "claude-cli", Command: provider, Authentication: clirunner.Authentication{Mode: "native", CredentialEnv: "DOCTOR_TEST_KEY"}},
	}}
	var out bytes.Buffer
	if err := doctor(context.Background(), root, cfg, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "doctor-secret") {
		t.Fatal("doctor report leaked credential")
	}
	var report struct {
		GitHead  string `json:"git_head"`
		Writable bool   `json:"state_write_permission"`
		Backends map[string]struct {
			Version  string `json:"version"`
			LoggedIn bool   `json:"native_cached_login"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.GitHead != "host-controller" || !report.Writable {
		t.Fatalf("controller checks missing: %+v", report)
	}
	for _, id := range []string{"codex", "claude"} {
		if !report.Backends[id].LoggedIn || !strings.Contains(report.Backends[id].Version, "fake-provider") {
			t.Fatalf("%s: %+v", id, report.Backends[id])
		}
	}
	calls, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "--version") != 2 || !strings.Contains(string(calls), "auth status --json") || !strings.Contains(string(calls), "login status") {
		t.Fatalf("missing cached probes: %s", calls)
	}
}

func TestDoctorNativeSDKChecksTokenWithoutCLIAuthenticationOrGateway(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCTOR_SUBSCRIPTION", "subscription-secret")
	provider := doctorFixtureCommand(t, root, "fake-node", `
case "$*" in
--version) echo 'fixture-node';;
*) exit 43;;
esac
`)
	doctorFixtureCommand(t, root, "git", `echo fixture-head`)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	bridge := filepath.Join(root, "bridge.mjs")
	if err := os.WriteFile(bridge, nil, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"version":"`+clirunner.ClaudeSDKVersion+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := clirunner.Config{Agents: map[string]clirunner.Agent{"sdk": {Provider: "claude-sdk", Command: provider, Bridge: bridge, Authentication: clirunner.Authentication{Mode: "native", CredentialEnv: "DOCTOR_SUBSCRIPTION"}}}}
	var out bytes.Buffer
	if err := doctor(context.Background(), root, cfg, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "subscription-secret") {
		t.Fatal("doctor leaked token")
	}
	var report struct {
		Backends map[string]struct {
			NativeToken string `json:"native_token_authentication"`
		} `json:"backends"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Backends["sdk"].NativeToken != "available; model acceptance unverified" {
		t.Fatal("doctor did not distinguish token availability from live acceptance")
	}
}

func TestDoctorUnselectedContainerDiagnostics(t *testing.T) {
	root := t.TempDir()
	doctorFixtureCommand(t, root, "docker", `
 case "$*" in
 'version --format {{.Server.Version}}') test "$PWD" = "$DOCTOR_ROOT" || exit 4; echo 5.0;;
 'image inspect --format {{.Id}} fixture') echo sha256:fixture;;
 *) exit 3;;
 esac
 `)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCTOR_ROOT", root)
	policy := runtimeexec.Policy{Mode: "container", Runtime: "docker", Image: "fixture"}
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"version", "--format", "{{.Server.Version}}"}, "5.0"}, {[]string{"image", "inspect", "--format", "{{.Id}}", "fixture"}, "sha256:fixture"}} {
		got, err := doctorContainerCommand(context.Background(), root, policy, tc.args...)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %q %v", tc.args, got, err)
		}
	}
}
