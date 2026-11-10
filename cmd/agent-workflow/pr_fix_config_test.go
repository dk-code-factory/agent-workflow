//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/prfix"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestPRFixPreparedRunInheritsConfigurationAndHonorsOverride(t *testing.T) {
	for _, name := range []string{"inherited", "explicit override", "explicit missing"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			ref := "workflows/prepared.yml"
			if err := os.WriteFile(filepath.Join(root, ref), []byte("jobs: {gate: {steps: [{uses: input/wait, with: {prompt: fixture, evidence-id: config-proof}}]}}"), 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "nondefault.yml")
			if err := os.WriteFile(config, []byte("agents:\n  fixture:\n    provider: claude-cli\n    command: \"false\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := clirunner.LoadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			roles, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			savedConfig := config
			if name == "explicit override" {
				savedConfig = filepath.Join(root, "absent-saved.yml")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ledger, err := prfix.OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := prfix.ParseIdentity("https://github.com/owner/repo/pull/1")
			if err != nil {
				ledger.Close()
				t.Fatal(err)
			}
			plan := prfix.Plan{Version: prfix.Version, LogicalRun: strings.Repeat("a", 32), Identity: identity, ConfigPath: savedConfig, Roles: roles, Ref: ref, Fingerprint: strings.Repeat("b", 64), Selection: prfix.Selection{Config: prfix.Configuration{Options: map[string]any{"limits": prfix.Limits{ReviewCycles: 5, Retries: 1, CIRepairs: 5, CIReruns: 3}}}}}
			path, err := ledger.SavePlan(ctx, plan)
			ledger.Close()
			if err != nil {
				t.Fatal(err)
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			owner := &controller.Server{Dir: store.Dir, Store: store, Executor: &workflows.Executor{WorkspaceDir: root, Store: store, MaxParallel: 4, Roles: roles}}
			done := make(chan error, 1)
			go func() { done <- owner.Serve(ctx) }()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("fixture controller did not join")
				}
			}()
			api := controller.NewClient(store.Dir)
			for api.Call(ctx, "GET", "/health", nil, nil) != nil {
				if err := pause(ctx, time.Millisecond); err != nil {
					t.Fatal("fixture controller did not start", err)
				}
			}
			args := []string{"pr-fix", "run", "--workspace", root, "--prepared", path}
			if name == "explicit override" {
				args = append(args, "--config", "nondefault.yml")
			}
			if name == "explicit missing" {
				args = append(args, "--config", "absent-override.yml")
			}
			var out, stderr bytes.Buffer
			err = command(ctx, args, &out, &stderr)
			if name == "explicit missing" {
				if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "absent-override.yml") {
					t.Fatal("explicit config was replaced by saved config", err)
				}
				return
			}
			var run workflows.Run
			if !errors.Is(err, errInputWaiting) || json.Unmarshal(out.Bytes(), &run) != nil || run.Status != workflows.RunStatusWaiting {
				t.Fatal("prepared run did not reach its local workflow", out.String(), stderr.String(), err)
			}
		})
	}
}
