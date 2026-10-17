package clirunner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Native constrained decoding is used only for strict object schemas; other
// existing contracts retain prompt guidance plus engine validation.
func nativeSchema(contract *workflows.AgentOutputContract) bool {
	if contract == nil || contract.Schema["type"] != "object" {
		return false
	}
	return strictObjects(contract.Schema)
}
func strictObjects(schema map[string]any) bool {
	if schema["type"] == "object" {
		if schema["additionalProperties"] != false {
			return false
		}
		properties, _ := schema["properties"].(map[string]any)
		required := map[string]bool{}
		switch list := schema["required"].(type) {
		case []any:
			for _, v := range list {
				s, ok := v.(string)
				if !ok {
					return false
				}
				required[s] = true
			}
		case []string:
			for _, s := range list {
				required[s] = true
			}
		}
		for name, value := range properties {
			if !required[name] {
				return false
			}
			if nested, ok := value.(map[string]any); ok && !strictObjects(nested) {
				return false
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok && !strictObjects(items) {
		return false
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if choices, ok := schema[key].([]any); ok {
			for _, value := range choices {
				if child, ok := value.(map[string]any); ok && !strictObjects(child) {
					return false
				}
			}
		}
	}
	return true
}
func (r *Runner) schemaFile(schema map[string]any) (string, func(), error) {
	dir := filepath.Join(r.Workspace, ".agent-workflow", "schemas")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	file, err := os.CreateTemp(dir, "schema-*.json")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if err = json.NewEncoder(file).Encode(schema); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, err
	}
	if err = file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return file.Name(), cleanup, nil
}
func schemaJSON(schema map[string]any) (string, error) {
	b, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("encode native output schema: %w", err)
	}
	return string(b), nil
}
