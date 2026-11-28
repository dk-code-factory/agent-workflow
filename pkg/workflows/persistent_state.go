package workflows

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// PersistentStateDirectory declares only a symbolic resource and container path.
type PersistentStateDirectory struct {
	MountTarget string `json:"mount-target" yaml:"mount-target"`
}

// PersistentStateBinding binds each resource to the admitted package identity.
// Neither versions nor run identifiers select the durable namespace.
type PersistentStateBinding struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	Target  string `json:"target"`
}

var persistentStateName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

func persistentStateDeclarations(declarations map[string]PersistentStateDirectory) ([]PersistentStateBinding, error) {
	var bindings []PersistentStateBinding
	for name, declaration := range declarations {
		if !persistentStateName.MatchString(name) {
			return nil, fmt.Errorf("invalid persistent-state name %q", name)
		}
		if err := runtimeexec.ValidatePersistentStateTarget(declaration.MountTarget); err != nil {
			return nil, err
		}
		for _, previous := range bindings {
			if previous.Target == declaration.MountTarget || strings.HasPrefix(previous.Target, declaration.MountTarget+"/") || strings.HasPrefix(declaration.MountTarget, previous.Target+"/") {
				return nil, fmt.Errorf("persistent-state mount-targets must be disjoint")
			}
		}
		bindings = append(bindings, PersistentStateBinding{Name: name, Target: declaration.MountTarget})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })
	return bindings, nil
}

func (e *Executor) bindPersistentState(declarations map[string]PersistentStateDirectory) ([]PersistentStateBinding, error) {
	bindings, err := persistentStateDeclarations(declarations)
	if err != nil {
		return nil, err
	}
	if len(bindings) > 0 && (e.packageBinding == nil || !persistentStateName.MatchString(e.packageBinding.Name)) {
		return nil, fmt.Errorf("persistent-state requires an admitted package")
	}
	for i := range bindings {
		bindings[i].Package = e.packageBinding.Name
	}
	return bindings, nil
}

func (e *Executor) persistentStateMounts(bindings []PersistentStateBinding, engineState string) ([]runtimeexec.PersistentStateMount, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	workspace, err := filepath.Abs(e.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, err
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return nil, fmt.Errorf("XDG_STATE_HOME must be absolute for persistent state")
	}
	hash := sha256.Sum256([]byte(workspace))
	root := filepath.Join(stateHome, "agent-workflow", "plugin-state", "repositories", hex.EncodeToString(hash[:]))
	protected := []string{workspace, engineState, filepath.Join(workspace, ".agent-workflow")}
	mounts := make([]runtimeexec.PersistentStateMount, 0, len(bindings))
	for _, binding := range bindings {
		if e.packageBinding == nil || binding.Package != e.packageBinding.Name || !persistentStateName.MatchString(binding.Package) || !persistentStateName.MatchString(binding.Name) {
			return nil, fmt.Errorf("persistent-state package binding missing or changed")
		}
		if err := runtimeexec.ValidatePersistentStateTarget(binding.Target); err != nil {
			return nil, err
		}
		source := filepath.Join(root, binding.Package, binding.Name)
		if err := runtimeexec.ValidatePersistentStateLocation(source, true, protected...); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(source, 0700); err != nil {
			return nil, err
		}
		mounts = append(mounts, runtimeexec.PersistentStateMount{Source: source, Target: binding.Target})
	}
	if err := runtimeexec.ValidatePersistentStateMounts(mounts, protected...); err != nil {
		return nil, err
	}
	return mounts, nil
}
