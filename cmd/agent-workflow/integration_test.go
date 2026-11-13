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

func TestRunAndStatusWithNativeCodexTasks(t *testing.T) {
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
	for _, name := range []string{"review_correctness", "review_security", "task"} {
		path := filepath.Join(root, "workflows", "agents", name+".toml")
		os.MkdirAll(filepath.Dir(path), 0700)
		if err := os.WriteFile(path, []byte("name='"+name+"'\ndescription='Fixture'\ndeveloper_instructions='Execute fixture.'\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "fake-codex")
	fake := `#!/usr/bin/env python3
import json,sys
if '--version' in sys.argv: print('codex-cli 0.167.0'); sys.exit(0)
for line in sys.stdin:
    f=json.loads(line); method=f.get('method'); i=f.get('id')
    def send(x): print(json.dumps(x),flush=True)
    if method=='initialize': send({'id':i,'result':{'userAgent':'codex_cli_rs/0.167.0 (fixture)'}})
    elif method in ('account/login/start','turn/interrupt'): send({'id':i,'result':{}})
    elif method in ('thread/start','thread/resume'): send({'id':i,'result':{'thread':{'id':'fixture-thread'}}})
    elif method=='turn/start':
        send({'id':i,'result':{'turn':{'id':'turn-1'}}})
        send({'method':'item/completed','params':{'threadId':'fixture-thread','turnId':'turn-1','item':{'type':'agentMessage','text':'fixture result'}}})
        send({'method':'turn/completed','params':{'threadId':'fixture-thread','turn':{'id':'turn-1','status':'completed'}}})
`
	if err := os.WriteFile(path, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOW_FIXTURE_CODEX_KEY", "fixture-local-key")
	agents := map[string]clirunner.Agent{"codex": {Provider: "codex-app-server", Command: path, Authentication: clirunner.Authentication{Mode: "api-key", CredentialEnv: "WORKFLOW_FIXTURE_CODEX_KEY"}}}
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
	if run.Status != workflows.RunStatusSucceeded || run.Outputs["summary"] != "fixture result" {
		t.Fatalf("bad run: %+v", run)
	}
	if run.Jobs["security_review"].Outputs["findings"] != "fixture result" {
		t.Fatal("Security review result lost")
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
