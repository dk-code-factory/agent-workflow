package workflows

import (
	"context"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"sort"
	"strings"
)

func (e *Executor) preflight(ctx context.Context, all map[string]*Workflow) error {
	if err := e.pinContainers(ctx, all); err != nil {
		return err
	}
	if err := e.pinHelperBundles(ctx, all); err != nil {
		return err
	}
	if err := e.preflightConversations(all); err != nil {
		return err
	}
	if hasInputWait(all) && externalInputJournal(e.Store) == nil {
		return fmt.Errorf("input/wait requires a durable ExternalInputJournal (%s)", DurableExternalInputV1)
	}
	for _, w := range all {
		for _, j := range w.Jobs {
			if j.ForEach != nil && iterationJournal(e.Store) == nil {
				return fmt.Errorf("for-each requires a durable IterationJournal (%s)", OrderedDynamicIterationV1)
			}
		}
	}
	for _, workflow := range all {
		for _, job := range workflow.Jobs {
			for _, step := range lifecycleSteps(job.Steps) {
				if step.Uses == "agent/run" {
					if e.Workers == nil {
						return fmt.Errorf("agent/run requires durable controller-owned workers")
					}
					if _, ok := e.Store.(Journal); !ok {
						return fmt.Errorf("agent/run requires durable step receipts")
					}
					if _, ok := e.Agents.(NativeTaskRunner); !ok {
						return fmt.Errorf("agent/run requires the configured Codex runtime")
					}
				}
			}
		}
	}

	if err := e.configureActions(all); err != nil {
		return err
	}
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
	if err := runtimeexec.CheckPolicy(ctx, e.CommandExecution); err != nil {
		return err
	}
	if e.CommandExecution.Mode == "container" {
		for _, workflow := range all {
			for _, job := range workflow.Jobs {
				if job.Container != nil {
					continue
				}
				for _, step := range lifecycleSteps(job.Steps) {
					if job.Container == nil && !isDockerStep(step) && (len(step.Argv) > 0 || step.Run != "") {
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
			for _, step := range lifecycleSteps(job.Steps) {
				for _, role := range step.Conversation {
					set[role] = true
				}
				if strings.HasPrefix(step.Uses, "agent/") {
					set[agentRole(step)] = true
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
