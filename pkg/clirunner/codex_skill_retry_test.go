package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestCodexSkillRetryNotificationCancelsBeforeLaterSuccess(t *testing.T) {
	if os.Getenv("AW_SKILL_RETRY_FIXTURE") == "1" {
		scanner := bufio.NewScanner(os.Stdin)
		encoder := json.NewEncoder(os.Stdout)
		for scanner.Scan() {
			var f struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &f) != nil {
				os.Exit(2)
			}
			result := map[string]any{}
			switch f.Method {
			case "initialize":
				result["userAgent"] = "codex_cli_rs/0.167.1 (fixture)"
			case "config/read":
				result["config"] = map[string]any{"model_provider": codexSkillProvider, "model_providers": map[string]any{codexSkillProvider: map[string]any{"name": "OpenAI", "supports_standalone_web_search": true, "http_headers": map[string]any{"version": "0.167.1"}, "env_http_headers": map[string]any{"OpenAI-Organization": "OPENAI_ORGANIZATION", "OpenAI-Project": "OPENAI_PROJECT"}, "request_max_retries": 0, "stream_max_retries": 0, "supports_websockets": false, "wire_api": "responses", "requires_openai_auth": true}}, "features": map[string]any{"unbounded_connection_retries": false, "responses_websockets": false, "responses_websockets_v2": false}}
			case "skills/list":
				result["data"] = []any{map[string]any{"cwd": f.Params["cwds"].([]any)[0], "skills": []any{map[string]any{"path": filepath.Join(os.Getenv("AW_SKILL_ROOT"), "skills/probe/SKILL.md"), "enabled": true}}, "errors": []any{}}}
			case "thread/start":
				result = map[string]any{"thread": map[string]any{"id": "original-session"}, "modelProvider": codexSkillProvider}
			case "turn/start":
				_ = encoder.Encode(map[string]any{"id": f.ID, "result": map[string]any{"turn": map[string]any{"id": "original-turn"}}})
				thread, turn := "original-session", "original-turn"
				if os.Getenv("AW_SKILL_RETRY_MODE") == "foreign" {
					thread = "foreign-session"
					turn = "foreign-turn"
				}
				_ = encoder.Encode(map[string]any{"method": "error", "params": map[string]any{"threadId": thread, "turnId": turn, "willRetry": true, "error": map[string]any{"message": "Reconnecting"}}})
				_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "original-session", "turn": map[string]any{"id": "original-turn", "status": "completed"}}})
				continue
			case "turn/interrupt":
				_ = os.WriteFile("interrupt-observed", []byte("yes"), 0600)
			case "initialized":
				continue
			case "account/login/start", "skills/extraRoots/set":
			default:
				os.Exit(3)
			}
			_ = encoder.Encode(map[string]any{"id": f.ID, "result": result})
		}
		os.Exit(0)
	}
	for _, mode := range []string{"matched", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			req := codexSkillFixture(t)
			root := t.TempDir()
			self, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			command := filepath.Join(root, "native-fixture")
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
			script := fmt.Sprintf("#!/bin/sh\nexport AW_SKILL_RETRY_FIXTURE=1 AW_SKILL_ROOT=%s AW_SKILL_RETRY_MODE=%s\nexec %s -test.run '^TestCodexSkillRetryNotificationCancelsBeforeLaterSuccess$'\n", quote(req.SkillRoot), quote(mode), quote(self))
			if e = os.WriteFile(command, []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("AW_SKILL_KEY", "synthetic-fixture-only")
			r := &Runner{Workspace: root}
			agent := Agent{Command: command, Authentication: Authentication{Mode: "api-key", CredentialEnv: "AW_SKILL_KEY"}}
			ledger := &workflows.SkillEvidence{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			result, e := r.codexTurn(ctx, agent, req, func(event workflows.ProviderEvent) {
				if event.Type == "skill_attempt_uncertainty" {
					raw, _ := json.Marshal(event.Data)
					var o workflows.SkillAttemptUncertainty
					if err := json.Unmarshal(raw, &o); err != nil {
						t.Error(err)
					} else if err = ledger.ObserveUncertainAttempt(req.Skill, o); err != nil {
						t.Error(err)
					}
				}
			})
			if mode == "matched" {
				if e == nil || result.Status == workflows.RunStatusSucceeded || result.Outputs["usage_incomplete"] != true || len(ledger.UncertainAttempts) != 1 {
					t.Fatal("later success settled retry", result, e, ledger)
				}
				if _, e = os.Stat(filepath.Join(root, "interrupt-observed")); e != nil {
					t.Fatal("retry did not cancel native turn", e)
				}
			} else if e != nil || result.Status != workflows.RunStatusSucceeded || len(ledger.UncertainAttempts) != 0 {
				t.Fatal("foreign retry changed current turn", result, e, ledger)
			}
		})
	}
}

func TestCodexSkillEndpointRequiresWinningSessionFlagProvenance(t *testing.T) {
	for _, variant := range []string{"stock", "explicit-cli", "implicit-cli", "project", "user", "missing-origin", "missing-layer", "changed-layer", "disabled-layer", "credential", "changed-name", "extra-header"} {
		t.Run(variant, func(t *testing.T) {
			p := map[string]any{"name": "OpenAI", "wire_api": "responses", "requires_openai_auth": true, "supports_standalone_web_search": true, "supports_websockets": false, "request_max_retries": 0, "stream_max_retries": 0, "http_headers": map[string]any{"version": "0.167.1"}, "env_http_headers": map[string]any{"OpenAI-Organization": "OPENAI_ORGANIZATION", "OpenAI-Project": "OPENAI_PROJECT"}, "auth": nil}
			endpoint := "http://127.0.0.1:1"
			origin := map[string]any{"name": map[string]any{"type": "sessionFlags"}, "version": "original"}
			layer := map[string]any{"name": map[string]any{"type": "sessionFlags"}, "version": "original", "config": map[string]any{"model_providers": map[string]any{codexSkillProvider: map[string]any{"base_url": endpoint}}}}
			response := map[string]any{"config": map[string]any{"model_provider": codexSkillProvider, "model_providers": map[string]any{codexSkillProvider: p}, "features": map[string]any{"unbounded_connection_retries": false, "responses_websockets": false, "responses_websockets_v2": false}}, "origins": map[string]any{"model_providers." + codexSkillProvider + ".base_url": origin}, "layers": []any{layer}}
			explicit := true
			if variant != "stock" {
				p["base_url"] = endpoint
			}
			switch variant {
			case "implicit-cli":
				explicit = false
			case "project", "user":
				origin["name"] = map[string]any{"type": variant}
				layer["name"] = map[string]any{"type": variant}
			case "missing-origin":
				response["origins"] = map[string]any{}
			case "missing-layer":
				response["layers"] = []any{}
			case "changed-layer":
				layer["config"] = map[string]any{}
			case "disabled-layer":
				layer["disabledReason"] = "disabled"
			case "credential":
				p["auth"] = map[string]any{"command": "unadmitted"}
			case "changed-name":
				p["name"] = "foreign"
			case "extra-header":
				p["http_headers"].(map[string]any)["unadmitted"] = "foreign"
			}
			raw, _ := json.Marshal(response)
			err := verifyCodexSkillRetryConfig(raw, explicit)
			if (err == nil) != (variant == "stock" || variant == "explicit-cli") {
				t.Fatal("endpoint/control authority changed", variant, err)
			}
		})
	}
}
