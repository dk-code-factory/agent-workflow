package clirunner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestSkillChildCapabilityPinsExistingRoleAuthorityAndResolvedEffort(t *testing.T) {
	root := t.TempDir()
	metadata := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk/package.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"version":"0.3.322"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"supported", "no-Read", "Bash-role", "MCP-role", "parent-model", "declared-effort", "resolved-effort", "child-model", "child-effort", "child-tools", "child-type", "background", "turns", "calls", "resource", "retained-ceiling", "retained-effort", "retained-profile"} {
		t.Run(variant, func(t *testing.T) {
			role := Agent{Provider: "claude-sdk", Bridge: filepath.Join(root, "bridge.mjs"), Tools: []string{"Read"}}
			plan := &workflows.SkillInvocation{Version: 1, Binding: workflows.SkillBinding{Version: 1, Name: "probe", Entry: "skills/probe/SKILL.md", Assets: &workflows.AssetBundle{}}, Model: "claude-sonnet-4-6", Children: []workflows.SkillChild{{Name: "review", Type: "aw-child-review", Prompt: "Review retained resource.", Description: "Admitted review", Tools: []string{"Read"}, Resources: []string{"reference.txt"}, Model: "claude-sonnet-4-6", MaxTurns: 2, MaxCalls: 1}}}
			if len(variant) > 9 && variant[:9] == "retained-" {
				var err error
				plan, err = pinSkillChildProfile(plan, role)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch variant {
			case "no-Read":
				role.Tools = nil
			case "Bash-role":
				role.Tools = []string{"Read", "Bash"}
			case "MCP-role":
				role.MCPGrants = []workflows.ToolGrant{{}}
			case "parent-model":
				plan.Model = "claude-opus-4-6"
			case "declared-effort":
				plan.Effort = "high"
			case "resolved-effort", "retained-effort":
				plan.ResolvedEffort = "low"
			case "child-model":
				plan.Children[0].Model = "haiku"
			case "child-effort":
				plan.Children[0].Effort = "low"
			case "child-tools":
				plan.Children[0].Tools = []string{"Bash"}
			case "child-type":
				plan.Children[0].Type = "general-purpose"
			case "background":
				plan.Children[0].Background = true
			case "turns":
				plan.Children[0].MaxTurns = 3
			case "calls":
				plan.Children[0].MaxCalls = 2
			case "resource":
				plan.Children[0].Resources = []string{"../foreign"}
			case "retained-ceiling":
				plan.EffectiveTools = []string{"Read", "Bash"}
			case "retained-profile":
				plan.Profile = "unproved-profile"
			}
			runner := &Runner{Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": role}}}
			admitted, err := runner.AdmitSkillTask(plan)
			if variant != "supported" {
				if err == nil {
					t.Fatal("unsupported selected child controls admitted", variant, admitted)
				}
				return
			}
			if err != nil || admitted.Profile != claudeSkillChildProfile || admitted.Effort != "" || admitted.ResolvedEffort != "high" || admitted.Children[0].Effort != "high" || !admitted.ContinuationRequiresEvidence || len(admitted.EffectiveTools) != 1 || admitted.EffectiveTools[0] != "Read" {
				t.Fatal(admitted, err)
			}
			if _, err = runner.AdmitSkillTask(admitted); err != nil {
				t.Fatal("retained canonical profile rejected", err)
			}
			if plan.ResolvedEffort != "" || plan.Children[0].Effort != "" {
				t.Fatal("capability admission mutated caller declaration")
			}
		})
	}
}

func TestSkillChildUnsupportedAuthorityRejectsBeforeRunAdmission(t *testing.T) {
	for _, variant := range []string{"no-Read", "Bash", "model", "declared-effort", "MCP"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			for name, text := range map[string]string{
				"bridge/node_modules/@anthropic-ai/claude-agent-sdk/package.json": `{"version":"0.3.322"}`,
				"skill/SKILL.md":          "---\nname: probe\ndescription: Admitted fixture.\n---\nBODY\n",
				"skill/reference.txt":     "REFERENCE\n",
				"workflows/reviewer.yaml": "name: reviewer\ndescription: Review retained resource.\ninstructions: Review.\nmodel: claude-sonnet-4-6\n",
				"workflows/main.yml": `on: {workflow_call: {inputs: {selected: {type: skill, required: true}}}}
jobs:
  work:
    steps:
      - uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: Review.
          skill:
            input: selected
            resources: [reference.txt]
            evidence: {invocation: required, children: [review]}
            children: [{name: review, prompt: Review., description: Admitted review, tools: [Read], resources: [reference.txt], model: inherit, max-turns: 2, max-calls: 1, background: false}]
`,
			} {
				p := filepath.Join(root, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer gateway.Close()
			t.Setenv("CHILD_PRECLAIM_FIXTURE", "synthetic-loopback-only")
			role := Agent{Provider: "claude-sdk", Bridge: filepath.Join(root, "bridge/bridge.mjs"), Tools: []string{"Read"}, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "CHILD_PRECLAIM_FIXTURE"}}
			switch variant {
			case "no-Read":
				role.Tools = nil
			case "Bash":
				role.Tools = []string{"Read", "Bash"}
			case "MCP":
				role.MCPGrants = []workflows.ToolGrant{{}}
			case "model":
				if err := os.WriteFile(filepath.Join(root, "workflows/reviewer.yaml"), []byte("name: reviewer\ndescription: Review.\ninstructions: Review.\nmodel: claude-opus-4-6\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "declared-effort":
				f, err := os.OpenFile(filepath.Join(root, "workflows/reviewer.yaml"), os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("reasoning-effort: high\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": role}}}
			store, err := workflows.OpenSQLite(filepath.Join(root, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			executor := &workflows.Executor{WorkspaceDir: root, Agents: runner, Store: store}
			prepared, err := executor.Prepare(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"selected": map[string]any{"directory": filepath.Join(root, "skill")}}})
			if err == nil || prepared != nil || calls.Load() != 0 {
				t.Fatal("unsupported child authority crossed preclaim boundary", prepared, err, calls.Load())
			}
			runs, err := store.List(t.Context())
			if err != nil || len(runs) != 0 {
				t.Fatal("unsupported child retained an execution", runs, err)
			}
			if entries, err := os.ReadDir(filepath.Join(store.Dir, "assets")); err == nil && len(entries) != 0 || err != nil && !os.IsNotExist(err) {
				t.Fatal("unsupported child was materialized before capability admission", entries, err)
			}
		})
	}
}
