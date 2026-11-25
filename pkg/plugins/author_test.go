package plugins

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

type failedAuthorFile struct {
	*os.File
	onWrite func()
	write   bool
	close   bool
}

func (f *failedAuthorFile) Write(data []byte) (int, error) {
	if f.onWrite != nil {
		f.onWrite()
	}
	if f.write {
		n, _ := f.File.Write(data[:min(4, len(data))])
		return n, errors.New("injected write failure")
	}
	return f.File.Write(data)
}

func (f *failedAuthorFile) Close() error {
	err := f.File.Close()
	if f.close {
		return errors.New("injected close failure")
	}
	return err
}

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

func TestPluginInputsUsesAdmittedWorkflowBinding(t *testing.T) {
	for _, binding := range []string{"workflows/main.yml", "./workflows/main.yml", " \t./workflows/main.yml \n"} {
		t.Run(binding, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", ""); err != nil {
				t.Fatal(err)
			}
			d, err := ReadDescriptor(root)
			if err != nil {
				t.Fatal(err)
			}
			d.Skills[0].Workflow = binding
			data, err := yaml.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, DescriptorFile, string(data))
			d, err = Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if d.Skills[0].Workflow != binding {
				t.Fatalf("descriptor binding changed: %q", d.Skills[0].Workflow)
			}
			inputs, err := Inputs(root, d.Skills[0])
			if err != nil || len(inputs) != 1 || inputs["task"].Type != "string" || !inputs["task"].Required {
				t.Fatalf("admitted workflow inputs: %v, %v", inputs, err)
			}
		})
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
      definition: {type: string, default: agents/task.yaml}
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
	put(t, source, "workflows/nested/agents/task.yaml", exampleAgent)
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
	for _, p := range []string{"imports/demo/workflows/main.yml", "imports/demo/workflows/nested/child.yml", "imports/demo/workflows/nested/agents/task.yaml", "imports/demo/workflows/nested/helpers/report.py"} {
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
	put(t, source, "agents/task.yaml", exampleAgent)
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
		{name: "missing agent", agent: "missing.yaml"},
		{name: "absolute agent", agent: "/etc/passwd"},
		{name: "escaping agent", agent: "../../outside.yaml"},
		{name: "remote reusable", uses: "org/repo/workflows/main.yml@main"},
		{name: "escaping reusable", uses: "workflows/../main.yml"},
		{name: "missing resource", agent: "agents/task.yaml", resource: "missing"},
		{name: "escaping resource", agent: "agents/task.yaml", resource: "../../outside"},
		{name: "symlink", agent: "agents/task.yaml", symlink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			put(t, source, "workflows/agents/task.yaml", exampleAgent)
			w := exampleWorkflow
			if tc.uses != "" {
				w = "on: {workflow_call: {}}\njobs:\n  main:\n    uses: " + tc.uses + "\n"
			} else {
				w = strings.ReplaceAll(w, "agents/task.yaml", tc.agent)
			}
			if tc.resource != "" {
				w += "      - uses: docker://python:3\n        with: {entrypoint: python, args: helper.py}\n        files: [" + tc.resource + "]\n"
			}
			put(t, source, "workflows/main.yml", w)
			if tc.symlink {
				if err := os.Symlink(filepath.Join(source, "workflows/agents/task.yaml"), filepath.Join(source, "workflows/link.yaml")); err != nil {
					t.Fatal(err)
				}
				put(t, source, "workflows/main.yml", strings.ReplaceAll(w, "agents/task.yaml", "link.yaml"))
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
              primary: ../agents/task.yaml
              secondary: ../agents/other
          prompt: hello
        finally:
          - uses: agent/run
            with: {agent: ../agents/final.yaml, prompt: finish}
`)
	for _, p := range []string{"workflows/agents/task.yaml", "workflows/agents/other/definition.yaml", "workflows/agents/final.yaml"} {
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
	for _, p := range []string{"task.yaml", "other/definition.yaml", "final.yaml"} {
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

func TestPluginImportWorkspaceRelativeNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name, entry, callee string
	}{
		{"entry namespace", "examples/pr-fix/workflows/main.yml", "examples/pr-fix/workflows/child.yml"},
		{"sibling namespace", "examples/entry/workflows/main.yml", "examples/shared/workflows/child.yml"},
		{"repeated workflows segment", "workflows/team/workflows/main.yml", "workflows/team/workflows/child.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			put(t, source, tc.entry, "on: {workflow_call: {}}\njobs:\n  call:\n    uses: "+tc.callee+"\n")
			put(t, source, tc.callee, "on: {workflow_call: {}}\njobs:\n  task:\n    steps:\n      - uses: agent/run\n        with: {agent: agents/task.yaml, prompt: Review}\n")
			put(t, source, filepath.ToSlash(filepath.Join(filepath.Dir(tc.callee), "agents/task.yaml")), exampleAgent)
			put(t, source, "unrelated.txt", "do not import")
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", filepath.Join(source, tc.entry)); err != nil {
				t.Fatal(err)
			}
			d, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			all, err := (&workflows.Executor{WorkspaceDir: root}).Load(d.Skills[0].Workflow)
			if err != nil || len(all) != 2 {
				t.Fatalf("closure: %v %v", all, err)
			}
			for _, ref := range []string{tc.entry, tc.callee} {
				if _, err := os.Stat(filepath.Join(root, "imports/demo", ref)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "imports/demo/unrelated.txt")); !os.IsNotExist(err) {
				t.Fatalf("copied undeclared file: %v", err)
			}
		})
	}
}

func TestPluginRejectAmbiguousImportRoot(t *testing.T) {
	source := t.TempDir()
	entry := "nested/workflows/main.yml"
	put(t, source, entry, "on: {workflow_call: {}}\njobs:\n  call:\n    uses: workflows/child.yml\n")
	child := "on: {workflow_call: {}}\njobs:\n  task:\n    steps:\n      - uses: agent/run\n        with: {agent: agents/task.yaml, prompt: Review}\n"
	for _, prefix := range []string{"", "nested/"} {
		put(t, source, prefix+"workflows/child.yml", child)
		put(t, source, prefix+"workflows/agents/task.yaml", exampleAgent)
	}
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, entry)); err == nil || !strings.Contains(err.Error(), "ambiguous source root") {
		t.Fatalf("ambiguous closure accepted: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("failed import created destination: %v", err)
	}
}

func TestPluginImportPreservesEmptyDeclaredDirectoriesAndSkillFixtures(t *testing.T) {
	source := t.TempDir()
	put(t, source, "workflows/main.yml", "on: {workflow_call: {}}\njobs:\n  task:\n    container: {image: python:3, files: [empty]}\n    steps:\n      - uses: docker://python:3\n        files: [helpers]\n        with: {entrypoint: python, args: '-c pass'}\n")
	for _, name := range []string{"empty", "helpers/nested"} {
		if err := os.MkdirAll(filepath.Join(source, "workflows", name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml")); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join(root, "fixtures/example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddSkill(root, "example", filepath.Join(source, "workflows/main.yml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatal(err)
	}
	for _, skill := range []string{"demo", "example"} {
		for _, name := range []string{"empty", "helpers", "helpers/nested"} {
			info, err := os.Stat(filepath.Join(root, "imports", skill, "workflows", name))
			if err != nil || !info.IsDir() {
				t.Fatalf("lost %s/%s: %v", skill, name, err)
			}
		}
	}
	additional, err := os.ReadFile(filepath.Join(root, "fixtures/example/example.yaml"))
	if err != nil || !strings.Contains(string(additional), "cases: []") {
		t.Fatalf("missing imported skill fixture: %q %v", additional, err)
	}
	original, err := os.ReadFile(filepath.Join(root, "fixtures/example.yaml"))
	if err != nil || string(original) != string(fixture) {
		t.Fatalf("original fixture changed: %v", err)
	}
}

func TestPluginInitWriteAndCreateFailuresLeaveNoPartialProject(t *testing.T) {
	for _, failure := range []string{"write", "close", "create"} {
		t.Run(failure, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			disk := authoringIO()
			create := disk.create
			disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
				final := strings.HasPrefix(name, root+string(filepath.Separator)) && filepath.Base(name) == "SKILL.md"
				if final && failure == "create" {
					return nil, errors.New("injected create failure")
				}
				f, err := create(name, mode)
				if err != nil || !final {
					return f, err
				}
				return &failedAuthorFile{File: f.(*os.File), write: failure == "write", close: failure == "close"}, nil
			}
			if err := initProject(root, "demo", "0.2.2", "", disk); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("missing injected failure: %v", err)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatalf("failed init retained destination: %v", err)
			}
			if err := Init(root, "demo", "0.2.2", ""); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestPluginAddSkillWriteCloseAndDescriptorFailuresRollback(t *testing.T) {
	for _, failure := range []string{"write", "close", "descriptor", "descriptor-change"} {
		t.Run(failure, func(t *testing.T) {
			source := t.TempDir()
			put(t, source, "custom.yaml", exampleWorkflow)
			put(t, source, "agents/task.yaml", exampleAgent)
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", ""); err != nil {
				t.Fatal(err)
			}
			put(t, root, "notes.txt", "keep")
			before, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
			expected := before
			wantError := "injected"
			if failure == "descriptor-change" {
				expected = append([]byte("# concurrent descriptor edit\n"), before...)
				wantError = "changed during import"
			}
			disk := authoringIO()
			create := disk.create
			if failure == "descriptor" {
				disk.rename = func(_, _ string) error { return errors.New("injected descriptor failure") }
			} else {
				disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
					f, err := create(name, mode)
					if err != nil || filepath.Base(name) != "SKILL.md" {
						return f, err
					}
					wrapped := &failedAuthorFile{File: f.(*os.File), write: failure == "write", close: failure == "close"}
					if failure == "descriptor-change" {
						wrapped.onWrite = func() { put(t, root, DescriptorFile, string(expected)) }
					}
					return wrapped, nil
				}
			}
			if err := addSkill(root, "retry", filepath.Join(source, "custom.yaml"), disk); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("missing injected failure: %v", err)
			}
			for _, name := range []string{"imports/retry", "skills/retry", "fixtures/retry", ".plugin-author.lock"} {
				if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatalf("failed import retained %s: %v", name, err)
				}
			}
			after, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
			if !bytes.Equal(expected, after) {
				t.Fatal("failed import changed descriptor")
			}
			if note, err := os.ReadFile(filepath.Join(root, "notes.txt")); err != nil || string(note) != "keep" {
				t.Fatalf("unrelated file changed: %q %v", note, err)
			}
			if _, err := Load(root); err != nil {
				t.Fatal(err)
			}
			if err := AddSkill(root, "retry", filepath.Join(source, "custom.yaml")); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestPluginRollbackPreservesConcurrentUnrelatedFiles(t *testing.T) {
	source := t.TempDir()
	put(t, source, "custom.yaml", exampleWorkflow)
	put(t, source, "agents/task.yaml", exampleAgent)
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
	disk := authoringIO()
	create := disk.create
	disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
		f, err := create(name, mode)
		if err != nil || filepath.Base(name) != "SKILL.md" {
			return f, err
		}
		return &failedAuthorFile{File: f.(*os.File), write: true, onWrite: func() {
			put(t, root, "imports/retry/workflows/keep.txt", "concurrent unrelated file")
		}}, nil
	}
	err := addSkill(root, "retry", filepath.Join(source, "custom.yaml"), disk)
	if err == nil || !strings.Contains(err.Error(), "rollback newly created paths") {
		t.Fatalf("missing retained-path diagnostic: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "imports/retry/workflows/keep.txt")); err != nil || string(b) != "concurrent unrelated file" {
		t.Fatalf("removed unrelated concurrent file: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "imports/retry/workflows/custom.yaml")); !os.IsNotExist(err) {
		t.Fatalf("retained own imported file: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, DescriptorFile))
	if !bytes.Equal(before, after) {
		t.Fatal("failed import changed descriptor")
	}
}

func TestPluginImportExplicitCurrentDirectoryHelpers(t *testing.T) {
	for _, kind := range []string{"container", "step", "standalone"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			entry := "workflows/main.yml"
			child := "workflows/nested/child.yml"
			put(t, source, entry, "on: {workflow_call: {}}\njobs:\n  call:\n    uses: "+child+"\n")
			declaration := "    container: {image: python:3, files: [.]}\n    steps:\n      - run: python helper.py\n"
			if kind != "container" {
				declaration = "    steps:\n      - uses: docker://python:3\n        files: [.]\n        with: {entrypoint: python, args: helper.py}\n"
			}
			if kind == "standalone" {
				entry, child = "custom.yaml", "custom.yaml"
			}
			put(t, source, child, "on: {workflow_call: {}}\njobs:\n  task:\n"+declaration)
			base := filepath.Dir(child)
			put(t, source, filepath.Join(base, "agents/task.yaml"), exampleAgent)
			put(t, source, filepath.Join(base, "helper.py"), "print('declared')\n")
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", filepath.Join(source, entry)); err != nil {
				t.Fatal(err)
			}
			d, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&workflows.Executor{WorkspaceDir: root}).Load(d.Skills[0].Workflow); err != nil {
				t.Fatal(err)
			}
			prefix := "imports/demo"
			if kind == "standalone" {
				prefix += "/workflows"
			}
			if _, err := os.Stat(filepath.Join(root, prefix, base, "helper.py")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPluginAddSkillPreservesDescriptorEditAfterCapture(t *testing.T) {
	source := t.TempDir()
	put(t, source, "custom.yaml", exampleWorkflow)
	put(t, source, "agents/task.yaml", exampleAgent)
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	disk := authoringIO()
	load := disk.load
	var edited []byte
	disk.load = func(root string) (*Descriptor, *collector, error) {
		descriptor, captured, err := load(root)
		if err != nil {
			return nil, nil, err
		}
		// Change a valid descriptor immediately after it was parsed, before the
		// import or any destination writes. A late baseline misses this edit.
		concurrent := *descriptor
		concurrent.Version = "0.2.0"
		edited, err = yaml.Marshal(&concurrent)
		if err == nil {
			err = os.WriteFile(filepath.Join(root, DescriptorFile), edited, 0600)
		}
		return descriptor, captured, err
	}
	if err := addSkill(root, "second", filepath.Join(source, "custom.yaml"), disk); err == nil || !strings.Contains(err.Error(), "changed during import") {
		t.Fatalf("missed concurrent descriptor edit: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(root, DescriptorFile))
	if err != nil || !bytes.Equal(current, edited) {
		t.Fatalf("overwrote concurrent descriptor edit: %s (%v)", current, err)
	}
	for _, name := range []string{"imports/second", "skills/second", "fixtures/second", ".plugin-author.lock"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("failed import retained %s: %v", name, err)
		}
	}
}

func TestPluginValidationPreservesReplacedStage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	disk := authoringIO()
	create := disk.create
	var stage string
	var replacement fs.FileInfo
	disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
		if stage == "" {
			// The first scaffold file is README.md in the private stage root.
			stage = filepath.Dir(name)
			if !strings.HasPrefix(filepath.Base(stage), ".plugin-validate-") {
				t.Fatalf("unexpected validation path: %s", name)
			}
			if err := os.Rename(stage, stage+"-original"); err != nil {
				return nil, err
			}
			if err := os.Mkdir(stage, 0700); err != nil {
				return nil, err
			}
			var err error
			replacement, err = os.Lstat(stage)
			if err != nil {
				return nil, err
			}
		}
		return create(name, mode)
	}
	if err := initProject(root, "demo", "0.2.2", "", disk); err == nil || !strings.Contains(err.Error(), "changed identity; retained during rollback") {
		t.Fatalf("removed or missed replaced stage: %v", err)
	}
	current, err := os.Lstat(stage)
	if err != nil || !os.SameFile(replacement, current) {
		t.Fatalf("replaced stage was removed: %v", err)
	}
	entries, err := os.ReadDir(stage)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned stage children were not cleaned: %v (%v)", entries, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("failed validation reserved destination: %v", err)
	}
}

func TestPluginImportPreservesEmptyJobUses(t *testing.T) {
	for _, uses := range []string{"''", "null", "~"} {
		t.Run(uses, func(t *testing.T) {
			source := t.TempDir()
			workflow := "on: {workflow_call: {}}\njobs: {main: {uses: " + uses + ", steps: [{argv: [echo, hello]}]}}\n"
			put(t, source, "custom.yaml", workflow)
			parsed, err := workflows.Parse([]byte(workflow))
			if err != nil {
				t.Fatal(err)
			}
			if err = workflows.Validate(parsed); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", filepath.Join(source, "custom.yaml")); err != nil {
				t.Fatal(err)
			}
			descriptor, err := Load(root)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := (&workflows.Executor{WorkspaceDir: root}).Load(descriptor.Skills[0].Workflow)
			if err != nil || loaded[descriptor.Skills[0].Workflow].Jobs["main"].Uses != "" || len(loaded[descriptor.Skills[0].Workflow].Jobs["main"].Steps) != 1 {
				t.Fatalf("changed local job: %+v (%v)", loaded, err)
			}
		})
	}
}

func TestPluginImportReadOnlyHelperDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory modes do not enforce POSIX owner write permissions")
	}
	source := t.TempDir()
	put(t, source, "custom.yaml", "on: {workflow_call: {}}\njobs: {main: {container: {image: python:3, files: [helpers]}, steps: [{argv: [python, helpers/report.py]}]}}\n")
	put(t, source, "helpers/report.py", "print('declared')\n")
	if err := os.Chmod(filepath.Join(source, "helpers"), 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(source, "helpers"), 0755) })
	root := filepath.Join(t.TempDir(), "demo")
	helper := filepath.Join(root, "imports/demo/workflows/helpers")
	t.Cleanup(func() { _ = os.Chmod(helper, 0755) })
	disk := authoringIO()
	create := disk.create
	writes := 0
	disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
		if filepath.Base(name) == "report.py" {
			parent, err := os.Stat(filepath.Dir(name))
			if err != nil {
				return nil, err
			}
			// Assert ordinary-user creation permissions even when run as root.
			if parent.Mode().Perm()&0300 != 0300 {
				return nil, fmt.Errorf("helper directory is not writable/searchable during population: %o", parent.Mode().Perm())
			}
			writes++
		}
		return create(name, mode)
	}
	if err := initProject(root, "demo", "0.2.2", filepath.Join(source, "custom.yaml"), disk); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(helper)
	if err != nil || info.Mode().Perm() != 0555 || writes != 2 {
		t.Fatalf("helper modes/writes: %v, %d (%v)", info, writes, err)
	}
	contents, err := os.ReadFile(filepath.Join(helper, "report.py"))
	if err != nil || string(contents) != "print('declared')\n" {
		t.Fatalf("helper contents: %q (%v)", contents, err)
	}
	// A later descriptor failure must still roll back files inside finalized
	// read-only directories, preserving the existing project and source tree.
	disk.rename = func(_, _ string) error { return errors.New("injected descriptor failure") }
	if err := addSkill(root, "second", filepath.Join(source, "custom.yaml"), disk); err == nil || !strings.Contains(err.Error(), "injected descriptor failure") {
		t.Fatalf("missing descriptor failure: %v", err)
	}
	for _, name := range []string{"imports/second", "skills/second", "fixtures/second"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("rollback retained %s: %v", name, err)
		}
	}
}
