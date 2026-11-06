// Package workflowdef validates and evaluates versioned JSON workflow definitions.
// Validation never reads sources, starts providers, or executes commands.
package workflowdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	assets "github.com/dk-code-factory/agent-workflow"
	"github.com/google/jsonschema-go/jsonschema"
)

const Version = "0.2"

const MaxDocumentBytes = 4 << 20

type Definition struct {
	Data    map[string]any
	schemas map[string]*jsonschema.Resolved
}

var documentContract = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(assets.WorkflowDefinitionSchema, &schema); err != nil {
		return nil, err
	}
	return schema.Resolve(nil)
})

// DecodeJSON rejects duplicate keys, trailing documents, non-JSON syntax, and
// excessive nesting. JSON numbers use float64, as required by JSONPath's model.
func DecodeJSON(data []byte) (any, error) {
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("JSON exceeds 4 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	value, err := decodeValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON document")
	}
	return value, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("JSON nesting exceeds 64")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("object key must be a string")
			}
			if _, exists := m[name]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", name)
			}
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[name] = value
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return m, nil
	case json.Delim('['):
		values := []any{}
		for d.More() {
			value, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return values, nil
	default:
		if _, ok := token.(json.Delim); ok {
			return nil, fmt.Errorf("unexpected JSON delimiter")
		}
		return token, nil
	}
}

// Parse validates syntax, the embedded document schema, and semantic references.
func Parse(data []byte) (*Definition, error) {
	value, err := DecodeJSON(data)
	if err != nil {
		return nil, err
	}
	contract, err := documentContract()
	if err != nil {
		return nil, fmt.Errorf("workflow document schema: %w", err)
	}
	if err := contract.Validate(value); err != nil {
		return nil, fmt.Errorf("workflow definition: %w", err)
	}
	d := &Definition{Data: value.(map[string]any), schemas: map[string]*jsonschema.Resolved{}}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Definition) schema(use any) (*jsonschema.Resolved, error) {
	if name, ok := use.(string); ok {
		if schema := d.schemas[name]; schema != nil {
			return schema, nil
		}
		return nil, fmt.Errorf("unknown schema %q", name)
	}
	return resolveSchema(use)
}

// ValidateValue validates a schema use without inserting defaults or coercing data.
func (d *Definition) ValidateValue(use, value any) error {
	schema, err := d.schema(use)
	if err != nil {
		return err
	}
	return schema.Validate(value)
}

func resolveSchema(value any) (*jsonschema.Resolved, error) {
	if err := checkSchema(value, true); err != nil {
		return nil, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("invalid JSON Schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON Schema: %w", err)
	}
	return resolved, nil
}

// Only traverse schema locations: "$ref" inside an example is ordinary data.
func checkSchema(value any, root bool) error {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if dialect, exists := m["$schema"]; exists && dialect != "https://json-schema.org/draft/2020-12/schema" {
		return fmt.Errorf("only JSON Schema 2020-12 is supported")
	}
	if _, exists := m["$id"]; exists && !root {
		return fmt.Errorf("nested schema $id is unsupported")
	}
	for _, key := range []string{"$ref", "$dynamicRef"} {
		if value, exists := m[key]; exists {
			text, ok := value.(string)
			if !ok || !strings.HasPrefix(text, "#") {
				return fmt.Errorf("schema %s must be a local fragment", key)
			}
		}
	}
	for _, key := range []string{"$defs", "definitions", "properties", "patternProperties", "dependentSchemas"} {
		if children, ok := m[key].(map[string]any); ok {
			for _, child := range children {
				if err := checkSchema(child, false); err != nil {
					return err
				}
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema"} {
		if child, exists := m[key]; exists {
			if err := checkSchema(child, false); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if children, ok := m[key].([]any); ok {
			for _, child := range children {
				if err := checkSchema(child, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func object(value any) map[string]any { m, _ := value.(map[string]any); return m }
func list(value any) []any            { l, _ := value.([]any); return l }
func text(value any) string           { s, _ := value.(string); return s }

func clone(value any) any {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Float64()
		if err == nil {
			return n
		}
		return v
	case map[string]any:
		m := make(map[string]any, len(v))
		for key, item := range v {
			m[key] = clone(item)
		}
		return m
	case []any:
		l := make([]any, len(v))
		for i, item := range v {
			l[i] = clone(item)
		}
		return l
	default:
		return value
	}
}
