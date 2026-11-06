package workflowdef

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/theory/jsonpath"
)

// Select implements exact cardinality, preserving a matched null or array.
func Select(query string, context map[string]any, many bool) (any, error) {
	if len(query) > 4096 {
		return nil, fmt.Errorf("JSONPath exceeds 4096 bytes")
	}
	p, err := jsonpath.Parse(query)
	if err != nil {
		return nil, err
	}
	values := p.Select(context)
	if many {
		out := make([]any, len(values))
		copy(out, values)
		return clone(out), nil
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("binding.cardinality: %s selected %d values, expected one", query, len(values))
	}
	return clone(values[0]), nil
}

// Resolve evaluates only explicit data-binding positions, never arbitrary strings
// inside schemas, source literals, or inserted prompt values.
func Resolve(value any, context map[string]any) (any, error) {
	switch value := value.(type) {
	case string:
		if strings.HasPrefix(value, "$") {
			return Select(value, context, false)
		}
		return value, nil
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			resolved, err := Resolve(item, context)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	case map[string]any:
		if len(value) == 1 {
			if literal, ok := value["literal"]; ok {
				return clone(literal), nil
			}
			if query, ok := value["all"]; ok {
				if _, ok := query.(string); !ok {
					return nil, fmt.Errorf("all requires a JSONPath string")
				}
				return Select(text(query), context, true)
			}
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			resolved, err := Resolve(item, context)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	default:
		return value, nil
	}
}

func Prompt(value string, context map[string]any) (string, error) {
	return interpolate(value, func(query string) (string, error) {
		value, err := Select(query, context, false)
		if err != nil {
			return "", err
		}
		if value, ok := value.(string); ok {
			return value, nil
		}
		data, err := json.Marshal(value)
		return string(data), err
	})
}

func (d *Definition) Condition(value any, context map[string]any) (bool, error) {
	switch value := value.(type) {
	case bool:
		return value, nil
	case string:
		selected, err := Select(value, context, false)
		if err != nil {
			return false, err
		}
		boolean, ok := selected.(bool)
		if !ok {
			return false, fmt.Errorf("condition requires a boolean, got %T", selected)
		}
		return boolean, nil
	case map[string]any:
		if query, ok := value["path"]; ok {
			selected, err := Select(text(query), context, false)
			if err != nil {
				return false, err
			}
			if expected, ok := value["equals"]; ok {
				return reflect.DeepEqual(selected, expected), nil
			}
			schema, err := d.schema(value["matches"])
			if err != nil {
				return false, err
			}
			return schema.Validate(selected) == nil, nil
		}
		if child, ok := value["not"]; ok {
			result, err := d.Condition(child, context)
			return !result, err
		}
		for _, key := range []string{"all", "any"} {
			if children, ok := value[key]; ok {
				for _, child := range list(children) {
					result, err := d.Condition(child, context)
					if err != nil {
						return false, err
					}
					if (key == "any" && result) || (key == "all" && !result) {
						return result, nil
					}
				}
				return key == "all", nil
			}
		}
	}
	return false, fmt.Errorf("invalid condition")
}

// Sources reads admission inputs once. Secret names remain in the definition;
// they are never exposed in ordinary data or materialized in this snapshot.
func (d *Definition) Sources(directory string, lookupEnv func(string) (string, bool)) (map[string]any, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	values := map[string]any{}
	for name, raw := range object(d.Data["sources"]) {
		source := object(raw)
		var value any
		switch {
		case source["file"] != nil:
			path := text(source["file"])
			if !filepath.IsAbs(path) {
				path = filepath.Join(directory, path)
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
			data, err := io.ReadAll(io.LimitReader(f, MaxDocumentBytes+1))
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, err
			}
			value, err = DecodeJSON(data)
			if err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
		case source["env"] != nil:
			text, present := lookupEnv(text(source["env"]))
			if !present {
				var found bool
				value, found = source["default"]
				if !found {
					return nil, fmt.Errorf("source %s: environment variable is absent", name)
				}
				value = clone(value)
			} else if source["parse"] == "json" {
				var err error
				value, err = DecodeJSON([]byte(text))
				if err != nil {
					return nil, fmt.Errorf("source %s: %w", name, err)
				}
			} else {
				value = text
			}
		case source["secret"] != nil:
			continue
		default:
			value = clone(source["value"])
		}
		if schema, ok := source["schema"]; ok {
			if err := d.ValidateValue(schema, value); err != nil {
				return nil, fmt.Errorf("source %s: %w", name, err)
			}
		}
		values[name] = value
	}
	return values, nil
}

func contextCopy(context map[string]any) map[string]any {
	out := maps.Clone(context)
	out["steps"] = maps.Clone(object(context["steps"]))
	return out
}

func (d *Definition) MarshalJSON() ([]byte, error) { return json.Marshal(d.Data) }
func (d *Definition) UnmarshalJSON(data []byte) error {
	parsed, err := Parse(data)
	if err != nil {
		return err
	}
	*d = *parsed
	return nil
}
