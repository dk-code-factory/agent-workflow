package workflows

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	data, err := readAgentDefinition(path, info)
	if err != nil {
		return nil, err
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

func readAgentDefinition(path string, admitted os.FileInfo) ([]byte, error) {
	file, err := openAgentDefinitionFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > 1<<20 || !os.SameFile(admitted, opened) || opened.Size() != admitted.Size() || !opened.ModTime().Equal(admitted.ModTime()) {
		return nil, fmt.Errorf("agent definition changed before reading")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("agent definition exceeds 1 MiB")
	}
	return data, nil
}

type NativeTaskRunner interface {
	AdmitNativeTask(*AgentDefinition) (*AgentDefinition, error)
}

// agentDefinitionChoices keeps definition paths literal while allowing the
// selected name to come from workflow data. All choices are admitted together.
func agentDefinitionChoices(value any) (map[string]string, error) {
	if reference, ok := value.(string); ok && strings.TrimSpace(reference) != "" {
		return nil, nil
	}
	selection, ok := value.(map[string]any)
	if !ok || len(selection) != 2 {
		return nil, fmt.Errorf("agent/run requires with.agent definition, reference or {select, definitions}")
	}
	selector, ok := selection["select"].(string)
	if !ok || strings.TrimSpace(selector) == "" {
		return nil, fmt.Errorf("agent selection requires a nonempty string select")
	}
	values, ok := selection["definitions"].(map[string]any)
	if !ok || len(values) == 0 || len(values) > 32 {
		return nil, fmt.Errorf("agent selection requires 1–32 literal definitions")
	}
	choices := make(map[string]string, len(values))
	for name, value := range values {
		path, ok := value.(string)
		if strings.TrimSpace(name) == "" || len(name) > 128 || strings.Contains(name, "${{") || !ok || strings.TrimSpace(path) == "" || strings.Contains(path, "${{") {
			return nil, fmt.Errorf("agent selection requires literal names and definition paths")
		}
		choices[name] = path
	}
	if !wholeExpression(selector) {
		if _, ok := choices[selector]; !ok {
			return nil, fmt.Errorf("unknown agent definition selection %q", selector)
		}
	}
	return choices, nil
}

func (e *Executor) admitAgentDefinitions(w *Workflow, path string) error {
	for id, job := range w.Jobs {
		steps, err := mapSteps(job.Steps, "", func(step Step) (Step, error) {
			if step.Uses != "agent/run" {
				return step, nil
			}
			load := func(reference string) (*AgentDefinition, error) {
				if !filepath.IsAbs(reference) {
					reference = filepath.Join(filepath.Dir(path), reference)
				}
				definition, err := LoadAgentDefinition(reference)
				if err == nil {
					if runner, ok := e.Agents.(NativeTaskRunner); ok {
						definition, err = runner.AdmitNativeTask(definition)
					}
				}
				return definition, err
			}
			choices, err := agentDefinitionChoices(step.With["agent"])
			if err != nil {
				return step, err
			}
			if choices == nil {
				reference := step.With["agent"].(string)
				// String expressions retain their existing session-only meaning.
				if !wholeExpression(reference) {
					step.Definition, err = load(reference)
				}
				return step, err
			}
			step.Definitions = make(map[string]*AgentDefinition, len(choices))
			names := make([]string, 0, len(choices))
			for name := range choices {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				definition, err := load(choices[name])
				if err != nil {
					return step, fmt.Errorf("agent definition %s: %w", name, err)
				}
				step.Definitions[name] = definition
			}
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
