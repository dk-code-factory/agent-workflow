package workflows

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type Resolver struct {
	WorkspaceDir   string
	DefinitionsDir string
}

type ResolvedRef struct {
	Canonical string
	Path      string
}

const DefaultDefinitionsDir = "workflows"

func (r Resolver) ResolveLocal(ref string) (ResolvedRef, error) {
	canonical, err := CanonicalLocalRef(ref)
	if err != nil {
		return ResolvedRef{}, err
	}
	workspaceDir := strings.TrimSpace(r.WorkspaceDir)
	if workspaceDir == "" {
		workspaceDir = "."
	}
	definitionsDir, err := cleanDefinitionsDir(r.DefinitionsDir)
	if err != nil {
		return ResolvedRef{}, err
	}
	root := filepath.Join(workspaceDir, definitionsDir)
	rel := strings.TrimPrefix(canonical, DefaultDefinitionsDir+"/")
	target := filepath.Join(root, filepath.FromSlash(rel))
	if err := ensureInsideWorkflowRoot(workspaceDir, root, target); err != nil {
		return ResolvedRef{}, err
	}
	return ResolvedRef{Canonical: canonical, Path: target}, nil
}

func cleanDefinitionsDir(value string) (string, error) {
	value = strings.TrimSpace(filepath.ToSlash(value))
	if value == "" {
		value = DefaultDefinitionsDir
	}
	if path.IsAbs(value) || filepath.IsAbs(value) {
		return "", fmt.Errorf("workflow definitions dir %q must be relative", value)
	}
	clean := path.Clean(value)
	if clean == "." {
		return "", fmt.Errorf("workflow definitions dir %q is invalid", value)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("workflow definitions dir %q contains unsafe path component", value)
		}
	}
	return filepath.FromSlash(clean), nil
}

func CanonicalLocalRef(ref string) (string, error) {
	ref = strings.TrimSpace(filepath.ToSlash(ref))
	if ref == "" {
		return "", fmt.Errorf("workflow ref is required")
	}
	if path.IsAbs(ref) || filepath.IsAbs(ref) {
		return "", fmt.Errorf("workflow ref %q must be relative", ref)
	}
	if strings.HasPrefix(ref, "./") {
		ref = strings.TrimPrefix(ref, "./")
	}
	for _, part := range strings.Split(ref, "/") {
		switch part {
		case "", ".", "..":
			return "", fmt.Errorf("workflow ref %q contains unsafe path component", ref)
		}
	}
	clean := path.Clean(ref)
	if !strings.HasPrefix(clean, DefaultDefinitionsDir+"/") {
		return "", fmt.Errorf("workflow ref %q must start with workflows/", ref)
	}
	ext := strings.ToLower(path.Ext(clean))
	if ext != ".yml" && ext != ".yaml" {
		return "", fmt.Errorf("workflow ref %q must end with .yml or .yaml", ref)
	}
	return clean, nil
}

func ensureInsideWorkflowRoot(workspace, root, target string) error {
	workspaceAbs, err := filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("resolve workflow workspace: %w", err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve workflow root: %w", err)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("resolve workflow path: %w", err)
	}
	workspaceEval, err := evalWorkflowPathPrefix(workspaceAbs)
	if err != nil {
		return fmt.Errorf("resolve workflow workspace symlink: %w", err)
	}
	rootEval, err := evalWorkflowPathPrefix(rootAbs)
	if err != nil {
		return fmt.Errorf("resolve workflow root symlink: %w", err)
	}
	if !workflowPathStrictlyInside(workspaceEval, rootEval) {
		return fmt.Errorf("workflow definitions root escapes workspace")
	}
	targetEval, err := evalWorkflowPathPrefix(targetAbs)
	if err != nil {
		return fmt.Errorf("resolve workflow symlink: %w", err)
	}
	if !workflowPathStrictlyInside(rootEval, targetEval) {
		return fmt.Errorf("workflow path escapes workflow root")
	}
	return nil
}

func workflowPathStrictlyInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." &&
		rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

func evalWorkflowPathPrefix(absPath string) (string, error) {
	clean := filepath.Clean(absPath)
	probe := clean
	var suffix []string
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			eval, evalErr := filepath.EvalSymlinks(probe)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				eval = filepath.Join(eval, suffix[i])
			}
			return filepath.Clean(eval), nil
		} else if os.IsNotExist(statErr) {
			parent := filepath.Dir(probe)
			if parent == probe {
				return clean, nil
			}
			suffix = append(suffix, filepath.Base(probe))
			probe = parent
		} else {
			return "", statErr
		}
	}
}

func IsLocalWorkflowRef(ref string) bool {
	_, err := CanonicalLocalRef(ref)
	return err == nil
}
