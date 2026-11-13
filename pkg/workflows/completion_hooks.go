package workflows

import (
	"fmt"
)

func completionAware(step Step) bool {
	return len(step.OnResult)+len(step.OnComplete)+len(step.OnIncomplete) > 0
}

func completionSchema() map[string]any {
	return map[string]any{"type": "object", "required": []any{"complete"}, "properties": map[string]any{"complete": map[string]any{"type": "boolean"}}}
}
func verdictOutput() map[string]any {
	return map[string]any{"format": "json", "schema": completionSchema(), "repair-attempts": 0}
}

func validateCompletionContract(step Step, output any) error {
	if !completionAware(step) {
		return nil
	}
	if step.Uses != "agent/run" {
		return fmt.Errorf("completion hooks require agent/run")
	}
	contract, err := ParseAgentOutputContract(output)
	if err != nil {
		return err
	}
	if contract == nil {
		return fmt.Errorf("completion hooks require a JSON output schema with required boolean complete")
	}
	properties, _ := contract.Schema["properties"].(map[string]any)
	field, _ := properties["complete"].(map[string]any)
	required := false
	switch values := contract.Schema["required"].(type) {
	case []any:
		for _, value := range values {
			if value == "complete" {
				required = true
			}
		}
	case []string:
		for _, value := range values {
			if value == "complete" {
				required = true
			}
		}
	}
	if contract.Schema["type"] != "object" || field["type"] != "boolean" || !required {
		return fmt.Errorf("completion hooks require required boolean complete in an object schema")
	}
	return nil
}

func completionValue(outputs map[string]any) (bool, error) {
	value, ok := outputs["structured"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("completion result requires a JSON object")
	}
	complete, ok := value["complete"].(bool)
	if !ok {
		return false, fmt.Errorf("completion result requires boolean complete")
	}
	return complete, nil
}
