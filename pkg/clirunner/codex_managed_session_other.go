//go:build !linux

package clirunner

import (
	"context"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (r *Runner) codexManagedTurn(context.Context, Agent, workflows.SessionRequest, func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	return workflows.SessionResult{}, fmt.Errorf("managed Codex executable verification is supported only on Linux amd64")
}
