package workflows

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// AgentConversion reports every native field excluded from the portable source.
// It contains field names and policy guidance, never native credential values.
type AgentConversion struct {
	Output               string   `json:"output,omitempty"`
	IncompatibleOptions  []string `json:"incompatible-options"`
	RuntimePolicyChanges []string `json:"runtime-policy-changes"`
}

type AgentConversionError struct{ Report AgentConversion }

func (e *AgentConversionError) Error() string {
	return "conversion would discard native options; review required runtime-policy changes and acknowledge each omitted field with --acknowledge-loss FIELD"
}

// ConvertAgent reads native source through the same protected bounded boundary
// as agent admission. It never invokes a host, installs dependencies or executes
// hooks, and creates output exclusively only after all losses are acknowledged.
func ConvertAgent(from, input, output string, acknowledged []string) (AgentConversion, error) {
	report := AgentConversion{IncompatibleOptions: []string{}, RuntimePolicyChanges: []string{}}
	if from != "codex" && from != "claude" {
		return report, fmt.Errorf("--from must be codex or claude")
	}
	info, err := os.Lstat(input)
	if err != nil {
		return report, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return report, fmt.Errorf("conversion input must be a regular file of at most 1 MiB")
	}
	data, err := readAgentDefinition(input, info)
	if err != nil {
		return report, err
	}
	var source map[string]any
	var body string
	if from == "codex" {
		if err = toml.Unmarshal(data, &source); err != nil {
			return report, fmt.Errorf("%s: invalid Codex TOML: %w", input, err)
		}
	} else {
		source, body, err = readClaudeAgent(data)
		if err != nil {
			return report, fmt.Errorf("%s: %w", input, err)
		}
	}
	normalized := map[string]string{}
	compatible := map[string]string{"name": "name", "description": "description", "model": "model"}
	if from == "codex" {
		compatible["developer_instructions"] = "instructions"
		compatible["model_reasoning_effort"] = "reasoning-effort"
	} else {
		normalized["instructions"] = body
		compatible["effort"] = "reasoning-effort"
	}
	for key, value := range source {
		if target, ok := compatible[key]; ok {
			text, ok := value.(string)
			if !ok {
				return report, fmt.Errorf("%s: %s must be a string", input, key)
			}
			if from == "claude" && key == "model" && text == "inherit" {
				continue
			}
			normalized[target] = text
			continue
		}
		report.IncompatibleOptions = append(report.IncompatibleOptions, key)
		if nativePolicyField(key) {
			report.RuntimePolicyChanges = append(report.RuntimePolicyChanges, key+": review and configure the corresponding target workspace agents.yml runtime policy; it is not part of the agent definition")
		}
	}
	sort.Strings(report.IncompatibleOptions)
	sort.Strings(report.RuntimePolicyChanges)
	allowed := map[string]bool{}
	for _, field := range acknowledged {
		allowed[field] = true
	}
	for _, field := range report.IncompatibleOptions {
		if !allowed[field] {
			return report, &AgentConversionError{Report: report}
		}
	}
	for field := range allowed {
		found := false
		for _, omitted := range report.IncompatibleOptions {
			if field == omitted {
				found = true
			}
		}
		if !found {
			return report, fmt.Errorf("acknowledged field %q is not an omitted native option", field)
		}
	}
	encoded, err := yaml.Marshal(normalized)
	if err != nil {
		return report, err
	}
	if _, err = ParseAgentDefinition(encoded); err != nil {
		return report, fmt.Errorf("%s: converted agent: %w", input, err)
	}
	if !isYAMLDefinition(output) {
		return report, fmt.Errorf("conversion output must end with .yaml or .yml")
	}
	file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return report, err
	}
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if writeErr != nil {
		return report, writeErr
	}
	if closeErr != nil {
		return report, closeErr
	}
	report.Output = output
	return report, nil
}
func nativePolicyField(key string) bool {
	lower := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	for _, fragment := range []string{"auth", "approval", "credential", "tool", "sandbox", "permission", "mcp", "hook", "execution", "limit", "turn", "budget", "network", "isolation", "memory", "background", "skill"} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}
func readClaudeAgent(data []byte) (map[string]any, string, error) {
	// Retain the original Markdown body bytes, including newline conventions.
	lines := bytes.SplitAfter(data, []byte("\n"))
	if len(lines) == 0 || strings.TrimRight(string(lines[0]), "\r\n") != "---" {
		return nil, "", fmt.Errorf("Claude agent requires YAML frontmatter")
	}
	offset := len(lines[0])
	end := -1
	bodyStart := 0
	for _, line := range lines[1:] {
		if strings.TrimRight(string(line), "\r\n") == "---" {
			end = offset
			bodyStart = offset + len(line)
			break
		}
		offset += len(line)
	}
	if end < 0 {
		return nil, "", fmt.Errorf("Claude agent frontmatter is not closed")
	}
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data[len(lines[0]):end]))
	if err := decoder.Decode(&node); err != nil {
		return nil, "", err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, "", fmt.Errorf("Claude agent frontmatter must be a mapping")
	}
	if err := checkNode(node.Content[0], reflect.TypeOf(map[string]any{}), 0); err != nil {
		return nil, "", err
	}
	var source map[string]any
	if err := node.Decode(&source); err != nil {
		return nil, "", err
	}
	return source, string(data[bodyStart:]), nil
}
