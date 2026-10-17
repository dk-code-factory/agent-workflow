package workflows

import (
	"context"
	"sort"
	"strings"
)

func (e *Executor) preflight(ctx context.Context, all map[string]*Workflow) error {
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
