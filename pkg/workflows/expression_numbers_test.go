package workflows

import (
	"encoding/json"
	"testing"
)

func TestInputNumberComparisonsPreserveExtremeExponents(t *testing.T) {
	for _, test := range []struct {
		left, expression string
		want             bool
	}{
		{"1e1000001", "inputs.value == 10e1000000", true},
		{"1e1000001", "inputs.value > 1e1000000", true},
		{"1e-100000000000000000000", "inputs.value > 0", true},
		{"-1e-100000000000000000000", "inputs.value < 0", true},
		{"0e100000000000000000000", "inputs.value == 0", true},
		{"1e1000001", "inputs.value < 0", false},
		{"9007199254740993", "inputs.value == 9007199254740992", false},
		{"9007199254740993", "inputs.value > 9007199254740992", true},
	} {
		t.Run(test.left+"/"+test.expression, func(t *testing.T) {
			ctx := expressionContext{Inputs: map[string]any{"value": json.Number(test.left)}}
			if got, err := evalIf(test.expression, ctx); err != nil || got != test.want {
				t.Fatalf("condition %q for %s: got %v/%v, want %v", test.expression, test.left, got, err, test.want)
			}
		})
	}
}

func TestDeclaredNumberInputRejectsOtherJSONTypes(t *testing.T) {
	call := &WorkflowCall{Inputs: map[string]Input{"value": {Type: "number"}}}
	for _, value := range []any{"1e400", true, nil, []any{1}, map[string]any{"n": 1}, json.Number("invalid")} {
		if _, err := resolveInputs(call, map[string]any{"value": value}, nil); err == nil {
			t.Fatalf("non-number input admitted: %#v", value)
		}
	}
}
