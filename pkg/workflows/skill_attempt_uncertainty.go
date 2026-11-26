package workflows

import (
	"encoding/json"
	"fmt"
)

type SkillAttemptUncertainty struct {
	Key       string          `json:"key"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Provider  string          `json:"provider"`
	Raw       json.RawMessage `json:"raw"`
}

func (e *SkillEvidence) ObserveUncertainAttempt(plan *SkillInvocation, observation SkillAttemptUncertainty) error {
	if plan == nil || plan.ID == "" || e.PlanID != "" && e.PlanID != plan.ID || observation.Key == "" || len(observation.Key) > 512 || observation.SessionID == "" || !json.Valid(observation.Raw) {
		return fmt.Errorf("native retry evidence identity is incomplete or changed")
	}
	var raw map[string]any
	if json.Unmarshal(observation.Raw, &raw) != nil {
		return fmt.Errorf("native attempt evidence is malformed")
	}
	valid := false
	switch observation.Provider {
	case "claude-sdk":
		valid = raw["session_id"] == observation.SessionID && (raw["type"] == "system" && raw["subtype"] == "api_retry" || raw["type"] == "tool_progress" && raw["subagent_retry"] != nil || raw["type"] == "assistant" && raw["error"] != nil)
	case "codex-app-server":
		params, _ := raw["params"].(map[string]any)
		valid = observation.TurnID != "" && raw["method"] == "error" && params["willRetry"] == true && params["threadId"] == observation.SessionID && params["turnId"] == observation.TurnID
	}
	if !valid {
		return fmt.Errorf("native attempt evidence omitted original public error provenance")
	}

	for _, o := range e.Observations {
		if o.SessionID != observation.SessionID {
			return fmt.Errorf("native retry changed original skill session")
		}
	}
	for _, o := range e.UncertainAttempts {
		if o.SessionID != observation.SessionID {
			return fmt.Errorf("native retry changed original session")
		}
		if o.Key == observation.Key {
			if actionHash(o) != actionHash(observation) {
				return fmt.Errorf("conflicting original native retry event")
			}
			return nil
		}
	}
	candidate := *e
	candidate.PlanID = plan.ID
	candidate.UncertainAttempts = append(append([]SkillAttemptUncertainty(nil), e.UncertainAttempts...), observation)
	rawBytes, _ := json.Marshal(candidate)
	if len(candidate.UncertainAttempts)+len(candidate.Observations)+len(candidate.Children) > MaxSkillObservations || len(rawBytes) > MaxSkillEvidenceBytes {
		return fmt.Errorf("native retry evidence exceeds retention limits")
	}
	*e = candidate
	return nil
}
