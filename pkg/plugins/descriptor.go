// Package plugins implements the offline workflow-plugin authoring contract.
package plugins

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

const DescriptorFile = "plugin.yaml"
const SchemaVersion = 1

// Descriptor contains distribution bindings, never workspace execution authority.
// Discovery metadata lives in each skill's SKILL.md; inputs live in its workflow.
type Descriptor struct {
	SchemaVersion int      `yaml:"schema-version" json:"schema-version"`
	Name          string   `yaml:"name" json:"name"`
	Version       string   `yaml:"version" json:"version"`
	EngineVersion string   `yaml:"engine-version" json:"engine-version"`
	Hosts         []string `yaml:"hosts" json:"hosts"`
	Skills        []Skill  `yaml:"skills" json:"skills"`
	Resources     []string `yaml:"resources,omitempty" json:"resources,omitempty"`
}
type Skill struct {
	Name     string `yaml:"name" json:"name"`
	Path     string `yaml:"path" json:"path"`
	Workflow string `yaml:"workflow" json:"workflow"`
}
type Metadata struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
}

var identity = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
var version = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

func exactVersion(value string) bool {
	if !version.MatchString(value) {
		return false
	}
	core := strings.SplitN(value, "+", 2)[0]
	if _, pre, ok := strings.Cut(core, "-"); ok {
		for _, part := range strings.Split(pre, ".") {
			numeric := true
			for _, r := range part {
				if r < '0' || r > '9' {
					numeric = false
					break
				}
			}
			if numeric && len(part) > 1 && part[0] == '0' {
				return false
			}
		}
	}
	return true
}

func strictYAML(data []byte, out any) error {
	var node yaml.Node
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&node); err != nil {
		return err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one YAML document")
	}
	var walk func(*yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.AliasNode {
			return fmt.Errorf("YAML aliases are unsupported")
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&node); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}

// Parse validates descriptor metadata without reading files or invoking providers.
func Parse(data []byte) (*Descriptor, error) {
	var d Descriptor
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("%s: exceeds 1 MiB", DescriptorFile)
	}
	if err := strictYAML(data, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", DescriptorFile, err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", DescriptorFile, err)
	}
	return &d, nil
}
func (d *Descriptor) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema-version %d; expected %d", d.SchemaVersion, SchemaVersion)
	}
	if !identity.MatchString(d.Name) {
		return fmt.Errorf("name must be a lowercase hyphenated identity")
	}
	if !exactVersion(d.Version) || !exactVersion(d.EngineVersion) {
		return fmt.Errorf("version and engine-version must be exact semantic versions (without v or ranges)")
	}
	hosts := map[string]bool{}
	for _, h := range d.Hosts {
		if h != "codex" && h != "claude" || hosts[h] {
			return fmt.Errorf("unsupported or duplicate host %q", h)
		}
		hosts[h] = true
	}
	if len(hosts) == 0 || len(d.Skills) == 0 {
		return fmt.Errorf("hosts and skills must be nonempty")
	}
	names, paths, bindings := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range d.Skills {
		if !identity.MatchString(s.Name) || names[s.Name] {
			return fmt.Errorf("invalid or duplicate skill identity %q", s.Name)
		}
		skillPath, err := localPath(s.Path)
		if err != nil {
			return fmt.Errorf("skill %s path: %w", s.Name, err)
		}
		if filepath.Ext(s.Path) != ".md" || filepath.Base(s.Path) != "SKILL.md" {
			return fmt.Errorf("skill %s path must name SKILL.md", s.Name)
		}
		canonical, err := workflows.CanonicalLocalRef(s.Workflow)
		if err != nil {
			return fmt.Errorf("skill %s workflow: %w", s.Name, err)
		}
		if paths[skillPath] || bindings[canonical] {
			return fmt.Errorf("skill %s duplicates a path or workflow binding", s.Name)
		}
		names[s.Name], paths[skillPath], bindings[canonical] = true, true, true
	}
	for _, r := range d.Resources {
		if _, err := localPath(r); err != nil {
			return fmt.Errorf("resource %q: %w", r, err)
		}
	}
	return nil
}

func ReadMetadata(data []byte) (*Metadata, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("SKILL.md requires YAML frontmatter")
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("SKILL.md requires closing frontmatter delimiter")
	}
	var meta Metadata
	if err := strictYAML([]byte(text[4:4+end]), &meta); err != nil {
		return nil, err
	}
	if !identity.MatchString(meta.Name) || strings.TrimSpace(meta.Description) == "" || strings.TrimSpace(text[4+end+5:]) == "" {
		return nil, fmt.Errorf("SKILL.md requires name, description and instructions")
	}
	return &meta, nil
}

// Load validates metadata, bindings and the complete declared dependency closure.
func Load(root string) (*Descriptor, error) {
	return loadCollected(newCollector(root))
}

func loadCaptured(root string) (*Descriptor, *collector, error) {
	files := newCollector(root)
	descriptor, err := loadCollected(files)
	if err != nil {
		return nil, nil, err
	}
	return descriptor, files, nil
}

func loadCollected(files *collector) (*Descriptor, error) {
	b, err := files.read(DescriptorFile)
	if err != nil {
		return nil, err
	}
	d, err := Parse(b)
	if err != nil {
		return nil, err
	}
	for _, s := range d.Skills {
		b, err := files.read(s.Path)
		if err != nil {
			return nil, err
		}
		m, err := ReadMetadata(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Path, err)
		}
		if m.Name != s.Name {
			return nil, fmt.Errorf("%s: name %q does not match binding %q", s.Path, m.Name, s.Name)
		}
		if err := files.workflow(s.Workflow, 0); err != nil {
			return nil, err
		}
	}
	for _, r := range d.Resources {
		if err := files.resource(r); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Inputs returns the workflow declarations directly, without a duplicate schema.
func Inputs(root string, skill Skill) (map[string]workflows.Input, error) {
	c := newCollector(root)
	b, err := c.read(skill.Workflow)
	if err != nil {
		return nil, err
	}
	w, err := workflows.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", skill.Workflow, err)
	}
	if w.On.WorkflowCall == nil {
		return nil, nil
	}
	return w.On.WorkflowCall.Inputs, nil
}

// ReadDescriptor reads the source descriptor; use Load to validate project files.
func ReadDescriptor(root string) (*Descriptor, error) {
	b, err := os.ReadFile(filepath.Join(root, DescriptorFile))
	if err != nil {
		return nil, err
	}
	return Parse(b)
}
