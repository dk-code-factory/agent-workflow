package workflows

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type SkillChildObservation struct {
	Key           string          `json:"key"`
	Kind          string          `json:"kind"`
	SessionID     string          `json:"session_id"`
	ParentCallID  string          `json:"parent_call_id"`
	ChildID       string          `json:"child_id,omitempty"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Raw           json.RawMessage `json:"raw"`
	ParentMessage json.RawMessage `json:"parent_message,omitempty"`
}

func childObject(value any) map[string]any { m, _ := value.(map[string]any); return m }
func childArray(value any) []any           { a, _ := value.([]any); return a }

func childRaw(observation SkillChildObservation) map[string]any {
	var value map[string]any
	_ = json.Unmarshal(observation.Raw, &value)
	return value
}

func (e *SkillEvidence) ObserveChild(plan *SkillInvocation, observation SkillChildObservation) error {
	if plan == nil || len(plan.Children) != 1 || e.PlanID != plan.ID || observation.Name != plan.Children[0].Name || observation.Type != plan.Children[0].Type || observation.Key == "" || len(observation.Key) > 512 || observation.SessionID == "" || observation.ParentCallID == "" || len(observation.Raw) == 0 || !json.Valid(observation.Raw) {
		return fmt.Errorf("skill child evidence identity is incomplete or changed")
	}
	for _, parent := range e.Observations {
		if parent.SessionID != observation.SessionID {
			return fmt.Errorf("skill child changed original session")
		}
	}
	raw := childRaw(observation)
	if nativeString(raw, "session_id") != observation.SessionID {
		return fmt.Errorf("skill child raw session differs")
	}
	switch observation.Kind {
	case "permit":
		if raw["hook_event_name"] != "PreToolUse" || raw["tool_name"] != "Agent" || raw["tool_use_id"] != observation.ParentCallID || nativeString(raw, "agent_id") != "" {
			return fmt.Errorf("skill child permit omitted original parent hook")
		}
		args := childObject(raw["tool_input"])
		child := plan.Children[0]
		if len(args) != 4 || args["subagent_type"] != child.Type || args["prompt"] != child.Prompt || args["description"] != child.Description || args["run_in_background"] != false || childObject(raw["effort"])["level"] != plan.ResolvedEffort {
			return fmt.Errorf("skill child permit differs from pinned controls")
		}
		var parent map[string]any
		if json.Unmarshal(observation.ParentMessage, &parent) != nil || parent["type"] != "assistant" || parent["session_id"] != observation.SessionID || parent["parent_tool_use_id"] != nil || childObject(parent["message"])["model"] != plan.Model || childObject(parent["message"])["role"] != "assistant" || nativeString(childObject(parent["message"]), "id") == "" {
			return fmt.Errorf("skill child permit omitted original parent assistant call")
		}
		matched := 0
		for _, block := range childArray(childObject(parent["message"])["content"]) {
			value := childObject(block)
			if value["type"] == "tool_use" && value["id"] == observation.ParentCallID && value["name"] == "Agent" && actionHash(value["input"]) == actionHash(args) {
				matched++
			}
		}
		if matched != 1 {
			return fmt.Errorf("original parent child call differs from normalized hook controls")
		}
	case "start", "stop":
		want := "SubagentStart"
		if observation.Kind == "stop" {
			want = "SubagentStop"
		}
		if raw["hook_event_name"] != want || nativeString(raw, "agent_id") == "" || raw["agent_id"] != observation.ChildID || raw["agent_type"] != observation.Type {
			return fmt.Errorf("skill child hook identity differs")
		}
	case "message":
		if raw["parent_tool_use_id"] != observation.ParentCallID || raw["agent_id"] != observation.ChildID || raw["subagent_type"] != observation.Type || raw["type"] != "assistant" && raw["type"] != "user" {
			return fmt.Errorf("skill child message omitted native parent edge")
		}
	case "task":
		parentEdge := raw["tool_use_id"] == observation.ParentCallID
		// The pinned task_updated envelope has only task/run IDs. It is retained
		// and joined to the original task_started edge, never completion proof.
		if raw["subtype"] == "task_updated" && raw["tool_use_id"] == nil {
			parentEdge = true
		}
		if raw["type"] != "system" || !strings.HasPrefix(nativeString(raw, "subtype"), "task_") || !parentEdge || raw["task_id"] != observation.ChildID || nativeString(raw, "run_id") == "" {
			return fmt.Errorf("skill child task identity differs")
		}
	case "result":
		if raw["type"] != "user" || raw["parent_tool_use_id"] != nil {
			return fmt.Errorf("skill child result is not an original parent tool result")
		}
		result := childObject(raw["tool_use_result"])
		if result["agentId"] != observation.ChildID || result["agentType"] != observation.Type {
			return fmt.Errorf("skill child original result identity differs")
		}
		matched := false
		for _, item := range childArray(childObject(raw["message"])["content"]) {
			value := childObject(item)
			if value["type"] == "tool_result" && value["tool_use_id"] == observation.ParentCallID && value["is_error"] != true {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("skill child result omitted original call edge")
		}
	case "failed_result":
		if raw["type"] != "user" || raw["parent_tool_use_id"] != nil {
			return fmt.Errorf("skill child failure omitted original parent result")
		}
		matched := false
		for _, item := range childArray(childObject(raw["message"])["content"]) {
			value := childObject(item)
			if value["type"] == "tool_result" && value["tool_use_id"] == observation.ParentCallID && value["is_error"] == true {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("skill child failure omitted original call edge")
		}
	case "parent_terminal":
		if raw["type"] != "result" {
			return fmt.Errorf("skill child parent terminal is not a provider result")
		}
	default:
		return fmt.Errorf("unknown skill child evidence kind")
	}
	for _, old := range e.Children {
		if old.SessionID != observation.SessionID || old.ParentCallID != observation.ParentCallID || old.ChildID != "" && observation.ChildID != "" && old.ChildID != observation.ChildID {
			return fmt.Errorf("skill child changed original child/parent identity")
		}
		if old.Key == observation.Key {
			if actionHash(old) != actionHash(observation) {
				return fmt.Errorf("conflicting original skill child event")
			}
			return nil
		}
		if old.Kind == observation.Kind && slices.Contains([]string{"permit", "start", "stop", "result", "parent_terminal"}, observation.Kind) {
			return fmt.Errorf("conflicting original skill child lifecycle event")
		}
		if old.Kind == "task" && observation.Kind == "task" && childRaw(old)["run_id"] != raw["run_id"] {
			return fmt.Errorf("skill child native task run identity changed")
		}
	}
	candidate := *e
	candidate.Children = append(append([]SkillChildObservation(nil), e.Children...), observation)
	data, _ := json.Marshal(candidate)
	if len(candidate.Children)+len(candidate.Observations)+len(candidate.UncertainAttempts) > MaxSkillObservations || len(data) > MaxSkillEvidenceBytes {
		return fmt.Errorf("skill child evidence exceeds retention limits")
	}
	*e = candidate
	return nil
}

// Completion joins hook/task/result observations across arrival order while
// preserving child message stream order. A task summary or status=completed
// cannot substitute for the original stopped child's output.
func (e *SkillEvidence) completeChildren(plan *SkillInvocation) error {
	if len(plan.Children) == 0 {
		return nil
	}
	if len(plan.Children) != 1 {
		return fmt.Errorf("unsupported retained child declaration")
	}
	child := plan.Children[0]
	var permit, start, stop, taskStart, taskEnd, result, parent bool
	var finalText, stopText string
	var finalTools bool
	var output []any
	messageIDs := []string{}
	reads, results := map[string]bool{}, map[string]bool{}
	for _, observation := range e.Children {
		raw := childRaw(observation)
		switch observation.Kind {
		case "permit":
			permit = true
		case "start":
			start = true
		case "stop":
			if childObject(raw["effort"])["level"] != child.Effort {
				return fmt.Errorf("skill child resolved effort differs")
			}
			stop = true
			stopText = nativeString(raw, "last_assistant_message")
		case "task":
			if raw["subtype"] == "task_started" {
				if raw["subagent_type"] != child.Type || raw["is_backgrounded"] != false || raw["spawn_depth"] != float64(1) || raw["task_type"] != "local_agent" || raw["prompt"] != child.Prompt || raw["description"] != child.Description {
					return fmt.Errorf("skill child was not the admitted foreground child")
				}
				taskStart = true
			}
			if raw["subtype"] == "task_notification" && raw["status"] == "completed" {
				taskEnd = true
			}
		case "message":
			if raw["type"] == "user" {
				for _, block := range childArray(childObject(raw["message"])["content"]) {
					value := childObject(block)
					if value["type"] == "tool_result" {
						id := nativeString(value, "tool_use_id")
						if id == "" || value["is_error"] == true {
							return fmt.Errorf("skill child tool output failed or omitted identity")
						}
						results[id] = true
					}
				}
			}
			if raw["type"] != "assistant" {
				continue
			}
			if raw["error"] != nil {
				return fmt.Errorf("original native child returned an API error")
			}
			message := childObject(raw["message"])
			id := nativeString(message, "id")
			if id == "" || message["model"] != child.Model {
				return fmt.Errorf("skill child message changed admitted model or identity")
			}
			if !slices.Contains(messageIDs, id) {
				messageIDs = append(messageIDs, id)
				finalText = ""
				finalTools = false
			}
			if len(messageIDs) > child.MaxTurns {
				return fmt.Errorf("skill child exceeded admitted turns")
			}
			for _, block := range childArray(message["content"]) {
				value := childObject(block)
				if value["type"] == "tool_use" {
					id := nativeString(value, "id")
					if value["name"] != "Read" || id == "" {
						return fmt.Errorf("skill child requested an undeclared tool")
					}
					reads[id] = true
					finalTools = true
				}
				if value["type"] == "text" {
					finalText += nativeString(value, "text")
				}
			}
		case "result":
			value := childObject(raw["tool_use_result"])
			if value["status"] != "completed" || value["resolvedModel"] != child.Model || value["prompt"] != child.Prompt {
				return fmt.Errorf("skill child result did not complete with admitted controls")
			}
			result = true
			output = childArray(value["content"])
		case "parent_terminal":
			if _, err := SkillChildUsage(raw); err != nil {
				return err
			}
			parent = raw["subtype"] == "success" && raw["is_error"] != true && raw["api_error"] == nil && raw["api_error_status"] == nil
		}
	}
	fullOutput := false
	for id := range reads {
		if !results[id] {
			return fmt.Errorf("skill child original tool output is missing")
		}
	}
	for id := range results {
		if !reads[id] {
			return fmt.Errorf("skill child tool result has no original call")
		}
	}
	for _, block := range output {
		value := childObject(block)
		if value["type"] == "text" && nativeString(value, "text") == stopText && stopText != "" {
			fullOutput = true
		}
	}
	if !permit || !start || !stop || !taskStart || !taskEnd || !result || !parent || !fullOutput || finalTools || finalText == "" || finalText != stopText {
		return fmt.Errorf("required original skill child evidence remains incomplete")
	}
	return nil
}
