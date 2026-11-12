package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseStartPreservesJSONInputs(t *testing.T) {
	for _, raw := range []string{
		`9007199254740993`, `1e400`, `1.234567890123456789`,
		`{"nested":[9007199254740993,1e400]}`, `true`, `null`, `"text"`,
	} {
		t.Run(raw, func(t *testing.T) {
			req, err := parseStart("workflows/main.yml", values{"value=" + raw}, nil)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(req.Inputs["value"])
			if err != nil || string(encoded) != raw {
				t.Fatalf("input changed: got %s, want %s (error %v)", encoded, raw, err)
			}
		})
	}
	for _, raw := range []string{"plain text", "123 trailing", "123 456", ""} {
		req, err := parseStart("workflows/main.yml", values{"value=" + raw}, nil)
		if err != nil || req.Inputs["value"] != raw {
			t.Fatalf("non-JSON input %q changed: %+v, %v", raw, req, err)
		}
	}
}

func TestValidateCommandChecksWorkflowAndAgentBindings(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents.yml"), []byte("agents:\n  codex:\n    provider: codex-app-server\n    authentication: {mode: native}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "codex.toml"), []byte("name='codex'\ndescription='Fixture'\ndeveloper_instructions='Execute fixture.'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workflows", "main.yml")
	if err := os.WriteFile(path, []byte("jobs:\n  review:\n    steps:\n      - uses: agent/run\n        with: {prompt: hello, agent: codex.toml}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/main.yml"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Valid: workflows/main.yml\n" {
		t.Fatal(stdout.String())
	}
	if err := os.WriteFile(path, []byte("jobs:\n  review:\n    steps:\n      - uses: agent/run\n        with: {prompt: hello, agent: missing.toml}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/main.yml"}, &stdout, &stderr); err == nil {
		t.Fatal("accepted missing agent binding")
	}
}

func TestInitScaffoldsBoundDefinitionsAndPreservesExistingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "workflows", "classify.yml")
	if err := os.WriteFile(existing, []byte("user definition\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := command(context.Background(), []string{"init", "--workspace", root}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "user definition\n" {
		t.Fatal("existing definition overwritten")
	}
	stdout.Reset()
	if err := command(context.Background(), []string{"validate", "--workspace", root, "workflows/develop.yml"}, &stdout, &stderr); err != nil {
		t.Fatal("scaffold has invalid agent bindings", err)
	}
	config := filepath.Join(root, "agents.yml")
	original, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = command(context.Background(), []string{"init", "--workspace", root}, &stdout, &stderr); err == nil {
		t.Fatal("existing configuration overwritten")
	}
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("configuration changed on duplicate init")
	}
}
