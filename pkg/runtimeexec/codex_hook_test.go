package runtimeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

func hookFixture() (network.Reference, map[string]string, network.Status) {
	ref := network.Reference{Name: "shared", Identity: strings.Repeat("a", 32), Generation: 7}
	ref.Endpoint = network.Endpoint(ref.Name, ref.Identity)
	data, _ := json.Marshal(ref)
	return ref, map[string]string{"AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name, network.ReferenceEnvironment: string(data), "DOCKER_HOST": "unix:///stale-host", "CONTAINER_HOST": "unix:///stale-host"}, network.Status{Reference: ref, Healthy: true, NetworkNamespace: "net:[1234]"}
}

func hookInput(command string) []byte {
	data, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": command, "timeout": 4567, "description": "private fixture", "nested": map[string]any{"flag": false}}})
	return data
}

func hookResult(t *testing.T, data []byte) (string, map[string]json.RawMessage) {
	t.Helper()
	if len(data) > codexHookLimit || !json.Valid(data) {
		t.Fatalf("invalid or oversized hook output (%d bytes)", len(data))
	}
	var out struct {
		Specific struct {
			Event    string                     `json:"hookEventName"`
			Decision string                     `json:"permissionDecision"`
			Input    map[string]json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Specific.Event != "PreToolUse" {
		t.Fatalf("unexpected response: %s", data)
	}
	return out.Specific.Decision, out.Specific.Input
}

func TestCodexHookRepairsPublicSelectorsPreservingInputAndExit(t *testing.T) {
	ref, env, status := hookFixture()
	for _, shellCommand := range []string{"printf '%s\\n' \"$DOCKER_HOST\" \"$CONTAINER_HOST\" \"$AGENT_WORKFLOW_NETWORK_RUNTIME\"; exit 23", "# original bytes\nprintf '%s' 'quotes\"$`'\n", ""} {
		input := hookInput(shellCommand)
		out := runCodexHook(context.Background(), bytes.NewReader(input), func(k string) string { return env[k] }, func(_ context.Context, got network.Reference) (network.Status, error) {
			if got != ref {
				t.Fatal("resolved a different reference")
			}
			return status, nil
		}, func() (string, error) { return status.NetworkNamespace, nil })
		decision, updated := hookResult(t, out)
		if decision != "allow" {
			t.Fatal(string(out))
		}
		var event struct {
			Input map[string]json.RawMessage `json:"tool_input"`
		}
		_ = json.Unmarshal(input, &event)
		var rewritten string
		_ = json.Unmarshal(updated["command"], &rewritten)
		delete(updated, "command")
		delete(event.Input, "command")
		if !reflect.DeepEqual(updated, event.Input) || !strings.HasSuffix(rewritten, "\n"+shellCommand) || strings.Contains(rewritten, network.ReferenceEnvironment) {
			t.Fatalf("changed options/command: %s", out)
		}
		if !strings.Contains(shellCommand, "exit 23") {
			continue
		}
		cmd := exec.Command("/bin/sh", "-c", rewritten)
		cmd.Env = []string{"DOCKER_HOST=stale", "CONTAINER_HOST=stale"}
		data, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 || string(data) != ref.Endpoint+"\n"+ref.Endpoint+"\n"+ref.Name+"\n" {
			t.Fatalf("exit/env: %q %v", data, err)
		}
		// A preexisting readonly routing variable makes export fail before the
		// original command is evaluated in the same native shell.
		cmd = exec.Command("/bin/sh", "-c", "readonly DOCKER_HOST=stale\n"+rewritten)
		data, err = cmd.CombinedOutput()
		if err == nil || strings.Contains(string(data), ref.Endpoint) {
			t.Fatalf("export failure ran original command: %s %v", data, err)
		}
	}
}

func TestCodexHookNormalFailuresDenyWithoutDiagnostics(t *testing.T) {
	ref, env, healthy := hookFixture()
	cases := []struct {
		name         string
		env          map[string]string
		input        []byte
		status       network.Status
		err          error
		namespace    string
		namespaceErr error
	}{
		{name: "partial-name", env: map[string]string{"AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name}},
		{name: "partial-ref", env: map[string]string{network.ReferenceEnvironment: env[network.ReferenceEnvironment]}},
		{name: "malformed-ref", env: map[string]string{"AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name, network.ReferenceEnvironment: "private-invalid"}},
		{name: "unknown-ref-field", env: map[string]string{"AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name, network.ReferenceEnvironment: strings.TrimSuffix(env[network.ReferenceEnvironment], "}") + `,"other":"private"}`}},
		{name: "oversized-ref", env: map[string]string{"AGENT_WORKFLOW_NETWORK_RUNTIME": ref.Name, network.ReferenceEnvironment: strings.Repeat("p", 1025)}},
		{name: "invalid-utf8", input: append(hookInput("private"), 0xff)},
		{name: "malformed-input", input: []byte(`{"private":"broken"`)},
		{name: "trailing-input", input: append(hookInput("private"), []byte(`{}`)...)},
		{name: "oversized-input", input: []byte(strings.Repeat("p", codexHookLimit+1))},
		{name: "missing-command", input: []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`)},
		{name: "null-command", input: []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":null}}`)},
		{name: "nul-command", input: hookInput("private\x00command")},
		{name: "wrong-event", input: []byte(`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"private"}}`)},
		{name: "service-error", err: errors.New("private service diagnostic")},
		{name: "unhealthy", status: network.Status{Reference: ref, NetworkNamespace: healthy.NetworkNamespace}},
		{name: "replacement", status: network.Status{Reference: network.Reference{Name: ref.Name, Generation: 8}, Healthy: true, NetworkNamespace: healthy.NetworkNamespace}},
		{name: "missing-namespace", status: network.Status{Reference: ref, Healthy: true}},
		{name: "namespace-spoof", namespace: "net:[9999]"},
		{name: "namespace-error", namespaceErr: errors.New("private namespace error")},
		{name: "output-bound", input: hookInput(strings.Repeat("x", codexHookLimit-220))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := tc.env
			if selected == nil {
				selected = env
			}
			input := tc.input
			if input == nil {
				input = hookInput("private command")
			}
			status := tc.status
			if status.Reference.Name == "" {
				status = healthy
			}
			ns := tc.namespace
			if ns == "" {
				ns = healthy.NetworkNamespace
			}
			out := runCodexHook(context.Background(), bytes.NewReader(input), func(k string) string { return selected[k] }, func(context.Context, network.Reference) (network.Status, error) { return status, tc.err }, func() (string, error) { return ns, tc.namespaceErr })
			decision, updated := hookResult(t, out)
			if decision != "deny" || updated != nil || bytes.Contains(out, []byte("private")) {
				t.Fatalf("unsafe denial: %s", out)
			}
		})
	}
}

type hookReadError struct{}

func (hookReadError) Read([]byte) (int, error) { return 0, errors.New("private read error") }

func TestCodexHookOutsideContextAndReadTimeoutErrors(t *testing.T) {
	ref, env, status := hookFixture()
	unreachable := func(context.Context, network.Reference) (network.Status, error) {
		t.Fatal("unexpected service call")
		return network.Status{}, nil
	}
	out := runCodexHook(context.Background(), hookReadError{}, func(k string) string {
		if k == "DOCKER_HOST" {
			return "unix:///ordinary-host"
		}
		return ""
	}, unreachable, func() (string, error) { t.Fatal("unexpected namespace read"); return "", nil })
	if string(out) != "{}" {
		t.Fatal(string(out))
	}
	out = runCodexHook(context.Background(), hookReadError{}, func(k string) string { return env[k] }, unreachable, nil)
	if decision, _ := hookResult(t, out); decision != "deny" {
		t.Fatal(string(out))
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer readCancel()
	out = runCodexHook(readCtx, reader, func(k string) string { return env[k] }, unreachable, nil)
	if decision, _ := hookResult(t, out); decision != "deny" {
		t.Fatal(string(out))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	out = runCodexHook(ctx, bytes.NewReader(hookInput("private")), func(k string) string { return env[k] }, func(ctx context.Context, got network.Reference) (network.Status, error) {
		if got != ref {
			t.Fatal(got)
		}
		<-ctx.Done()
		return status, nil
	}, nil)
	if decision, _ := hookResult(t, out); decision != "deny" {
		t.Fatal(string(out))
	}
}

func TestCodexHookInternalReexecAndRestrictedContainerEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], codexHookMode)
	cmd.Env = []string{"AGENT_WORKFLOW_NETWORK_RUNTIME=selected", network.ReferenceEnvironment + "=private-malformed"}
	cmd.Stdin = strings.NewReader("private input")
	data, err := cmd.CombinedOutput()
	if err != nil || !bytes.HasPrefix(data, []byte(codexHookHeader)) {
		t.Fatalf("internal reexec: %s %v", data, err)
	}
	if decision, _ := hookResult(t, bytes.TrimPrefix(data, []byte(codexHookHeader))); decision != "deny" {
		t.Fatal(string(data))
	}
	ref, _, _ := hookFixture()
	cmd = exec.Command("codex", "--profile", "native")
	cmd.Env = network.Environment([]string{"OPENAI_API_KEY=fixture"}, ref)
	args, err := BuildContainer(cmd, Policy{Mode: "container", Image: "fixture", NetworkRuntime: ref.Name, Network: &ref}, ContainerOptions{Workspace: t.TempDir(), State: t.TempDir()}, containerapi.Owner{Name: "fixture"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if strings.Contains(arg, network.ReferenceEnvironment) || strings.Contains(arg, ref.Endpoint) || strings.Contains(arg, "control.sock") || strings.Contains(arg, network.Registry) {
			t.Fatalf("runtime authority forwarded to restricted container: %q", arg)
		}
	}
}

var _ io.Reader = hookReadError{}
