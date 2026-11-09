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

func TestNumericInputsPreserveOutputRepairBudget(t *testing.T) {
	for _, key := range []string{"repair_attempts", "repairAttempts", "repair-attempts"} {
		for _, raw := range []string{"2", "2.0", "2e0", "0.2e1"} {
			t.Run(key+"/"+raw, func(t *testing.T) {
				args, err := renderMap(map[string]any{
					"format": "json",
					key:      "${{ inputs.attempts }}",
				}, expressionContext{Inputs: map[string]any{"attempts": json.Number(raw)}})
				if err != nil {
					t.Fatal(err)
				}
				contract, err := ParseAgentOutputContract(args)
				if err != nil || contract == nil || contract.RepairAttempts != 2 {
					t.Fatalf("repair budget for %s: contract=%+v error=%v", raw, contract, err)
				}
			})
		}
	}
}

func TestOutputRepairBudgetHandlesUnusableJSONNumbers(t *testing.T) {
	for _, raw := range []string{"2.5", "1e100000000000000000000", "1e-100000000000000000000", "invalid"} {
		for _, key := range []string{"repair_attempts", "repairAttempts", "repair-attempts"} {
			t.Run(key+"/"+raw, func(t *testing.T) {
				contract, err := ParseAgentOutputContract(map[string]any{"format": "json", key: json.Number(raw)})
				if key == "repair-attempts" {
					if err == nil {
						t.Fatalf("invalid repair count accepted: %+v", contract)
					}
				} else if err != nil || contract == nil || contract.RepairAttempts != 1 {
					t.Fatalf("legacy repair count fallback changed: contract=%+v error=%v", contract, err)
				}
			})
		}
	}
}
