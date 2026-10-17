package workflows

import (
	"io/fs"
	"path/filepath"
	"strings"
)

type Definition struct {
	Ref   string        `json:"ref"`
	Name  string        `json:"name,omitempty"`
	Call  *WorkflowCall `json:"workflow_call,omitempty"`
	Error string        `json:"error,omitempty"`
}

func (e *Executor) Discover() ([]Definition, error) {
	dir := e.DefinitionsDir
	if dir == "" {
		dir = filepath.Join(e.WorkspaceDir, "workflows")
	}
	out := []Definition{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		ref := "workflows/" + filepath.ToSlash(relative)
		entry := Definition{Ref: ref}
		all, err := e.Load(ref)
		if err != nil {
			entry.Error = err.Error()
		} else {
			entry.Name = all[ref].Name
			entry.Call = all[ref].On.WorkflowCall
		}
		out = append(out, entry)
		return nil
	})
	return out, err
}
