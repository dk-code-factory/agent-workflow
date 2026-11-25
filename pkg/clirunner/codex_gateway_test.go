package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Actual pinned Codex, synthetic credentials, and a loopback Responses API.
// This checks prompt construction, not model adherence or live conformance.
func TestCodexAgainstFakeGateway(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_CODEX_TEST") != "1" {
		t.Skip("enable with installed pinned Codex")
	}
	binary, err := filepath.Abs(filepath.Join("..", "..", "images", "codex", "node_modules", ".bin", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if configured := os.Getenv("AGENT_WORKFLOW_CODEX_BINARY"); configured != "" {
		binary = configured
	}
	if _, err = os.Stat(binary); err != nil {
		t.Fatal("install pinned Codex first", err)
	}
	root := t.TempDir()
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "a", Prompt: "$repo-probe $user-probe $skill-creator Finish fixture."}}
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	for path, marker := range map[string]string{
		filepath.Join(root, ".agents", "skills", "repo-probe", "SKILL.md"):          "REPO_SKILL_BODY_MUST_NOT_LOAD_cad2",
		filepath.Join(state, "skills", "user-probe", "SKILL.md"):                    "USER_SKILL_BODY_MUST_NOT_LOAD_b91a",
		filepath.Join(state, "home", ".agents", "skills", "user-probe", "SKILL.md"): "HOME_SKILL_BODY_MUST_NOT_LOAD_793d",
	} {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(filepath.Dir(path))
		if err = os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: HOSTILE_CATALOG_MUST_NOT_LOAD_b8d2\n---\n"+marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Errorf("unexpected local API path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		for _, forbidden := range []string{"REPO_SKILL_BODY_MUST_NOT_LOAD", "USER_SKILL_BODY_MUST_NOT_LOAD", "HOME_SKILL_BODY_MUST_NOT_LOAD", "HOSTILE_CATALOG_MUST_NOT_LOAD", "Available skills", "skill-creator/SKILL.md"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("unadmitted skill entered request: %s", forbidden)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_fixture"}},
			{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "msg_fixture", "content": []any{map[string]any{"type": "output_text", "text": "CODEX_FIXTURE_OK"}}}},
			{"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "usage": map[string]any{"input_tokens": 0, "input_tokens_details": nil, "output_tokens": 0, "output_tokens_details": nil, "total_tokens": 0}}},
		} {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	defer gateway.Close()
	// The wrapper adds only fixture transport configuration; all isolation flags
	// and the discovery/disable protocol are supplied by the production adapter.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := filepath.Join(root, "codex-fixture")
	script := "#!/bin/sh\nexec " + quote(binary) + " \"$@\"" +
		" --config 'model_provider=\"fixture\"' --config 'model=\"gpt-5.4\"'" +
		" --config 'model_providers.fixture.name=\"fixture\"'" +
		" --config " + quote("model_providers.fixture.base_url="+fmt.Sprintf("%q", gateway.URL)) +
		" --config 'model_providers.fixture.wire_api=\"responses\"'" +
		" --config 'model_providers.fixture.requires_openai_auth=false'\n"
	if err = os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_FIXTURE_KEY", "synthetic-no-provider-key")
	r := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"a": {Provider: "codex-app-server", Command: command, Authentication: Authentication{Mode: "api-key", CredentialEnv: "CODEX_FIXTURE_KEY"}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := r.RunTurn(ctx, req, nil)
	if err != nil || result.Outputs["text"] != "CODEX_FIXTURE_OK" || calls.Load() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
	config, err := os.ReadFile(filepath.Join(state, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, ".agents", "skills", "repo-probe", "SKILL.md"), filepath.Join(state, "skills", "user-probe", "SKILL.md")} {
		if !strings.Contains(string(config), path) {
			t.Fatalf("hostile skill fixture was not discovered and disabled: %s", path)
		}
	}
	// Re-register isolation in a fresh process on resume as well.
	req.SessionID = result.SessionID
	resumed, err := r.RunTurn(ctx, req, nil)
	if err != nil || resumed.SessionID != result.SessionID || calls.Load() != 2 {
		t.Fatalf("resume=%+v err=%v calls=%d", resumed, err, calls.Load())
	}
}
