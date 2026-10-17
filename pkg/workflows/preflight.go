package workflows

import (
	"context"
	"github.com/dkropachev/agent-workflow/pkg/runtimeexec"
	"sort"
	"strings"
)

func (e *Executor) preflight(ctx context.Context, all map[string]*Workflow) error {
	if e.Ready != nil {
		if err := e.Ready(ctx); err != nil {
			return err
		}
	}
	if store := invocationStore(e.Store); store != nil {
		if err := store.CheckCleanupPrerequisites(ctx); err != nil {
			return err
		}
	}
	if err := e.CommandExecution.Validate(); err != nil {
		return err
	}
	if e.CommandExecution.Mode == "container" {
		for _, workflow := range all {
			for _, job := range workflow.Jobs {
				for _, step := range job.Steps {
					if len(step.Argv) > 0 || step.Run != "" {
						return e.preflightWithContainer(ctx, all)
					}
				}
			}
		}
	}
	return e.preflightAgents(ctx, all)
}
func (e *Executor) preflightWithContainer(ctx context.Context, all map[string]*Workflow) error {
	probe, cancel := runtimeexec.ProbeContext(ctx)
	defer cancel()
	if err := runtimeexec.CheckImage(probe, e.CommandExecution, false); err != nil {
		return err
	}
	return e.preflightAgents(ctx, all)
}
func (e *Executor) preflightAgents(ctx context.Context, all map[string]*Workflow) error {
	checker, ok := e.Agents.(interface {
		Preflight(context.Context, []string) error
	})
	if !ok {
		return nil
	}
	set := map[string]bool{}
	for _, w := range all {
		for _, job := range w.Jobs {
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "agent/") {
					set[strings.TrimPrefix(step.Uses, "agent/")] = true
				}
			}
		}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return checker.Preflight(ctx, ids)
}
