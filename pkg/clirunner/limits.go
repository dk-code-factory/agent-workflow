package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func turnContext(ctx context.Context, a Agent) (context.Context, context.CancelFunc, error) {
	duration := 30 * time.Minute
	if a.Limits.TurnTimeout != "" {
		var err error
		duration, err = time.ParseDuration(a.Limits.TurnTimeout)
		if err != nil || duration <= 0 {
			return nil, nil, fmt.Errorf("invalid role turn timeout")
		}
	}
	turn, cancel := context.WithTimeout(ctx, duration)
	return turn, cancel, nil
}
func outputLimit(a Agent) int {
	if a.Limits.MaxOutput > 0 && a.Limits.MaxOutput < maxOutput {
		return a.Limits.MaxOutput
	}
	return maxOutput
}
func limitResult(result workflows.SessionResult, a Agent) (workflows.SessionResult, error) {
	encoded, err := json.Marshal(result.Outputs)
	if err != nil {
		return result, err
	}
	if len(encoded) > outputLimit(a) {
		result.Outputs = nil
		return result, fmt.Errorf("provider output exceeds configured limit")
	}
	return result, nil
}
