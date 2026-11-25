package plugins

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Init creates a new authoring project. The caller supplies the exact engine
// release used by the CLI; plugin versions are independent of engine versions.
func Init(directory, name, engineVersion, workflow string) error {
	if !identity.MatchString(name) {
		return fmt.Errorf("invalid plugin name %q", name)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: destination already exists", directory)
	}
	files := map[string]file{}
	d := &Descriptor{SchemaVersion: SchemaVersion, Name: name, Version: "0.1.0", EngineVersion: engineVersion, Hosts: []string{"codex", "claude"}}
	if workflow == "" {
		files["workflows/main.yml"] = file{data: []byte(exampleWorkflow), mode: 0644}
		files["workflows/agents/task.yaml"] = file{data: []byte(exampleAgent), mode: 0644}
		d.Skills = []Skill{{Name: name, Path: path.Join("skills", name, "SKILL.md"), Workflow: "workflows/main.yml"}}
	} else {
		skill, imported, err := importWorkflow(name, workflow)
		if err != nil {
			return err
		}
		d.Skills = []Skill{skill}
		for p, f := range imported {
			files[p] = f
		}
	}
	files[d.Skills[0].Path] = file{data: skillText(name), mode: 0644}
	files["config/agents.codex.example.yml"] = file{data: []byte(codexTemplate), mode: 0644}
	files["config/agents.claude.example.yml"] = file{data: []byte(claudeTemplate), mode: 0644}
	// This stage defines fixture source data. The plugin test interpreter is a
	// later stage; importing an unknown workflow must not invent passing behavior.
	files["fixtures/example.yaml"] = file{data: []byte(fixtureText(name, workflow != "")), mode: 0644}
	files["README.md"] = file{data: []byte(projectReadme), mode: 0644}
	if err := d.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	files[DescriptorFile] = file{data: data, mode: 0644}
	// Build and validate off to the side; a failed import never leaves a project.
	parent := filepath.Dir(directory)
	stage, err := os.MkdirTemp(parent, ".plugin-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = writeFiles(stage, files); err != nil {
		return err
	}
	if _, err = Load(stage); err != nil {
		return err
	}
	// Mkdir reserves the destination without replacing an existing empty directory.
	if err = os.Mkdir(directory, 0755); err != nil {
		return err
	}
	if err = writeFiles(directory, files); err != nil {
		return err
	}
	return nil
}

// AddSkill imports only the workflow dependency closure, preserving every
// existing source file. New skills get isolated namespaces; collisions fail.
func AddSkill(root, name, workflow string) error {
	if !identity.MatchString(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	// Reserve an exclusive project lock; no descriptor update races another writer.
	lock, err := os.OpenFile(filepath.Join(root, ".plugin-author.lock"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("plugin authoring lock: %w", err)
	}
	lock.Close()
	defer os.Remove(filepath.Join(root, ".plugin-author.lock"))
	d, err := Load(root)
	if err != nil {
		return err
	}
	for _, s := range d.Skills {
		if s.Name == name {
			return fmt.Errorf("%s: skill %q already exists", DescriptorFile, name)
		}
	}
	skill, files, err := importWorkflow(name, workflow)
	if err != nil {
		return err
	}
	files[skill.Path] = file{data: skillText(name), mode: 0644}
	d.Skills = append(d.Skills, skill)
	if err = d.Validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	original, err := os.ReadFile(filepath.Join(root, DescriptorFile))
	if err != nil {
		return err
	}
	for p := range files {
		if err := checkNewPath(root, p); err != nil {
			return err
		}
	}
	// Prepare the updated descriptor before writing new sources, then rename it
	// atomically only after all exclusive file creations succeed.
	tmp, err := os.CreateTemp(root, ".plugin-descriptor-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = writeFiles(root, files); err != nil {
		return err
	}
	latest, err := os.ReadFile(filepath.Join(root, DescriptorFile))
	if err != nil {
		return err
	}
	if string(original) != string(latest) {
		return fmt.Errorf("%s changed during import; imported sources retained without rebinding", DescriptorFile)
	}
	return os.Rename(tmpName, filepath.Join(root, DescriptorFile))
}

func importWorkflow(name, source string) (Skill, map[string]file, error) {
	source, err := filepath.Abs(source)
	if err != nil {
		return Skill{}, nil, err
	}
	// The workspace's nearest workflows/ ancestor defines local reusable refs.
	base := filepath.Dir(source)
	root := base
	prefix := path.Join("imports", name, "workflows")
	for probe := base; ; probe = filepath.Dir(probe) {
		if filepath.Base(probe) == "workflows" {
			root = filepath.Dir(probe)
			prefix = path.Join("imports", name)
			break
		}
		if filepath.Dir(probe) == probe {
			break
		}
	}
	rel, err := filepath.Rel(root, source)
	if err != nil {
		return Skill{}, nil, err
	}
	entry := filepath.ToSlash(rel)
	c := newCollector(root)
	if strings.Contains(entry, "/workflows/") || strings.HasPrefix(entry, "workflows/") {
		if err = c.workflow(entry, 0); err != nil {
			return Skill{}, nil, err
		}
	} else {
		// A standalone FILE has the same relative asset resolution as its containing
		// directory. Alias its entry under workflows/ for the canonical CLI contract.
		if err = c.visit(entry, 0); err != nil {
			return Skill{}, nil, err
		}
	}
	files := map[string]file{}
	for p, f := range c.files {
		if c.loaded[p] {
			f.data, err = rewriteWorkflow(f.data, prefix)
			if err != nil {
				return Skill{}, nil, err
			}
		}
		files[path.Join(prefix, p)] = f
	}
	return Skill{Name: name, Path: path.Join("skills", name, "SKILL.md"), Workflow: path.Join(prefix, entry)}, files, nil
}

func checkNewPath(root, name string) error {
	p := root
	for _, part := range strings.Split(name, "/") {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: destination symlink is unsupported", p)
		}
		if p == filepath.Join(root, filepath.FromSlash(name)) || !info.IsDir() {
			return fmt.Errorf("%s: destination already exists", p)
		}
	}
	return nil
}
func writeFiles(root string, files map[string]file) error {
	names := make([]string, 0, len(files))
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if err := checkNewPath(root, p); err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(files[p].mode))
		if err != nil {
			return err
		}
		_, err = f.Write(files[p].data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
func skillText(name string) []byte {
	return []byte("---\nname: " + name + "\ndescription: Run the " + name + " workflow against the target workspace.\n---\n\nRun the bound workflow with inputs declared in its workflow_call contract.\nKeep runtime permissions and credentials in the target workspace's agents.yml.\n")
}
func fixtureText(name string, imported bool) string {
	if imported {
		return "schema-version: 1\n# Add scripted responses and assertions for the imported workflow before plugin test.\ncases: []\n"
	}
	return "schema-version: 1\ncases:\n  - name: success\n    skill: " + name + "\n    inputs: {task: Say hello}\n    operations:\n      - uses: agent/run\n        result: {text: Hello}\n    expect:\n      state: completed\n      invocation-count: 1\n"
}

const exampleWorkflow = `name: Example workflow skill
on:
  workflow_call:
    inputs:
      task:
        type: string
        required: true
    outputs:
      result:
        value: ${{ jobs.main.outputs.result }}
jobs:
  main:
    outputs:
      result: ${{ steps.task.outputs.text }}
    steps:
      - id: task
        uses: agent/run
        with:
          agent: agents/task.yaml
          prompt: ${{ inputs.task }}
`
const exampleAgent = `name: "task"
description: "Complete the supplied workflow task."
instructions: "Complete the supplied task. Respect the target workspace permissions."
`
const codexTemplate = `# Example only: review and copy into the TARGET workspace's agents.yml.
# Scaffolding never changes target configuration or invokes a provider.
agents:
  codex:
    provider: codex-app-server
    authentication: {mode: native}
    sandbox: read-only
`
const claudeTemplate = `# Example only: native agent/run provider selection arrives in stage 2.
# Set bridge to a separately installed, locked Claude SDK bridge.
# Supply an existing token via the named environment variable; do not log in implicitly.
agents:
  claude:
    provider: claude-sdk
    bridge: /absolute/path/to/bridge/claude/bridge.mjs
    authentication: {mode: native, credential-env: CLAUDE_CODE_OAUTH_TOKEN}
    tools: []
`
const projectReadme = `# Workflow plugin project

Developer-owned sources are plugin.yaml, skills/, workflows/ (or imports/),
config/ and fixtures/. Platform manifests and launchers will be generated by the
packaging stage into a separate output directory; do not hand-maintain them.

SKILL.md owns discovery names, descriptions and instructions. The descriptor binds
each skill to a workflow. Workflow on.workflow_call.inputs owns the input contract.
Plugin version and exact engine-version are independent semantic versions.

The example uses agent/run with a portable YAML definition. Edit the workflow and
skill instructions, and maintain scripted fixture responses and assertions.
Imported workflows receive an empty fixture template that needs authored cases.
Fixtures are deterministic control-flow evidence, not live host/model acceptance.
The fixture runner and generated host packaging are delivered in later stages.

Provider credentials, runtime limits and permissions belong to the target
workspace's agents.yml. config/ contains examples only and is never installed as
authority. Host selection (codex/claude) is independent from execution provider.
Scaffolding does not install dependencies, contact services or start a provider.
`
