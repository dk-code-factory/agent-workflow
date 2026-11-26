package workflows

import (
	"errors"
	"fmt"
	"math"
)

// SkillChildUsage interprets only the exact pinned SDK modelUsage surface.
// Parent usage is retained separately: it excludes native child calls. Even a
// malformed/incomplete receipt returns the known cumulative charges alongside
// its error so callers can commit them before failing the original turn.
func SkillChildUsage(raw map[string]any) (UsageUpdate, error) {
	update := UsageUpdate{CounterBasis: ClaudeChildUsageBasis, Provider: "claude-sdk", Model: "claude-sonnet-4-6", SessionID: nativeString(raw, "session_id"), Final: true}
	models := childObject(raw["modelUsage"])
	var problems []error
	if raw["type"] != "result" || update.SessionID == "" || len(models) != 1 || models[update.Model] == nil {
		problems = append(problems, fmt.Errorf("native child accounting omitted exact original model/session"))
	}
	if raw["is_error"] == true || raw["api_error"] != nil || raw["api_error_status"] != nil {
		problems = append(problems, fmt.Errorf("native child original parent terminal reports an API failure"))
	}
	total := map[string]any{}
	for target, source := range map[string]string{"input_tokens": "inputTokens", "output_tokens": "outputTokens", "cache_read_input_tokens": "cacheReadInputTokens", "cache_creation_input_tokens": "cacheCreationInputTokens"} {
		var sum int64
		known := false
		for _, item := range models {
			value := tokenValue(childObject(item)[source])
			if value == nil || *value > math.MaxInt64-sum {
				problems = append(problems, fmt.Errorf("native child accounting has incomplete %s", source))
				continue
			}
			known = true
			sum += *value
		}
		if known {
			total[target] = sum
		}
		floor := tokenValue(childObject(raw["usage"])[target])
		if floor == nil || !known || *floor > sum {
			problems = append(problems, fmt.Errorf("native child session accounting conflicts with original parent %s", target))
		}
		if floor != nil && (!known || *floor > sum) {
			total[target] = *floor
		}
	}
	var modelCost float64
	allCosts := len(models) > 0
	anyCost := false
	for name, item := range models {
		model := childObject(item)
		if name != update.Model || model["canonicalModel"] != update.Model || model["provider"] != "firstParty" || model["costBasis"] != "list" {
			problems = append(problems, fmt.Errorf("native child model accounting provenance changed"))
		}
		if cost := costValue(model["costUSD"]); cost != nil {
			modelCost += *cost
			anyCost = true
		} else {
			allCosts = false
		}
	}
	if cost := costValue(raw["total_cost_usd"]); cost != nil {
		update.CostUSD = *cost
		if !allCosts || math.Abs(*cost-modelCost) > 1e-12 {
			problems = append(problems, fmt.Errorf("native child cost accounting is incomplete or conflicting"))
			if modelCost > *cost && !math.IsInf(modelCost, 0) {
				update.CostUSD = modelCost
			}
		}
	} else {
		problems = append(problems, fmt.Errorf("native child original total cost is missing"))
		if anyCost && !math.IsInf(modelCost, 0) {
			update.CostUSD = modelCost
		}
	}
	update.Usage = map[string]any{"total": total}
	update.Incomplete = len(problems) != 0
	return update, errors.Join(problems...)
}
