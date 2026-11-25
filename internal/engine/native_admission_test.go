package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNativeDynamicOutputDeniedBeforeWorkerEffectsAfterRestart(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "provider-started")
	command := filepath.Join(root, "provider")
	// Any invocation of this provider command is observable. SDK preflight only
	// inspects its configured bridge; the rendered schema must fail before launch.
	script := "#!/bin/sh\nprintf started > '" + marker + "'\nexit 91\n"
	for path, content := range map[string]string{
		command: script,
		filepath.Join(root, "workflows", "reviewer.yaml"): "name: reviewer\ndescription: Test reviewer\ninstructions: Review the supplied task.\n",
		filepath.Join(root, "workflows", "main.yml"): `on:
  workflow_call:
    inputs:
      property: {type: string, required: true}
jobs:
  review:
    steps:
      - id: review
        uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: Review the task.
          output:
            format: json
            repair-attempts: 0
            schema:
              type: object
              required: ["${{ inputs.property }}"]
`,
	} {
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NATIVE_ADMISSION_TEST_KEY", "local-fixture-key")
	bridge := filepath.Join(root, "bridge.mjs")
	if err := os.WriteFile(bridge, []byte("// unreachable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-agent-sdk")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "package.json"), []byte(`{"version":"`+clirunner.ClaudeSDKVersion+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := clirunner.Config{AgentRuntime: "native", Agents: map[string]clirunner.Agent{"native": {Provider: "claude-sdk", Command: command, Bridge: bridge, Authentication: clirunner.Authentication{Mode: "native", CredentialEnv: "NATIVE_ADMISSION_TEST_KEY"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := workflows.OpenSQLite(state)
	if err != nil {
		t.Fatal(err)
	}
	executor, _ := Executor(cfg, store, root, 1)
	queued, err := executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"property": "__proto__"}})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	_, admitted, err := store.Load(ctx, queued.ID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	var saved clirunner.Config
	if err := json.Unmarshal(admitted.Roles, &saved); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if saved.NativeTaskAdmission != 1 {
		store.Close()
		t.Fatal("new admission lost output policy marker")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Recreate the production interpreter and SQLite connection. Current config
	// cannot disable the policy retained in the queued snapshot.
	store, err = workflows.OpenSQLite(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, workers := Executor(clirunner.Config{}, store, root, 1)
	run, err := executor.Resume(ctx, queued.ID, nil, false)
	if err == nil || !strings.Contains(err.Error(), "__proto__") {
		t.Fatalf("expected rendered contract denial, run=%+v err=%v", run, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("provider launched before denial: %v", err)
	}
	retainedWorkers, err := workers.List(ctx, queued.ID)
	if err != nil || len(retainedWorkers) != 0 {
		t.Fatalf("denied contract created workers: %+v %v", retainedWorkers, err)
	}
	attempts, err := store.Attempts(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.AgentTask != nil {
			t.Fatal("denied contract retained dispatch intent")
		}
	}
	_, restored, err := store.Load(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(admitted.Roles) != string(restored.Roles) {
		t.Fatal("resume rewrote admitted Roles")
	}
}

func TestKnownUnsupportedNativeRepairDeniedBeforeDiscovery(t *testing.T) {
	for _, dynamic := range []bool{false, true} {
		t.Run(map[bool]string{false: "literal", true: "dynamic-description"}[dynamic], func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "discovery")
			command := filepath.Join(root, "codex")
			if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			description := "static"
			if dynamic {
				description = "${{ inputs.description }}"
			}
			source := "on: {workflow_call: {inputs: {description: {type: string}}}}\njobs: {review: {steps: [{uses: agent/run, with: {agent: reviewer.yaml, prompt: Review, output: {format: json, repair-attempts: 1, schema: {type: object, description: '" + description + "'}}}}]}}\n"
			for name, text := range map[string]string{"main.yml": source, "reviewer.yaml": "name: reviewer\ndescription: Review\ninstructions: Review the task.\n"} {
				if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := clirunner.Config{Agents: map[string]clirunner.Agent{"codex": {Provider: "codex-app-server", Command: command}}}
			executor, _ := Executor(cfg, store, root, 1)
			_, err = executor.Prepare(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"description": "rendered"}})
			if err == nil || !strings.Contains(err.Error(), "tool-free output repair") {
				t.Fatalf("expected pure option denial, got %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("denied repair invoked executable discovery: %v", err)
			}
		})
	}
}

func TestNativeRuntimeOptionsDeniedBeforeExecutableDiscovery(t *testing.T) {
	for _, scenario := range []string{"missing-role", "unsupported-backend", "codex-tools", "codex-mcp", "claude-reasoning", "definition-model-type"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "invoked")
			command := filepath.Join(root, "provider")
			if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 99\n"), 0700); err != nil {
				t.Fatal(err)
			}
			definition := "name: reviewer\ndescription: Review\ninstructions: Review the supplied task.\n"
			role := clirunner.Agent{Provider: "codex-app-server", Command: command}
			selected := "native"
			expected := ""
			switch scenario {
			case "missing-role":
				selected = "missing"
				expected = "unconfigured"
			case "unsupported-backend":
				role.Provider = "claude-cli"
				expected = "runtime provider"
			case "codex-tools":
				role.Tools = []string{}
				expected = "managed tool subsets"
			case "codex-mcp":
				role.MCPGrants = []workflows.ToolGrant{{Server: "engine", Tool: "worker_spawn"}}
				expected = "managed tool subsets"
			case "claude-reasoning":
				role.Provider = "claude-sdk"
				definition += "reasoning-effort: high\n"
				expected = "reasoning-effort"
			case "definition-model-type":
				definition += "model: [unsupported, list]\n"
				expected = "string"
			}
			for name, text := range map[string]string{"main.yml": "jobs: {review: {steps: [{uses: agent/run, with: {agent: reviewer.yaml, prompt: Review}}]}}\n", "reviewer.yaml": definition} {
				if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			executor, _ := Executor(clirunner.Config{AgentRuntime: selected, Agents: map[string]clirunner.Agent{"native": role}}, store, root, 1)
			_, err = executor.Prepare(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), expected) {
				t.Fatalf("expected %q denial, got %v", expected, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("denied options invoked provider executable: %v", err)
			}
		})
	}
}

func TestRenderedNativeModelTypeDeniedBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, log := nativeRuntimeFixture(t, root, "claude-sdk")
	source := `on: {workflow_call: {inputs: {model: {type: object, required: true}}}}
jobs: {review: {steps: [{id: review, uses: agent/run, with: {agent: reviewer.yaml, prompt: Review, model: '${{ inputs.model }}'}}]}}
`
	for name, text := range map[string]string{"main.yml": source, "reviewer.yaml": "name: reviewer\ndescription: Review\ninstructions: Review the supplied task.\n"} {
		if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, workers := Executor(cfg, store, root, 1)
	run, err := executor.Run(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"model": map[string]any{"unsupported": "object"}}})
	if err == nil || !strings.Contains(err.Error(), "model must be a string") {
		t.Fatalf("model silently defaulted: %+v %v", run, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("invalid rendered model reached provider", err)
	}
	all, err := workers.List(context.Background(), run.ID)
	if err != nil || len(all) != 0 {
		t.Fatal("invalid rendered model retained worker", all, err)
	}
}

func TestUnsupportedNativeOptionsDeniedBeforeConnectorDiscovery(t *testing.T) {
	for _, admission := range []string{"prepare", "run"} {
		for _, scenario := range []struct {
			name, selector, role, provider, definition, output, denial, agent string
		}{
			{"explicit-codex-grant", "native", "native", "codex-app-server", "", "", "managed tool subsets", ""},
			{"default-codex-grant", "", "codex", "codex-app-server", "", "", "managed tool subsets", ""},
			{"default-codex-session-reference-grant", "", "codex", "codex-app-server", "", "", "managed tool subsets", "'${{ \"agent_existing\" }}'"},
			{"default-nonnative-session-reference-grant", "", "codex", "claude", "", "", "requires runtime provider", "'${{ \"agent_existing\" }}'"},
			{"claude-reasoning", "native", "native", "claude-sdk", "reasoning-effort: high\n", "", "reasoning-effort", ""},
			{"claude-schema", "native", "native", "claude-sdk", "", ", output: {format: json, repair-attempts: 0, schema: {type: object, required: [__proto__]}}", "__proto__", ""},
		} {
			t.Run(admission+"/"+scenario.name, func(t *testing.T) {
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(root, "connector-started")
				command := filepath.Join(root, "connector")
				agent := scenario.agent
				if agent == "" {
					agent = "reviewer.yaml"
				}
				for path, text := range map[string]string{
					command: "#!/bin/sh\nprintf started > '" + marker + "'\nexit 99\n",
					filepath.Join(root, "workflows", "reviewer.yaml"): "name: reviewer\ndescription: Review\ninstructions: Review the task.\n" + scenario.definition,
					filepath.Join(root, "workflows", "main.yml"):      "jobs: {review: {steps: [{uses: agent/run, with: {agent: " + agent + ", prompt: Review" + scenario.output + "}}]}}\n",
				} {
					if err := os.WriteFile(path, []byte(text), 0700); err != nil {
						t.Fatal(err)
					}
				}
				cfg := clirunner.Config{AgentRuntime: scenario.selector, Agents: map[string]clirunner.Agent{scenario.role: {Provider: scenario.provider, MCPGrants: []workflows.ToolGrant{{Server: "external", Tool: "execute"}}}}, MCPServers: map[string]clirunner.MCPServer{"external": {Transport: "stdio", Command: command}}}
				store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				executor, _ := Executor(cfg, store, root, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if admission == "prepare" {
					_, err = executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
				} else {
					_, err = executor.Run(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
				}
				if err == nil || !strings.Contains(err.Error(), scenario.denial) {
					t.Fatalf("expected capability denial %q, got %v", scenario.denial, err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("unsupported native options started MCP discovery", err)
				}
			})
		}
	}
}

func TestNonNativeAdmissionKeepsUnusedDefaultCodexMCPGrant(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {wait: {steps: [{uses: input/wait, with: {prompt: Continue, evidence-id: fixture}}]}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "unused-codex-policy", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		t.Error("catalog admission executed a tool")
		return &mcp.CallToolResult{}, nil
	})
	var requests atomic.Int32
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	cfg := clirunner.Config{Agents: map[string]clirunner.Agent{"codex": {Provider: "codex-app-server", Command: "unavailable-codex", MCPGrants: []workflows.ToolGrant{{Server: "fixture", Tool: "lookup"}}}}, MCPServers: map[string]clirunner.MCPServer{"fixture": {Transport: "streamable-http", Endpoint: endpoint.URL}}}
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor, _ := Executor(cfg, store, root, 1)
	queued, err := executor.Prepare(ctx, workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := store.Load(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retained clirunner.Config
	if err := json.Unmarshal(snapshot.Roles, &retained); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 || len(retained.ToolCatalog) != 1 || retained.CatalogFingerprint == "" {
		t.Fatal("fresh admission did not retain its discovered catalog", requests.Load(), retained)
	}
}
