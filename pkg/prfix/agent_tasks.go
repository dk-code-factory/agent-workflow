package prfix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Task preparation owns PR claims and checklist policy, never provider dispatch.
func (r *Runtime) taskPrepare(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, err := r.record(ctx, p, in)
	if err != nil {
		return output(), err
	}
	defer l.Close()
	if c.State == "complete" || c.State == "optional" {
		return claimOutput(c), nil
	}
	if in.Round > 0 && (c.State != "recheck" || in.Round != c.Checks) {
		return output(), fmt.Errorf("checklist round is not the retained pending round")
	}
	assignment, err := os.ReadFile(c.Assignment)
	if err != nil {
		return output(), err
	}
	out := claimOutput(c)
	out.Invoke = true
	out.Agent = c.Worker
	out.Prompt = "Perform the supplied PR task under its retained assignment. Return the original JSON report with completion and every checklist item."
	out.TaskInput = map[string]any{"claim": c.ID, "round": in.Round, "assignment": json.RawMessage(assignment)}
	if in.Round > 0 {
		if !strings.HasPrefix(c.Worker, "agent_") {
			return output(), fmt.Errorf("original pipeline agent binding is unavailable")
		}
		o, err := p.Occurrence(c.Occurrence)
		if err != nil {
			return output(), err
		}
		out.TaskInput["checklist"] = o.Checklist
		out.Prompt = "Verify every checklist item in this same session and finish missing work. Preserve the original analysis, findings and budgets. Return the original JSON report."
	}
	return out, nil
}

func (r *Runtime) taskResult(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
	l, c, err := r.record(ctx, p, in)
	if err != nil {
		return output(), err
	}
	defer l.Close()
	if c.State == "complete" || c.State == "optional" {
		return claimOutput(c), nil
	}
	origin, err := r.origin(ctx, ac)
	if err != nil {
		return output(), err
	}
	if origin != c.Engine || !strings.HasPrefix(in.Agent, "agent_") {
		return output(), fmt.Errorf("pipeline agent belongs to another claim execution")
	}
	worker, err := r.Workers.Get(ctx, c.Engine, in.Agent)
	if err != nil {
		return output(), err
	}
	if worker.Role != "codex" || worker.Definition == nil || worker.SessionID == "" {
		return output(), fmt.Errorf("original Codex task session binding is unavailable")
	}
	if strings.HasPrefix(c.Worker, "agent_") && c.Worker != in.Agent {
		return output(), fmt.Errorf("pipeline agent binding changed")
	}
	messages, err := r.Workers.Messages(ctx, c.Engine, in.Agent)
	if err != nil {
		return output(), err
	}
	matched := false
	for _, message := range messages {
		if input, ok := message.Input.(map[string]any); ok && input["claim"] == c.ID && fmt.Sprint(input["round"]) == fmt.Sprint(in.Round) {
			matched = true
		}
	}
	if !matched {
		return output(), fmt.Errorf("pipeline agent has no matching retained task input")
	}
	if _, err = l.db.ExecContext(ctx, "UPDATE claims SET worker=? WHERE run=? AND id=?", in.Agent, c.Run, c.ID); err != nil {
		return output(), err
	}
	c.Worker = in.Agent
	out := claimOutput(c)
	out.Settled = true
	return out, nil
}
