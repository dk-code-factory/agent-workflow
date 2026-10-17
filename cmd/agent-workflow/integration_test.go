//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

func TestRunAndStatusWithBothCLIProcesses(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "..", "workflows", "parallel-review.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), source, 0600); err != nil {
		t.Fatal(err)
	}
	agents := map[string]clirunner.Agent{}
	for _, name := range []string{"claude", "codex"} {
		payload := `{"type":"result","subtype":"success","result":"claude result"}`
		if name == "codex" {
			payload = "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"codex result\"}}\n{\"type\":\"turn.completed\"}"
		}
		path := filepath.Join(root, "fake-"+name)
		body := "#!/bin/sh\ncat >/dev/null\ncat <<'PAYLOAD'\n" + payload + "\nPAYLOAD\n"
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		agents[name] = clirunner.Agent{Provider: name + "-cli", Command: path}
	}
	config, err := yaml.Marshal(clirunner.Config{Agents: agents})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents.yml"), config, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = command(context.Background(), []string{"run", "--foreground", "--workspace", root, "--parallel", "2", "--input", "task=example", "workflows/main.yml"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run failed: %v stderr=%s", err, stderr.String())
	}
	var run workflows.Run
	if err := json.Unmarshal(stdout.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != workflows.RunStatusSucceeded || run.Outputs["summary"] != "claude result" {
		t.Fatalf("bad run: %+v", run)
	}
	if run.Jobs["codex_review"].Outputs["findings"] != "codex result" {
		t.Fatal("Codex result lost")
	}
	stdout.Reset()
	if err := command(context.Background(), []string{"status", "--workspace", root, run.ID}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var saved workflows.Run
	if err := json.Unmarshal(stdout.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID != run.ID || saved.Status != run.Status {
		t.Fatalf("status mismatch: %+v", saved)
	}
}
