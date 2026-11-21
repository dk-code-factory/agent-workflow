//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestControllerDeadlineOverrideRejectsInvalidScope(t *testing.T) {
	for _, args := range [][]string{
		{"controller", "--controller-run-timeout=-1s"},
		{"run", "--controller-run-timeout=2h", "workflows/main.yml"},
	} {
		var out bytes.Buffer
		if err := command(context.Background(), args, &out, &out); err == nil || !strings.Contains(err.Error(), "controller-run-timeout") {
			t.Fatalf("invalid override %v: %v", args, err)
		}
	}
}

func TestControllerDeadlineOverridePreservesRoleIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"workflows/main.yml":     "jobs: {task: {steps: [{uses: agent/run, with: {agent: fixture.toml, prompt: wait}}]}}\n",
		"workflows/fixture.toml": "name='fixture'\ndescription='Fixture'\ndeveloper_instructions='Wait for cancellation.'\n",
		"agents.yml":             "controller: {run-timeout: 30m}\nagents:\n  codex:\n    provider: codex-app-server\n    command: " + filepath.Join(root, "fake-codex") + "\n    authentication: {mode: api-key, credential-env: WORKFLOW_DEADLINE_FIXTURE_KEY}\n",
		"fake-codex": `#!/usr/bin/env python3
import json,sys,time
if '--version' in sys.argv: print('codex-cli 0.167.1'); sys.exit(0)
for line in sys.stdin:
    f=json.loads(line); method=f.get('method'); i=f.get('id')
    def send(x): print(json.dumps(x),flush=True)
    if method=='initialize': send({'id':i,'result':{'userAgent':'codex_cli_rs/0.167.1 (fixture)'}})
    elif method=='account/login/start': send({'id':i,'result':{}})
    elif method in ('thread/start','thread/resume'): send({'id':i,'result':{'thread':{'id':'fixture-thread'}}})
    elif method=='turn/start':
        send({'id':i,'result':{'turn':{'id':'turn-1'}}})
        time.sleep(60)
`,
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WORKFLOW_DEADLINE_FIXTURE_KEY", "local-fixture-only")
	cfg, err := clirunner.LoadConfig(filepath.Join(root, "agents.yml"))
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := json.Marshal(cfg)
	want := sha256.Sum256(roles)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out, diagnostics bytes.Buffer
	done := make(chan error, 1)
	go func() {
		// Allow the supervised version probe to finish under race instrumentation
		// before testing cancellation of the blocked turn.
		done <- command(ctx, []string{"controller", "--workspace", root, "--controller-run-timeout=3s"}, &out, &diagnostics)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("controller did not stop within the cleanup deadline")
		}
	})
	api := controller.NewClient(filepath.Join(root, ".agent-workflow"))
	var health struct {
		RolesHash string `json:"roles_hash"`
	}
	for api.Call(ctx, "GET", "/health", nil, &health) != nil {
		if ctx.Err() != nil {
			t.Fatal("controller did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if health.RolesHash != hex.EncodeToString(want[:]) {
		t.Fatal("deadline override changed the admitted role configuration")
	}
	var run workflows.Run
	if err := api.Call(ctx, "POST", "/runs", controller.StartRequest{Ref: "workflows/main.yml"}, &run); err != nil {
		t.Fatal(err)
	}
	for {
		if err := api.Call(ctx, "GET", "/runs/"+run.ID, nil, &run); err != nil {
			t.Fatal(err)
		}
		if run.Status != workflows.RunStatusQueued && run.Status != workflows.RunStatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(run.Error, "deadline exceeded") {
		t.Fatalf("controller did not enforce its override: %s %s", run.Status, run.Error)
	}
}
