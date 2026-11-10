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

func TestPRFixPreparedConfigurationMatchesControllerAdmission(t *testing.T) {
	for _, grants := range []bool{false, true} {
		name := "empty catalog"
		if grants {
			name = "granted catalog"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			ref := "workflows/prepared.yml"
			if err := os.WriteFile(filepath.Join(root, ref), []byte("jobs: {gate: {steps: [{uses: input/wait, with: {prompt: fixture, evidence-id: roles-proof}}]}}"), 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "agents.yml")
			text := "agents:\n  fixture:\n    provider: claude-cli\n    command: \"false\"\n"
			if grants {
				text = "agents:\n  fixture:\n    provider: claude-sdk\n    bridge: unused-fixture.mjs\n    authentication: {mode: native, credential-env: AW_PRFIX_FIXTURE_TOKEN}\n    mcp-grants: [{server: engine, tool: worker_status}]\n"
			}
			if err := os.WriteFile(config, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			preparedConfig, err := loadPRFixConfig(ctx, root, config)
			if err != nil {
				t.Fatal(err)
			}
			preparedRoles, err := json.Marshal(preparedConfig)
			if err != nil {
				t.Fatal(err)
			}
			controllerConfig, err := clirunner.LoadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			executor := &workflows.Executor{WorkspaceDir: root, Store: store, MaxParallel: 4, Agents: &clirunner.Runner{Config: controllerConfig, Workspace: root}}
			run, err := executor.Prepare(ctx, workflows.RunRequest{Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			_, admitted, err := store.Load(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(admitted.Roles, preparedRoles) {
				t.Fatal("prepared roles differ from production controller admission")
			}
		})
	}
}

func TestPRFixJSONRejectsOversizedAndTrailingDocuments(t *testing.T) {
	const limit = 4 << 20
	for _, tc := range []struct {
		name, input, wantError string
	}{
		{"single document", `{}`, ""},
		{"exact limit", `{}` + strings.Repeat(" ", limit-2), ""},
		{"two documents", `{} {}`, "one JSON document"},
		{"second document beyond limit", `{}` + strings.Repeat(" ", limit-2) + `{}`, "exceeds 4 MiB"},
		{"whitespace beyond limit", `{}` + strings.Repeat(" ", limit-1), "exceeds 4 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			err := readPRFixJSON(path, &out)
			if tc.wantError == "" {
				if err != nil || out == nil || len(out) != 0 {
					t.Fatal("valid document rejected", out, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
		})
	}
}
