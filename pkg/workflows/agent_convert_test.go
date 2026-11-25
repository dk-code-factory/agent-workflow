package workflows

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentConvertOfflinePreservesCompatibleInstructions(t *testing.T) {
	t.Setenv("PATH", "")
	for _, from := range []string{"codex", "claude"} {
		t.Run(from, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "native")
			output := filepath.Join(dir, "agent.yaml")
			instructions := "First instruction.\nKeep both paragraphs.\n\nDo not publish.\n"
			source := "name = 'reviewer'\ndescription = 'Review changes'\nmodel = 'configured-model'\nmodel_reasoning_effort = 'high'\ndeveloper_instructions = '''" + instructions + "'''\n"
			if from == "claude" {
				source = "---\nname: reviewer\ndescription: Review changes\nmodel: configured-model\neffort: high\n...\n# End metadata.\n---\n" + instructions
			}
			if err := os.WriteFile(input, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			report, err := ConvertAgent(from, input, output, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := LoadAgentDefinition(output)
			if err != nil {
				t.Fatal(err)
			}
			if got.DeveloperInstructions != instructions || got.Model != "configured-model" || got.ReasoningEffort != "high" || got.Name != "reviewer" {
				t.Fatalf("lost definition: %+v", got)
			}
			if report.Output != output || len(report.IncompatibleOptions) != 0 {
				t.Fatalf("report %+v", report)
			}
			before, _ := os.ReadFile(input)
			if string(before) != source {
				t.Fatal("modified native source")
			}
		})
	}
}
func TestAgentConvertRefusesUnacknowledgedPolicyAndHooks(t *testing.T) {
	t.Setenv("PATH", "")
	for _, from := range []string{"codex", "claude"} {
		t.Run(from, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "native")
			output := filepath.Join(dir, "agent.yaml")
			marker := filepath.Join(dir, "hook-ran")
			source := "name = 'reviewer'\ndescription = 'Review'\ndeveloper_instructions = 'Inspect changes.'\nsandbox_mode = 'read-only'\napproval_policy = 'on-request'\nhooks = ['touch " + marker + "']\n"
			fields := []string{"sandbox_mode", "approval_policy", "hooks"}
			if from == "claude" {
				source = "---\nname: reviewer\ndescription: Review\ntools: [Read]\nhooks: {start: 'touch " + marker + "'}\n---\nInspect changes.\n"
				fields = []string{"tools", "hooks"}
			}
			if err := os.WriteFile(input, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			report, err := ConvertAgent(from, input, output, nil)
			var loss *AgentConversionError
			if !errors.As(err, &loss) || len(report.IncompatibleOptions) != len(fields) || len(report.RuntimePolicyChanges) != len(fields) {
				t.Fatalf("missing loss/policy diagnostic: %+v %v", report, err)
			}
			if _, err = os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("created unacknowledged output")
			}
			if from == "codex" && !strings.Contains(strings.Join(report.RuntimePolicyChanges, "\n"), "approval_policy: review and configure the corresponding target workspace agents.yml runtime policy") {
				t.Fatalf("missing approval-policy migration guidance: %+v", report)
			}
			if _, err = ConvertAgent(from, input, output, fields[:1]); err == nil {
				t.Fatal("partial loss acknowledgement accepted")
			}
			if _, err = ConvertAgent(from, input, output, fields); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("executed imported hook")
			}
			data, _ := os.ReadFile(input)
			if string(data) != source {
				t.Fatal("modified source")
			}
			if _, err = LoadAgentDefinition(output); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAgentConvertInvalidSourcesAndOutputCollisions(t *testing.T) {
	for _, tc := range []struct{ from, source string }{
		{"codex", "not TOML"}, {"codex", "name='a'\nname='b'"},
		{"codex", "name='a'\ndescription='d'\ndeveloper_instructions=42"},
		{"claude", "No frontmatter"}, {"claude", "---\nname: a\n---\n"},
		{"claude", "---\nname: a\nname: b\ndescription: d\n---\nWork"},
		{"claude", "---\nname: &name a\ndescription: *name\n---\nWork"},
		{"claude", "---\nname: a\ndescription: d\n...\ntools: [Bash]\n---\nWork"},
		{"claude", "---\nname: a\ndescription: d\n...\ninvalid trailing content\n---\nWork"},
	} {
		t.Run(tc.from+tc.source[:1], func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "native")
			output := filepath.Join(dir, "out.yaml")
			if err := os.WriteFile(input, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ConvertAgent(tc.from, input, output, nil); err == nil {
				t.Fatal("accepted invalid native source")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid input left output")
			}
		})
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "reviewer.toml")
	output := filepath.Join(dir, "out.yaml")
	source := []byte("name='reviewer'\ndescription='Review'\ndeveloper_instructions='Work'\n")
	if err := os.WriteFile(input, source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{input, output} {
		if _, err := ConvertAgent("codex", input, target, nil); err == nil {
			t.Fatal("overwrote output/source")
		}
	}
	before, _ := os.ReadFile(output)
	if string(before) != "keep" {
		t.Fatal("changed colliding output")
	}
	if err := os.Symlink(input, filepath.Join(dir, "link.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertAgent("codex", filepath.Join(dir, "link.toml"), filepath.Join(dir, "new.yaml"), nil); err == nil {
		t.Fatal("accepted source symlink")
	}
}
func TestAgentYAMLAdmissionStrictAndNativeGuidance(t *testing.T) {
	valid := "name: reviewer\ndescription: Review changes\ninstructions: |\n  Keep instructions.\n"
	for _, suffix := range []string{"tools: []\n", "authentication: {}\n", "sandbox: read-only\n", "permissions: {}\n", "mcp-grants: []\n", "turns: 2\n", "name: duplicate\n", "model: 42\n", "reasoning-effort: unsupported\n", "---\nname: other\n"} {
		if _, err := ParseAgentDefinition([]byte(valid + suffix)); err == nil {
			t.Fatalf("accepted %q", suffix)
		}
	}
	if _, err := ParseAgentDefinition(bytes.Repeat([]byte("a"), (1<<20)+1)); err == nil {
		t.Fatal("accepted oversized definition")
	}
	dir := t.TempDir()
	native := filepath.Join(dir, "reviewer.toml")
	if err := os.WriteFile(native, []byte("name='reviewer'\ndescription='Review'\ndeveloper_instructions='Work'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentDefinition(native); err == nil || !strings.Contains(err.Error(), "agent convert") {
		t.Fatalf("missing conversion guidance: %v", err)
	}
	if _, err := LoadAgentDefinition(dir); err == nil || !strings.Contains(err.Error(), "agent convert") {
		t.Fatalf("missing directory conversion guidance: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yml"), []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentDefinition(dir); err != nil {
		t.Fatal(err)
	}
}
func TestHistoricalTOMLSnapshotResumesWithoutMigration(t *testing.T) {
	source := "jobs:\n  review:\n    steps:\n      - id: work\n        uses: agent/run\n        with: {agent: reviewer.toml, prompt: work}\n"
	e, store, fake := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) { return map[string]any{"text": "done"}, nil })
	// This is the old admitted JSON shape, including raw TOML and original source.
	original := `{"name":"reviewer","description":"Review","developer_instructions":"Original instructions.","model":"pinned-codex-model","model_reasoning_effort":"high","source":"/removed/reviewer.toml","bytes":"name = 'reviewer'\ndescription = 'Review'\ndeveloper_instructions = 'Original instructions.'\n"}`
	var definition AgentDefinition
	if err := json.Unmarshal([]byte(original), &definition); err != nil {
		t.Fatal(err)
	}
	workflow, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["review"]
	job.Steps[0].Definition = &definition
	workflow.Jobs["review"] = job
	roles := json.RawMessage(`{"agents":{"codex":{"provider":"codex-app-server","model":"pinned-codex-model"}}}`)
	snap, err := snapshotFor(map[string]*Workflow{"workflows/main.yml": workflow}, RunRequest{}, roles, 1)
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: randomID(), WorkflowRef: "workflows/main.yml", Status: RunStatusQueued, Version: 1, Jobs: map[string]JobExecution{}}
	if err = store.Admit(t.Context(), run, snap); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err = store.db.QueryRow("SELECT snapshot FROM runs WHERE id=?", run.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	e.RoleRunner = func(admitted json.RawMessage) (AgentRunner, error) {
		if !bytes.Equal(admitted, roles) {
			t.Fatalf("changed Codex binding: %s", admitted)
		}
		return fake, nil
	}
	if err = os.RemoveAll(filepath.Join(e.WorkspaceDir, "workflows")); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resume(t.Context(), run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatalf("resume %+v: %v", result, err)
	}
	if len(fake.requests) != 1 || fake.requests[0].Definition.Bytes != definition.Bytes || fake.requests[0].Definition.Source != definition.Source || fake.requests[0].Definition.Model != definition.Model {
		t.Fatalf("migrated historical definition: %+v", fake.requests)
	}
	var after []byte
	if err = store.db.QueryRow("SELECT snapshot FROM runs WHERE id=?", run.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rewrote admitted snapshot bytes or hashes")
	}
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 1 {
		t.Fatal("replayed completed legacy work")
	}
}
