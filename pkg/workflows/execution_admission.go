package workflows

import (
	"context"
	"encoding/json"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// Pin both command and provider selections before discovery or admission. The
// resulting runner is private to this execution; concurrent runs never mutate
// the controller's configuration.
func (e *Executor) pinExecution(ctx context.Context) error {
	p, err := runtimeexec.PinPolicy(ctx, e.CommandExecution)
	if err != nil {
		return err
	}
	e.CommandExecution = p
	if source, ok := e.Agents.(interface {
		AdmittedRoles(context.Context) (json.RawMessage, error)
	}); ok {
		roles, err := source.AdmittedRoles(ctx)
		if err != nil {
			return err
		}
		e.Roles = roles
		if e.RoleRunner != nil {
			e.Agents, err = e.RoleRunner(roles)
		} else if source, ok := e.Agents.(interface {
			RunnerForRoles(json.RawMessage) (AgentRunner, error)
		}); ok {
			e.Agents, err = source.RunnerForRoles(roles)
		}
		return err
	}
	return nil
}
