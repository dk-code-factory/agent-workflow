package workflows

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// AgentDefinition is immutable admitted data, not a provider/security policy.
// Native custom agent files cannot replace authentication, tools or execution.
type AgentDefinition struct {
	Name                  string `toml:"name" json:"name"`
	Description           string `toml:"description" json:"description"`
	DeveloperInstructions string `toml:"developer_instructions" json:"developer_instructions"`
	Model                 string `toml:"model" json:"model,omitempty"`
	ReasoningEffort       string `toml:"model_reasoning_effort" json:"model_reasoning_effort,omitempty"`
	Source                string `toml:"-" json:"source"`
	Bytes                 string `toml:"-" json:"bytes"`
}

func LoadAgentDefinition(path string) (*AgentDefinition, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		var candidates []string
		for _, entry := range entries {
			if !entry.IsDir() && strings.EqualFold(filepath.Ext(entry.Name()), ".toml") {
				candidates = append(candidates, filepath.Join(path, entry.Name()))
			}
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("agent directory %s must contain exactly one TOML file (found %d)", path, len(candidates))
		}
		path = candidates[0]
		info, err = os.Stat(path)
		if err != nil {
			return nil, err
		}
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || !strings.EqualFold(filepath.Ext(path), ".toml") {
		return nil, fmt.Errorf("agent definition requires a regular TOML file of at most 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("agent definition exceeds 1 MiB")
	}
	var definition AgentDefinition
	decoder := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields()
	if err = decoder.Decode(&definition); err != nil {
		return nil, fmt.Errorf("unsupported or invalid Codex agent definition %s: %w", path, err)
	}
	if strings.TrimSpace(definition.Name) == "" || strings.TrimSpace(definition.Description) == "" || strings.TrimSpace(definition.DeveloperInstructions) == "" {
		return nil, fmt.Errorf("Codex agent definition requires name, description and developer_instructions")
	}
	switch definition.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return nil, fmt.Errorf("unsupported model_reasoning_effort %q", definition.ReasoningEffort)
	}
	definition.Source, definition.Bytes = path, string(data)
	return &definition, nil
}

type NativeTaskRunner interface {
	AdmitNativeTask(*AgentDefinition) (*AgentDefinition, error)
}

func (e *Executor) admitAgentDefinitions(w *Workflow, path string) error {
	for id, job := range w.Jobs {
		steps, err := mapSteps(job.Steps, "", func(step Step) (Step, error) {
			if step.Uses != "agent/run" {
				return step, nil
			}
			reference, _ := step.With["agent"].(string)
			// Expressions can only carry an admitted opaque reference. Loading a new
			// definition dynamically would bypass the immutable admission snapshot.
			if wholeExpression(reference) {
				return step, nil
			}
			if !filepath.IsAbs(reference) {
				reference = filepath.Join(filepath.Dir(path), reference)
			}
			definition, err := LoadAgentDefinition(reference)
			if err != nil {
				return step, err
			}
			if runner, ok := e.Agents.(NativeTaskRunner); ok {
				definition, err = runner.AdmitNativeTask(definition)
				if err != nil {
					return step, err
				}
			}
			step.Definition = definition
			return step, nil
		})
		if err != nil {
			return err
		}
		job.Steps = steps
		w.Jobs[id] = job
	}
	return nil
}
