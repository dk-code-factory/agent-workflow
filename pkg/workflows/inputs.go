package workflows

import (
	"fmt"
	"io"
)

func readDefinition(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, (4<<20)+1))
	if err == nil && len(b) > 4<<20 {
		err = fmt.Errorf("workflow exceeds 4 MiB")
	}
	return b, err
}

func resolveInputs(call *WorkflowCall, supplied map[string]any, secrets map[string]string) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range supplied {
		out[k] = v
	}
	if call == nil {
		return out, nil
	}
	for k := range supplied {
		if _, ok := call.Inputs[k]; !ok {
			return nil, fmt.Errorf("unknown input %q", k)
		}
	}
	for k, spec := range call.Inputs {
		value, ok := out[k]
		if !ok && spec.Default != nil {
			value = cloneJSONValue(spec.Default)
			out[k] = value
			ok = true
		}
		if !ok {
			if spec.Required {
				return nil, fmt.Errorf("required input %q is missing", k)
			}
			continue
		}
		valid := true
		switch spec.Type {
		case "string", "":
			_, valid = value.(string)
		case "boolean":
			_, valid = value.(bool)
		case "number":
			_, valid = asNumericFloat(value)
		case "object":
			_, valid = value.(map[string]any)
		case "array":
			_, valid = value.([]any)
		}
		if !valid {
			return nil, fmt.Errorf("input %q must have type %s", k, spec.Type)
		}
	}
	for k, spec := range call.Secrets {
		if spec.Required && secrets[k] == "" {
			return nil, fmt.Errorf("required secret %q is missing", k)
		}
	}
	return out, nil
}
