package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

func TestPluginImportEncodedJobUses(t *testing.T) {
	source := t.TempDir()
	put(t, source, "workflows/main.yml", "on: {workflow_call: {}}\njobs: {child: {uses: !!binary d29ya2Zsb3dzL2NoaWxkLnltbA==}}\n")
	put(t, source, "workflows/child.yml", "on: {workflow_call: {}}\njobs: {main: {steps: [{argv: [echo, hello]}]}}\n")
	if _, err := (&workflows.Executor{WorkspaceDir: source}).Load("workflows/main.yml"); err != nil {
		t.Fatalf("source is not engine-valid: %v", err)
	}
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml")); err != nil {
		t.Fatal(err)
	}
	descriptor, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := (&workflows.Executor{WorkspaceDir: root}).Load(descriptor.Skills[0].Workflow)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("imported encoded reference: %v (%v)", loaded, err)
	}
	if got := loaded[descriptor.Skills[0].Workflow].Jobs["child"].Uses; got != "imports/demo/workflows/child.yml" {
		t.Fatalf("rewritten reference %q", got)
	}
}

func TestCollectorAgentValidatesCapturedBytes(t *testing.T) {
	for _, validCapture := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid capture", true: "valid capture"}[validCapture], func(t *testing.T) {
			root := t.TempDir()
			captured, replacement := "name: invalid\n", exampleAgent
			if validCapture {
				captured, replacement = replacement, captured
			}
			put(t, root, "task.yaml", captured)
			collector := newCollector(root)
			if _, err := collector.read("task.yaml"); err != nil {
				t.Fatal(err)
			}
			// A dependency can have been collected as an explicit resource before
			// a concurrent edit changes the pathname used for agent admission.
			put(t, root, "task.yaml", replacement)
			err := collector.agent("task.yaml")
			if (err == nil) != validCapture {
				t.Fatalf("validated replacement rather than captured bytes: %v", err)
			}
			if got := string(collector.files["task.yaml"].data); got != captured {
				t.Fatalf("changed captured bytes: %q", got)
			}
		})
	}
}

func TestPluginLoadChecksEveryReusableEdge(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "callee first", true: "caller first"}[reverse], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", ""); err != nil {
				t.Fatal(err)
			}
			descriptor, err := ReadDescriptor(root)
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, "workflows/main.yml", "jobs: {main: {steps: [{argv: [echo, hello]}]}}\n")
			put(t, root, "workflows/caller.yml", "on: {workflow_call: {}}\njobs: {child: {uses: workflows/main.yml}}\n")
			put(t, root, "skills/caller/SKILL.md", string(skillText("caller")))
			descriptor.Skills = append(descriptor.Skills, Skill{Name: "caller", Path: "skills/caller/SKILL.md", Workflow: "workflows/caller.yml"})
			if reverse {
				descriptor.Skills[0], descriptor.Skills[1] = descriptor.Skills[1], descriptor.Skills[0]
			}
			data, err := yaml.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, DescriptorFile), data, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "workflows/main.yml: plugin workflow requires on.workflow_call") {
				t.Fatalf("accepted invalid reusable edge: %v", err)
			}
		})
	}
}

func TestPluginImportPreservesDefaultLeadingNewline(t *testing.T) {
	source := t.TempDir()
	put(t, source, "custom.yaml", `on:
  workflow_call:
    inputs:
      payload:
        type: string
        default: |-

          payload
jobs:
  main:
    steps:
      - argv: [printf, '%s', '${{ inputs.payload }}']
`)
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", filepath.Join(source, "custom.yaml")); err != nil {
		t.Fatal(err)
	}
	descriptor, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, descriptor.Skills[0].Workflow))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := workflows.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := imported.On.WorkflowCall.Inputs["payload"].Default; got != "\npayload" {
		t.Fatalf("default changed: got %q, want %q", got, "\npayload")
	}
}

func TestPluginImportPreservesBlockScalarValues(t *testing.T) {
	for _, command := range []string{"init", "add-skill"} {
		for _, style := range []string{"|-", "|", "|+", ">-", ">", ">+", "!!str |-", "!message |-"} {
			t.Run(command+"/"+style, func(t *testing.T) {
				source := t.TempDir()
				workflow := strings.ReplaceAll(`on:
  workflow_call:
    inputs:
      payload:
        type: string
        default: STYLE

          payload

jobs:
  call:
    uses: workflows/child.yml
  main:
    steps:
      - argv:
          - printf
          - STYLE

            command

      - uses: agent/run
        with:
          agent: agents/task.yaml
          prompt: STYLE


            prompt

        finally:
          - argv:
              - printf
              - STYLE


                lifecycle

`, "STYLE", style)
				put(t, source, "workflows/main.yml", workflow)
				put(t, source, "workflows/child.yml", "on: {workflow_call: {}}\njobs: {main: {steps: [{argv: [echo, child]}]}}\n")
				put(t, source, "workflows/agents/task.yaml", exampleAgent)
				original, err := workflows.Parse([]byte(workflow))
				if err != nil {
					t.Fatal(err)
				}
				if err := workflows.Validate(original); err != nil {
					t.Fatal(err)
				}
				if value, ok := original.On.WorkflowCall.Inputs["payload"].Default.(string); !ok || !strings.HasPrefix(value, "\n") {
					t.Fatalf("fixture lost its leading newline: %q", value)
				}
				root := filepath.Join(t.TempDir(), "demo")
				skillName := "demo"
				if command == "init" {
					err = Init(root, "demo", "0.2.2", filepath.Join(source, "workflows/main.yml"))
				} else {
					err = Init(root, "demo", "0.2.2", "")
					if err == nil {
						skillName = "extra"
						err = AddSkill(root, skillName, filepath.Join(source, "workflows/main.yml"))
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				descriptor, err := Load(root)
				if err != nil {
					t.Fatal(err)
				}
				importedBytes, err := os.ReadFile(filepath.Join(root, descriptor.Skills[len(descriptor.Skills)-1].Workflow))
				if err != nil {
					t.Fatal(err)
				}
				imported, err := workflows.Parse(importedBytes)
				if err != nil {
					t.Fatal(err)
				}
				values := func(workflow *workflows.Workflow) map[string]any {
					steps := workflow.Jobs["main"].Steps
					return map[string]any{"default": workflow.On.WorkflowCall.Inputs["payload"].Default, "command": steps[0].Argv[1], "lifecycle": steps[1].Finally[0].Argv[1], "prompt": steps[1].With["prompt"]}
				}
				got := values(imported)
				for name, want := range values(original) {
					if !reflect.DeepEqual(got[name], want) {
						t.Errorf("%s changed: got %q, want %q", name, got[name], want)
					}
				}
				if ref := imported.Jobs["call"].Uses; ref != "imports/"+skillName+"/workflows/child.yml" {
					t.Fatalf("reusable reference not rewritten: %s", ref)
				}
				if current, err := os.ReadFile(filepath.Join(source, "workflows/main.yml")); err != nil || string(current) != workflow {
					t.Fatalf("source changed: %v", err)
				}
			})
		}
	}
}
