//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package clirunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func script(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubprocessPromptModelWorkspaceAndFlags(t *testing.T) {
	for _, provider := range []string{"claude-cli", "codex-cli"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			payload := `{"type":"result","result":"ok"}`
			if provider == "codex-cli" {
				payload = "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}\n{\"type\":\"turn.completed\"}"
			}
			command := script(t, root, "pwd > cwd\nprintf '%s\\n' \"$@\" > args\ncat > prompt\ncat <<'PAYLOAD'\n"+payload+"\nPAYLOAD\n")
			runner := Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"test": {Provider: provider, Command: command, Model: "default-model"}}}}
			out, err := runner.RunAgent(context.Background(), workflows.AgentRequest{AgentID: "test", Model: "chosen-model", Prompt: "hello\nworld"})
			if err != nil || out["text"] != "ok" {
				t.Fatalf("%+v %v", out, err)
			}
			data, _ := os.ReadFile(filepath.Join(root, "prompt"))
			if string(data) != "hello\nworld" {
				t.Fatalf("wrong prompt %q", data)
			}
			data, _ = os.ReadFile(filepath.Join(root, "cwd"))
			if strings.TrimSpace(string(data)) != root {
				t.Fatalf("wrong cwd %q", data)
			}
			data, _ = os.ReadFile(filepath.Join(root, "args"))
			args := string(data)
			if !strings.Contains(args, "chosen-model") || strings.Contains(args, "dangerously") {
				t.Fatalf("bad flags %q", args)
			}
			if provider == "codex-cli" && !strings.Contains(args, "read-only") {
				t.Fatalf("missing sandbox: %s", args)
			}
			if provider == "claude-cli" && !strings.Contains(args, "dontAsk") {
				t.Fatalf("missing permission mode: %s", args)
			}
		})
	}
}

func TestSubprocessCancellationAndNonzeroExit(t *testing.T) {
	root := t.TempDir()
	command := script(t, root, "cat >/dev/null\nsleep 30 &\nwait\n")
	runner := Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"test": {Provider: "claude", Command: command}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runner.RunAgent(ctx, workflows.AgentRequest{AgentID: "test", Prompt: "test"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("subprocess descendants kept output pipes alive")
	}
	script(t, root, "cat >/dev/null\necho '{\"type\":\"result\",\"result\":\"partial\"}'\nexit 1\n")
	if _, err := runner.RunAgent(context.Background(), workflows.AgentRequest{AgentID: "test", Prompt: "test"}); err == nil {
		t.Fatal("accepted nonzero exit with partial output")
	}
}
