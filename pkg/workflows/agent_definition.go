package workflows

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDefinition is immutable admitted data, not a provider/security policy.
// Native custom agent files cannot replace authentication, tools or execution.
type AgentDefinition struct {
	Name                  string `yaml:"name" json:"name"`
	Description           string `yaml:"description" json:"description"`
	DeveloperInstructions string `yaml:"instructions" json:"developer_instructions"`
	Model                 string `yaml:"model,omitempty" json:"model,omitempty"`
	ReasoningEffort       string `yaml:"reasoning-effort,omitempty" json:"model_reasoning_effort,omitempty"`
	Source                string `yaml:"-" json:"source"`
	Bytes                 string `yaml:"-" json:"bytes"`
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
			if !entry.IsDir() && isYAMLDefinition(entry.Name()) {
				candidates = append(candidates, filepath.Join(path, entry.Name()))
			}
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("agent directory %s must contain exactly one YAML definition (found %d); convert native files with agent convert", path, len(candidates))
		}
		path = candidates[0]
		info, err = os.Stat(path)
		if err != nil {
			return nil, err
		}
	}
	if !isYAMLDefinition(path) {
		return nil, fmt.Errorf("agent definition %s: new admissions require YAML; use agent-workflow agent convert --from codex|claude --input FILE --output FILE.yaml", path)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("agent definition requires a regular YAML file of at most 1 MiB")
	}
	data, err := readAgentDefinition(path, info)
	if err != nil {
		return nil, err
	}
	definition, err := ParseAgentDefinition(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	definition.Source, definition.Bytes = path, string(data)
	return definition, nil
}

func isYAMLDefinition(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

// ParseAgentDefinition admits the shared source format. Snapshot JSON deliberately
// retains the historical field names and raw bytes; resume never reparses sources.
func ParseAgentDefinition(data []byte) (*AgentDefinition, error) {
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("agent definition exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("agent definition requires exactly one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("agent definition must be a YAML mapping")
	}
	if err := checkNode(doc.Content[0], reflect.TypeOf(AgentDefinition{}), 0); err != nil {
		return nil, fmt.Errorf("unsupported or invalid YAML agent definition: %w", err)
	}
	for i := 1; i < len(doc.Content[0].Content); i += 2 {
		value := doc.Content[0].Content[i]
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return nil, fmt.Errorf("agent definition fields must be strings")
		}
	}
	var definition AgentDefinition
	if err := doc.Decode(&definition); err != nil {
		return nil, err
	}
	if strings.TrimSpace(definition.Name) == "" || strings.TrimSpace(definition.Description) == "" || strings.TrimSpace(definition.DeveloperInstructions) == "" {
		return nil, fmt.Errorf("YAML agent definition requires name, description and instructions")
	}
	switch definition.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return nil, fmt.Errorf("unsupported reasoning-effort %q", definition.ReasoningEffort)
	}
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
			// This loader runs only for new admissions. Recovery consumes saved
			// definitions directly, preserving historical accepted contracts.
			if validator, ok := e.Agents.(interface {
				AdmitNativeOutput(*AgentOutputContract) error
			}); ok {
				output, err := resolveNamedOutput(w, step.With["output"])
				if err != nil {
					return step, err
				}
				contract, err := ParseAgentOutputContract(output)
				if err != nil {
					return step, err
				}
				if err := validator.AdmitNativeOutput(contract); err != nil {
					return step, err
				}
			}
			load := func(reference string) (*AgentDefinition, error) {
				if !filepath.IsAbs(reference) {
					reference = filepath.Join(filepath.Dir(path), reference)
				}
				if e.DefinitionRoot != "" {
					if !workflowPathStrictlyInside(e.DefinitionRoot, reference) {
						return nil, fmt.Errorf("package agent definition escapes source root")
					}
				}
				var definition *AgentDefinition
				var err error
				if e.packageFiles != nil {
					definition, err = e.capturedAgent(reference)
				} else {
					definition, err = LoadAgentDefinition(reference)
				}
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
