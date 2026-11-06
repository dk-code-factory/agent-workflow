package workflows

import (
	"fmt"
	"sort"
	"strings"
)

// Names belong to one definition, including for reusable workflows. Expansion
// leaves the authoring model untouched and retains all contract repair options.
func resolveNamedOutput(w *Workflow, raw any) (any, error) {
	output, ok := raw.(map[string]any)
	if !ok {
		return raw, nil
	}
	name, ok := output["schema"].(string)
	if !ok {
		return raw, nil
	}
	schema, exists := w.Schemas[name]
	if !exists {
		return nil, fmt.Errorf("schema: unknown workflow-local schema %q", name)
	}
	resolved := make(map[string]any, len(output))
	for key, value := range output {
		resolved[key] = value
	}
	// Normalize a private copy so concurrent consumers cannot share mutable maps.
	copy, err := normalizeSchemaMap(schema)
	if err != nil {
		return nil, fmt.Errorf("schema %q: %w", name, err)
	}
	resolved["schema"] = copy
	return resolved, nil
}

func validateNamedSchemas(w *Workflow) error {
	names := make([]string, 0, len(w.Schemas))
	for name := range w.Schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := "schemas." + name
		if !idPattern.MatchString(name) {
			return fmt.Errorf("%s: invalid schema name", path)
		}
		if w.Schemas[name] == nil {
			return fmt.Errorf("%s: expected a schema mapping", path)
		}
		schema, err := normalizeSchemaMap(w.Schemas[name])
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if err = validateNamedSchema(schema, path, 0); err != nil {
			return err
		}
		if _, err = compileJSONSchemaPatterns(schema); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// Reject constraints the shared output validator cannot enforce. Legacy inline
// schemas retain their existing admission behavior.
func validateNamedSchema(schema map[string]any, path string, depth int) error {
	if depth > maxAgentOutputSchemaDepth {
		return fmt.Errorf("%s: maximum schema depth exceeded", path)
	}
	for key, value := range schema {
		field := path + "." + key
		invalid := func() error { return fmt.Errorf("%s: invalid schema keyword value", field) }
		expected := ""
		switch key {
		case "properties", "required", "additionalProperties":
			expected = "object"
		case "items", "maxItems":
			expected = "array"
		case "minLength", "maxLength":
			expected = "string"
		}
		if expected != "" && schema["type"] != expected {
			return fmt.Errorf("%s: requires type: %s", field, expected)
		}
		switch key {
		case "type":
			text, ok := value.(string)
			if !ok || (text != "object" && text != "array" && text != "string" && text != "integer" && text != "number" && text != "boolean" && text != "null") {
				return invalid()
			}
		case "title", "description", "$comment", "pattern":
			if _, ok := value.(string); !ok {
				return invalid()
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				return invalid()
			}
			for name, raw := range props {
				child, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.%s: expected a schema mapping", field, name)
				}
				if err := validateNamedSchema(child, field+"."+name, depth+1); err != nil {
					return err
				}
			}
		case "items", "additionalProperties":
			if _, ok := value.(bool); ok && key == "additionalProperties" {
				continue
			}
			child, ok := value.(map[string]any)
			if !ok {
				return invalid()
			}
			if err := validateNamedSchema(child, field, depth+1); err != nil {
				return err
			}
		case "required", "enum":
			items, ok := value.([]any)
			if !ok || (key == "enum" && len(items) == 0) {
				return invalid()
			}
			if key == "required" {
				seen := map[string]bool{}
				for _, item := range items {
					text, ok := item.(string)
					if !ok || text == "" || text != strings.TrimSpace(text) || seen[text] {
						return invalid()
					}
					seen[text] = true
				}
			}
		case "minLength", "maxLength", "maxItems":
			// Zero upper bounds are not enforced by the historical validator.
			number, ok := asFloat(value)
			if !ok || number < 0 || (number == 0 && key != "minLength") || float64(schemaIntegerKeyword(schema, key)) != number {
				return invalid()
			}
		case "minimum":
			if schema["type"] != "number" && schema["type"] != "integer" {
				return fmt.Errorf("%s: requires type: number or integer", field)
			}
			if _, ok := asFloat(value); !ok {
				return invalid()
			}
		default:
			return fmt.Errorf("%s: unsupported schema keyword (references and composition are unsupported)", field)
		}
	}
	return nil
}
