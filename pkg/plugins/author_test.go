package plugins

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

func put(t *testing.T, root, name, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestPluginInitOffline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hello")
	t.Setenv("PATH", "")
	if err := Init(root, "hello", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	d, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version == d.EngineVersion || len(d.Hosts) != 2 {
		t.Fatalf("descriptor: %+v", d)
	}
	inputs, err := Inputs(root, d.Skills[0])
	if err != nil || !inputs["task"].Required {
		t.Fatalf("inputs %v: %v", inputs, err)
	}
	if _, err = os.Stat(filepath.Join(root, "agents.yml")); !os.IsNotExist(err) {
		t.Fatal("created runtime authority")
	}
	for _, p := range []string{"config/agents.codex.example.yml", "config/agents.claude.example.yml", "fixtures/example.yaml"} {
		if _, err = os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err = Init(root, "hello", "0.2.2", ""); err == nil {
		t.Fatal("overwrote project")
	}
}
func TestPluginDescriptorDiagnostics(t *testing.T) {
	valid := Descriptor{SchemaVersion: 1, Name: "demo", Version: "1.0.0", EngineVersion: "0.2.2", Hosts: []string{"codex"}, Skills: []Skill{{Name: "demo", Path: "skills/demo/SKILL.md", Workflow: "workflows/main.yml"}}}
	for _, tc := range []struct {
		name string
		edit func(*Descriptor)
	}{
		{"schema", func(d *Descriptor) { d.SchemaVersion = 2 }},
		{"identity", func(d *Descriptor) { d.Name = "../bad" }},
		{"version", func(d *Descriptor) { d.EngineVersion = "latest" }},
		{"host", func(d *Descriptor) { d.Hosts = []string{"codex", "codex"} }},
		{"duplicate skill", func(d *Descriptor) { d.Skills = append(d.Skills, d.Skills[0]) }},
		{"binding", func(d *Descriptor) { d.Skills[0].Workflow = "../other.yml" }},
		{"metadata path", func(d *Descriptor) { d.Skills[0].Path = "skills/demo/README.md" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			d.Skills = append([]Skill(nil), valid.Skills...)
			tc.edit(&d)
			data, _ := yaml.Marshal(d)
			_, err := Parse(data)
			if err == nil || !strings.Contains(err.Error(), DescriptorFile) {
				t.Fatalf("expected file diagnostic, got %v", err)
			}
		})
	}
	for _, extra := range []string{"name: again\n", "unknown: true\n", "---\nname: other\n"} {
		b, _ := yaml.Marshal(valid)
		if _, err := Parse(append(b, []byte(extra)...)); err == nil {
			t.Fatalf("accepted %q", extra)
		}
	}
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	put(t, root, "skills/demo/SKILL.md", "---\nname: demo\n---\nInstructions\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "skills/demo/SKILL.md") {
		t.Fatalf("missing metadata diagnostic: %v", err)
	}
}
func TestPluginImportDependencyClosure(t *testing.T) {
	source := t.TempDir()
	put(t, source, "workflows/main.yml", `on: {workflow_call: {inputs: {task: {type: string, required: true}}}}
jobs:
  nested:
    uses: ./workflows/nested/child.yml
    with: {task: '${{ inputs.task }}'}
`)
	put(t, source, "workflows/nested/child.yml", `on: {workflow_call: {inputs: {task: {type: string}}}}
step-definitions:
  invoke:
    parameters:
      definition: {type: string, default: agents/task.toml}
    step:
      uses: agent/run
      with: {agent: '${{ parameters.definition }}', prompt: '${{ inputs.task }}'}
jobs:
  main:
    steps:
      - id: invoke
        use-definition: invoke
      - uses: docker://python:3
        with: {entrypoint: python, args: helpers/report.py}
        files: [helpers]
`)
	put(t, source, "workflows/nested/agents/task.toml", exampleAgent)
	put(t, source, "workflows/nested/helpers/report.py", "print('hello')\n")
	if err := os.Chmod(filepath.Join(source, "workflows/nested/helpers/report.py"), 0755); err != nil {
		t.Fatal(err)
	}
	put(t, source, "workflows/unrelated.yml", "not a workflow")
	put(t, source, "secrets.txt", "must not copy")
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml")); err != nil {
		t.Fatal(err)
	}
	d, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	all, err := (&workflows.Executor{WorkspaceDir: root}).Load(d.Skills[0].Workflow)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("closure %v", all)
	}
	for _, p := range []string{"imports/demo/workflows/main.yml", "imports/demo/workflows/nested/child.yml", "imports/demo/workflows/nested/agents/task.toml", "imports/demo/workflows/nested/helpers/report.py"} {
		if _, err = os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatal(err)
		}
	}
	info, _ := os.Stat(filepath.Join(root, "imports/demo/workflows/nested/helpers/report.py"))
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("lost executable mode")
	}
	for _, p := range []string{"imports/demo/secrets.txt", "imports/demo/workflows/unrelated.yml"} {
		if _, err = os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Fatalf("copied undeclared %s", p)
		}
	}
	inputs, err := Inputs(root, d.Skills[0])
	if err != nil || !inputs["task"].Required {
		t.Fatalf("input declaration changed: %v %v", inputs, err)
	}
}
func TestPluginStandaloneAndAddSkillPreserveSources(t *testing.T) {
	source := t.TempDir()
	put(t, source, "custom.yaml", exampleWorkflow)
	put(t, source, "agents/task.toml", exampleAgent)
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(root, "workflows/main.yml"))
	put(t, root, "notes.txt", "keep me")
	if err := AddSkill(root, "custom", filepath.Join(source, "custom.yaml")); err != nil {
		t.Fatal(err)
	}
	d, err := Load(root)
	if err != nil || len(d.Skills) != 2 {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err = (&workflows.Executor{WorkspaceDir: root}).Load(d.Skills[1].Workflow); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(filepath.Join(root, "workflows/main.yml"))
	if !bytes.Equal(original, current) {
		t.Fatal("overwrote source")
	}
	if err = AddSkill(root, "custom", filepath.Join(source, "custom.yaml")); err == nil {
		t.Fatal("accepted duplicate skill")
	}
	put(t, root, "skills/collision/SKILL.md", "keep")
	before, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
	if err = AddSkill(root, "collision", filepath.Join(source, "custom.yaml")); err == nil {
		t.Fatal("overwrote collision")
	}
	after, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
	if !bytes.Equal(before, after) {
		t.Fatal("changed descriptor on failed import")
	}
	if _, err = os.Stat(filepath.Join(root, "imports/collision")); !os.IsNotExist(err) {
		t.Fatal("partial import on collision")
	}
}
func TestPluginRejectUnsafeAndMissingClosure(t *testing.T) {
	for _, tc := range []struct {
		name, agent, uses, resource string
		symlink                     bool
	}{
		{name: "missing agent", agent: "missing.toml"},
		{name: "absolute agent", agent: "/etc/passwd"},
		{name: "escaping agent", agent: "../../outside.toml"},
		{name: "remote reusable", uses: "org/repo/workflows/main.yml@main"},
		{name: "escaping reusable", uses: "workflows/../main.yml"},
		{name: "missing resource", agent: "agents/task.toml", resource: "missing"},
		{name: "escaping resource", agent: "agents/task.toml", resource: "../../outside"},
		{name: "symlink", agent: "agents/task.toml", symlink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			put(t, source, "workflows/agents/task.toml", exampleAgent)
			w := exampleWorkflow
			if tc.uses != "" {
				w = "on: {workflow_call: {}}\njobs:\n  main:\n    uses: " + tc.uses + "\n"
			} else {
				w = strings.ReplaceAll(w, "agents/task.toml", tc.agent)
			}
			if tc.resource != "" {
				w += "      - uses: docker://python:3\n        with: {entrypoint: python, args: helper.py}\n        files: [" + tc.resource + "]\n"
			}
			put(t, source, "workflows/main.yml", w)
			if tc.symlink {
				if err := os.Symlink(filepath.Join(source, "workflows/agents/task.toml"), filepath.Join(source, "workflows/link.toml")); err != nil {
					t.Fatal(err)
				}
				put(t, source, "workflows/main.yml", strings.ReplaceAll(w, "agents/task.toml", "link.toml"))
			}
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml")); err == nil {
				t.Fatal("accepted invalid closure")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("failed import left project")
			}
		})
	}
}

func TestPluginAgentSelectionsAndRelativeParents(t *testing.T) {
	source := t.TempDir()
	put(t, source, "workflows/main.yml", "on: {workflow_call: {}}\njobs:\n  nested:\n    uses: workflows/nested/child.yml\n")
	put(t, source, "workflows/nested/child.yml", `on: {workflow_call: {}}
jobs:
  main:
    steps:
      - id: work
        uses: agent/run
        with:
          agent:
            select: primary
            definitions:
              primary: ../agents/task.toml
              secondary: ../agents/other
          prompt: hello
        finally:
          - uses: agent/run
            with: {agent: ../agents/final.toml, prompt: finish}
`)
	for _, p := range []string{"workflows/agents/task.toml", "workflows/agents/other/definition.toml", "workflows/agents/final.toml"} {
		put(t, source, p, exampleAgent)
	}
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml")); err != nil {
		t.Fatal(err)
	}
	d, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (&workflows.Executor{WorkspaceDir: root}).Load(d.Skills[0].Workflow); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"task.toml", "other/definition.toml", "final.toml"} {
		if _, err = os.Stat(filepath.Join(root, "imports/demo/workflows/agents", p)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPluginResourceAncestorSymlinkAndCycle(t *testing.T) {
	for _, kind := range []string{"ancestor symlink", "cycle", "missing metadata", "mismatched metadata"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", ""); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "ancestor symlink":
				external := t.TempDir()
				if err := os.Symlink(external, filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				d, err := ReadDescriptor(root)
				if err != nil {
					t.Fatal(err)
				}
				d.Resources = []string{"linked/empty"}
				if err = os.Mkdir(filepath.Join(external, "empty"), 0755); err != nil {
					t.Fatal(err)
				}
				b, _ := yaml.Marshal(d)
				put(t, root, DescriptorFile, string(b))
			case "cycle":
				put(t, root, "workflows/main.yml", "on: {workflow_call: {}}\njobs:\n  call:\n    uses: workflows/main.yml\n")
			case "missing metadata":
				if err := os.Remove(filepath.Join(root, "skills/demo/SKILL.md")); err != nil {
					t.Fatal(err)
				}
			case "mismatched metadata":
				put(t, root, "skills/demo/SKILL.md", string(skillText("other")))
			}
			if _, err := Load(root); err == nil {
				t.Fatal("accepted invalid project")
			}
		})
	}
}

func TestPluginManualEntryAndFailedInitPreserve(t *testing.T) {
	source := t.TempDir()
	put(t, source, "manual.yml", "name: Manual entry\njobs:\n  hello:\n    steps:\n      - argv: [echo, hello]\n")
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "manual.yml")); err != nil {
		t.Fatal(err)
	}
	d, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := Inputs(root, d.Skills[0])
	if err != nil || len(inputs) != 0 {
		t.Fatalf("%v %v", inputs, err)
	}
	put(t, root, "untouched.txt", "original")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "missing.yml")); err == nil {
		t.Fatal("replaced existing project")
	}
	b, _ := os.ReadFile(filepath.Join(root, "untouched.txt"))
	if string(b) != "original" {
		t.Fatal("damaged existing project")
	}
}
