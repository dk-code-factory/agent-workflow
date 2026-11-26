package workflows

import (
	"context"
	"database/sql"
	"math"
)

// Retain the known child-message lower bound before a parent terminal exists.
// SDK assistant content blocks can repeat a message ID and its usage. Sum each
// original message once; a parent Agent result is a floor, not another charge.
func skillChildPartialUsage(plan *SkillInvocation, ledger *SkillEvidence) (UsageUpdate, bool) {
	if plan == nil || ledger == nil || ledger.PlanID != plan.ID || len(plan.Children) != 1 {
		return UsageUpdate{}, false
	}
	var call, session string
	for _, o := range ledger.Children {
		if o.Kind == "permit" {
			call, session = o.ParentCallID, o.SessionID
			break
		}
	}
	if call == "" {
		return UsageUpdate{}, false
	}
	fields := []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
	messages := map[string]map[string]*int64{}
	floor := map[string]*int64{}
	for _, o := range ledger.Children {
		if o.ParentCallID != call || o.SessionID != session {
			continue
		}
		raw := childRaw(o)
		if o.Kind == "message" && raw["type"] == "assistant" {
			message := childObject(raw["message"])
			id := nativeString(message, "id")
			if id == "" {
				continue
			}
			usage := childObject(message["usage"])
			if messages[id] == nil {
				messages[id] = map[string]*int64{}
			}
			for _, k := range fields {
				messages[id][k] = maxTokens(messages[id][k], tokenValue(usage[k]))
			}
		}
		if o.Kind == "result" || o.Kind == "failed_result" {
			for _, k := range fields {
				floor[k] = maxTokens(floor[k], tokenValue(childObject(childObject(raw["tool_use_result"])["usage"])[k]))
			}
		}
	}
	total := map[string]any{}
	for _, k := range fields {
		var sum *int64
		for _, usage := range messages {
			if !addTokens(&sum, usage[k]) {
				n := int64(math.MaxInt64)
				sum = &n
			}
		}
		sum = maxTokens(sum, floor[k])
		if sum != nil {
			total[k] = *sum
		}
	}
	if len(total) == 0 {
		return UsageUpdate{}, false
	}
	return UsageUpdate{Key: "skill-child-partial:" + call, Provider: "claude-sdk", Model: plan.Model, SessionID: session, Usage: total, Incomplete: true}, true
}

func retainSkillChildPartialUsageTx(ctx context.Context, tx *sql.Tx, identity UsageIdentity, worker Worker) error {
	if update, ok := skillChildPartialUsage(worker.Skill, worker.SkillEvidence); ok {
		return observeUsageTx(ctx, tx, identity, update)
	}
	return nil
}
