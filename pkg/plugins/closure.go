package plugins

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

type file struct {
	data []byte
	mode fs.FileMode
}
type collector struct {
	root   string
	files  map[string]file
	active map[string]bool
	loaded map[string]bool
	total  int
}

func newCollector(root string) *collector {
	return &collector{root: root, files: map[string]file{}, active: map[string]bool{}, loaded: map[string]bool{}}
}
func localPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.Contains(name, "${{") || path.IsAbs(name) {
		return "", fmt.Errorf("requires a literal relative path")
	}
	for _, p := range strings.Split(name, "/") {
		if p == ".." {
			return "", fmt.Errorf("path escapes source root")
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("whole source root cannot be declared")
	}
	return clean, nil
}
func (c *collector) read(name string) ([]byte, error) {
	name, err := localPath(name)
	if err != nil {
		return nil, err
	}
	if f, ok := c.files[name]; ok {
		return f.data, nil
	}
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := checkSourcePath(root, name); err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > 32<<20 {
		return nil, fmt.Errorf("%s: requires a regular file of at most 32 MiB", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return nil, fmt.Errorf("%s: requires a regular file of at most 32 MiB", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	c.total += len(data)
	if len(data) > 32<<20 || c.total > 32<<20 || len(c.files) >= 4096 {
		return nil, fmt.Errorf("%s: dependency closure exceeds 32 MiB or 4096 files", name)
	}
	c.files[name] = file{data: data, mode: info.Mode().Perm()}
	return data, nil
}

// checkSourcePath rejects symlinks before reading or enumerating dependencies.
func checkSourcePath(root *os.Root, name string) error {
	p := ""
	for _, part := range strings.Split(name, "/") {
		p = path.Join(p, part)
		info, err := root.Lstat(p)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are unsupported", p)
		}
	}
	return nil
}
func (c *collector) safe(name string) error {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return checkSourcePath(root, name)
}
func relativeAgent(base, reference string) (string, error) {
	if reference == "" || path.IsAbs(reference) || strings.ContainsAny(reference, "\\\x00") || strings.Contains(reference, "${{") {
		return "", fmt.Errorf("agent requires a literal relative path")
	}
	return localPath(path.Join(base, reference))
}
func (c *collector) resource(name string) error {
	return c.resourcePath(name, false)
}

func (c *collector) helper(base, reference string) error {
	if reference == "" || path.IsAbs(reference) || strings.ContainsAny(reference, "\\\x00") || strings.Contains(reference, "${{") {
		return fmt.Errorf("helper requires a literal relative path")
	}
	for _, part := range strings.Split(reference, "/") {
		if part == ".." {
			return fmt.Errorf("helper path traversal is unsupported")
		}
	}
	// An explicit '.' declares the owning workflow directory, including when
	// that directory is the selected source root. No dependency is inferred.
	return c.resourcePath(path.Join(base, reference), true)
}

func (c *collector) resourcePath(name string, allowRoot bool) error {
	var err error
	if name != "." || !allowRoot {
		name, err = localPath(name)
	}
	if err != nil {
		return err
	}
	if err = c.safe(name); err != nil {
		return err
	}
	// WalkDir does not follow links; read rejects links in all ancestor components.
	return filepath.WalkDir(filepath.Join(c.root, filepath.FromSlash(name)), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		rel, err := filepath.Rel(c.root, p)
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: resource symlinks are unsupported", rel)
		}
		if e.IsDir() {
			name := filepath.ToSlash(rel)
			if _, exists := c.files[name]; exists {
				return nil
			}
			if len(c.files) >= 4096 {
				return fmt.Errorf("%s: dependency closure exceeds 4096 entries", name)
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			c.files[name] = file{mode: fs.ModeDir | info.Mode().Perm()}
			return nil
		}
		_, err = c.read(filepath.ToSlash(rel))
		return err
	})
}
func (c *collector) agent(name string) error {
	name, err := localPath(name)
	if err != nil {
		return err
	}
	if err = c.safe(name); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(name)))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(filepath.Join(c.root, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		var candidates []string
		for _, e := range entries {
			if !e.IsDir() && (strings.EqualFold(path.Ext(e.Name()), ".yaml") || strings.EqualFold(path.Ext(e.Name()), ".yml")) {
				candidates = append(candidates, path.Join(name, e.Name()))
			}
		}
		if len(candidates) != 1 {
			return fmt.Errorf("%s: agent directory requires exactly one YAML definition", name)
		}
		name = candidates[0]
	}
	if _, err = c.read(name); err != nil {
		return err
	}
	_, err = workflows.LoadAgentDefinition(filepath.Join(c.root, filepath.FromSlash(name)))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
func (c *collector) steps(base string, steps []workflows.Step) error {
	for _, s := range steps {
		for _, r := range s.Files {
			if err := c.helper(base, r); err != nil {
				return err
			}
		}
		if s.Uses == "agent/run" {
			switch v := s.With["agent"].(type) {
			case string:
				trimmed := strings.TrimSpace(v)
				if strings.Contains(v, "${{") && !(strings.HasPrefix(trimmed, "${{") && strings.HasSuffix(trimmed, "}}") && strings.Count(trimmed, "${{") == 1) {
					return fmt.Errorf("agent reference must be literal or a whole session expression")
				}
				if !strings.Contains(v, "${{") {
					name, err := relativeAgent(base, v)
					if err != nil {
						return err
					}
					if err := c.agent(name); err != nil {
						return err
					}
				}
			case map[string]any:
				definitions, ok := v["definitions"].(map[string]any)
				if !ok {
					return fmt.Errorf("agent selection requires literal definitions")
				}
				for _, r := range definitions {
					name, ok := r.(string)
					if !ok {
						return fmt.Errorf("agent definition must be a literal path")
					}
					target, err := relativeAgent(base, name)
					if err != nil {
						return err
					}
					if err := c.agent(target); err != nil {
						return err
					}
				}
			}
		}
		for _, children := range [][]workflows.Step{s.Sequence, s.Parallel, s.OnResult, s.OnComplete, s.OnIncomplete, s.OnError, s.OnFailure, s.Finally} {
			if err := c.steps(base, children); err != nil {
				return err
			}
		}
	}
	return nil
}
func (c *collector) workflow(ref string, depth int) error {
	canonical, err := workflows.CanonicalLocalRef(ref)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	return c.visit(canonical, depth)
}
func (c *collector) visit(ref string, depth int) error {
	if depth > 200 {
		return fmt.Errorf("%s: workflow depth exceeds 200", ref)
	}
	if c.active[ref] {
		return fmt.Errorf("%s: workflow dependency cycle", ref)
	}
	if c.loaded[ref] {
		return nil
	}
	if len(c.loaded) >= 201 {
		return fmt.Errorf("%s: closure exceeds 201 workflows", ref)
	}
	data, err := c.read(ref)
	if err != nil {
		return err
	}
	w, err := workflows.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if err = workflows.Validate(w); err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if depth > 0 && w.On.WorkflowCall == nil {
		return fmt.Errorf("%s: plugin workflow requires on.workflow_call", ref)
	}
	c.active[ref] = true
	c.loaded[ref] = true
	for _, j := range w.Jobs {
		if j.Uses != "" {
			if err = c.workflow(j.Uses, depth+1); err != nil {
				return err
			}
		}
		if j.Container != nil {
			for _, r := range j.Container.Files {
				if err = c.helper(path.Dir(ref), r); err != nil {
					return err
				}
			}
		}
		if err = c.steps(path.Dir(ref), j.Steps); err != nil {
			return fmt.Errorf("%s: %w", ref, err)
		}
	}
	c.active[ref] = false
	return nil
}

// rewriteWorkflow changes only reusable job references. Native relative paths,
// step definitions and input declarations retain their author-owned YAML values.
func rewriteWorkflow(data []byte, prefix string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != "jobs" {
			continue
		}
		jobs := root.Content[i+1]
		for j := 1; j < len(jobs.Content); j += 2 {
			job := jobs.Content[j]
			for k := 0; k < len(job.Content); k += 2 {
				if job.Content[k].Value == "uses" {
					ref, err := workflows.CanonicalLocalRef(job.Content[k+1].Value)
					if err != nil {
						return nil, err
					}
					job.Content[k+1].Value = path.Join(prefix, ref)
				}
			}
		}
	}
	return yaml.Marshal(&doc)
}
