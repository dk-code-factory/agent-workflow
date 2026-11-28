package workflows

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// NativeTaskPolicy is an admitted grant ceiling, not an observed-operation log.
// NativeTools is the exact intersection with the original runtime role.
type NativeTaskPolicy struct {
	Mode        string   `json:"mode"`
	Provider    string   `json:"provider"`
	Profile     string   `json:"profile"`
	NativeTools []string `json:"native_tools"`
}

func validateWorkerNativePolicy(worker Worker, runner SessionRunner) error {
	if err := validateNativeTaskPolicy(worker.Definition); err != nil {
		return err
	}
	if worker.Definition == nil || worker.Definition.NativePolicy == nil {
		return nil
	}
	if worker.Skill != nil || len(worker.ToolGrants) != 0 || actionHash(worker.Tools) != actionHash(worker.Definition.NativePolicy.NativeTools) {
		return fmt.Errorf("%w: retained native task authority changed", ErrActionIntegrity)
	}
	validator, ok := runner.(NativeTaskRunner)
	if !ok {
		return fmt.Errorf("native task policy requires an enforcing runtime")
	}
	admitted, err := validator.AdmitNativeTask(worker.Definition)
	if err != nil {
		return err
	}
	if actionHash(admitted) != actionHash(worker.Definition) {
		return fmt.Errorf("%w: retained native task policy changed", ErrActionIntegrity)
	}
	return nil
}

func retainNativeTaskPolicy(worker Worker, message WorkerMessage, result *SessionResult, success bool) error {
	// Provider/model output cannot substitute for controller grant evidence.
	delete(result.Outputs, "tool_policy")
	if worker.Definition == nil || worker.Definition.NativePolicy == nil {
		result.TaskPolicy = nil
		return nil
	}
	policy := worker.Definition.NativePolicy
	evidence := result.TaskPolicy
	if evidence == nil && !success {
		return nil
	}
	if evidence == nil || message.RepairOnly || result.SessionID == "" || actionHash(evidence.Grant) != actionHash(policy) || evidence.Agent != worker.ID || evidence.InvocationID != message.ID || evidence.SessionFingerprint != NativeTaskSessionFingerprint(result.SessionID) || evidence.FormattingTool != (message.Output != nil && message.Output.Schema != nil) {
		result.TaskPolicy = nil
		return fmt.Errorf("%w: native task grant evidence is missing or changed", ErrActionIntegrity)
	}
	if evidence.TaskAttemptID != "" && evidence.TaskAttemptID != message.TaskAttemptID {
		result.TaskPolicy = nil
		return fmt.Errorf("%w: native task grant attempt identity changed", ErrActionIntegrity)
	}
	copy := *evidence
	copy.TaskAttemptID = message.TaskAttemptID
	result.TaskPolicy = &copy
	raw, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	var output map[string]any
	if err := decodeJSONWithNumbers(raw, &output); err != nil {
		return err
	}
	if result.Outputs == nil {
		result.Outputs = map[string]any{}
	}
	result.Outputs["tool_policy"] = output
	return nil
}

func validateRetainedNativeTaskPolicy(worker Worker, message WorkerMessage) error {
	if worker.Definition == nil || worker.Definition.NativePolicy == nil {
		return nil
	}
	if err := validateNativeTaskPolicy(worker.Definition); err != nil {
		return err
	}
	// Wait receipts can outlive the original executor. Recheck both the typed
	// controller binding and its public output before exposing a frozen result.
	retained := message.Result
	retained.Outputs, _ = cloneJSONValue(message.Result.Outputs).(map[string]any)
	if err := retainNativeTaskPolicy(worker, message, &retained, message.Status == RunStatusSucceeded); err != nil {
		return err
	}
	if actionHash(retained) != actionHash(message.Result) {
		return fmt.Errorf("%w: retained native task grant output changed", ErrActionIntegrity)
	}
	return nil
}

// NativeTaskPolicyEvidence binds an enforced grant to the original native turn.
// The controller adds the durable task-attempt identity before storing outputs.
type NativeTaskPolicyEvidence struct {
	Grant              NativeTaskPolicy `json:"grant"`
	Agent              string           `json:"agent"`
	InvocationID       string           `json:"invocation_id"`
	TaskAttemptID      string           `json:"task_attempt_id"`
	SessionFingerprint string           `json:"session_fingerprint"`
	FormattingTool     bool             `json:"formatting_tool"`
}

func NativeTaskSessionFingerprint(session string) string {
	if session == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(session))
	return hex.EncodeToString(digest[:])
}

func validateNativeTaskPolicy(definition *AgentDefinition) error {
	if definition == nil {
		return nil
	}
	if definition.ToolPolicy == "" {
		if definition.NativePolicy != nil {
			return fmt.Errorf("%w: native policy lacks its declaration", ErrActionIntegrity)
		}
		return nil
	}
	p := definition.NativePolicy
	if definition.ToolPolicy != "read-only" || p == nil || p.Mode != definition.ToolPolicy || p.Provider == "" || p.Profile == "" || p.NativeTools == nil {
		return fmt.Errorf("%w: native task policy is not admitted", ErrActionIntegrity)
	}
	seen := map[string]bool{}
	for _, tool := range p.NativeTools {
		if tool != "Read" && tool != "Glob" && tool != "Grep" || seen[tool] {
			return fmt.Errorf("%w: native read-only tool ceiling changed", ErrActionIntegrity)
		}
		seen[tool] = true
	}
	return nil
}

func (e *Executor) validateNativeTaskPolicies(all map[string]*Workflow) error {
	for _, workflow := range all {
		for _, job := range workflow.Jobs {
			for _, step := range lifecycleSteps(job.Steps) {
				definitions := make([]*AgentDefinition, 0, len(step.Definitions)+1)
				definitions = append(definitions, step.Definition)
				for _, definition := range step.Definitions {
					definitions = append(definitions, definition)
				}
				for _, definition := range definitions {
					if err := validateNativeTaskPolicy(definition); err != nil {
						return err
					}
					if definition == nil || definition.ToolPolicy == "" {
						continue
					}
					if step.Skill != nil {
						return fmt.Errorf("native read-only tasks cannot invoke supplied skills")
					}
					output, err := resolveNamedOutput(workflow, step.With["output"])
					if err != nil {
						return err
					}
					contract, err := ParseAgentOutputContract(output)
					if err != nil {
						return err
					}
					if contract != nil && contract.RepairAttempts > 0 {
						return fmt.Errorf("native read-only tasks require output.repair-attempts: 0")
					}
					runner, ok := e.Agents.(NativeTaskRunner)
					if !ok {
						return fmt.Errorf("native task policy requires an enforcing runtime")
					}
					admitted, err := runner.AdmitNativeTask(definition)
					if err != nil {
						return err
					}
					if actionHash(admitted) != actionHash(definition) {
						return fmt.Errorf("%w: retained native task policy changed", ErrActionIntegrity)
					}
				}
			}
		}
	}
	return nil
}
