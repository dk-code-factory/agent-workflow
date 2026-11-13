package prfix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (r *Runtime) turnState(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	l.Close()
	out := claimOutput(c)
	m, e := r.message(ctx, c, in.Round)
	if errors.Is(e, sql.ErrNoRows) {
		out.Settled = false
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.Settled = m.Status != workflows.RunStatusRunning && m.Status != workflows.RunStatusQueued && !resumableNative(m)
	return out, nil
}
func (r *Runtime) prepareCheck(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	l.Close()
	out := claimOutput(c)
	out.Round = c.Checks
	out.Invoke = c.State == "recheck"
	return out, nil
}
func resumableNative(m workflows.WorkerMessage) bool {
	return m.Status == "interrupted" || m.Status == workflows.RunStatusCanceled || m.FailureClass == "canceled" || strings.Contains(m.Error, "owner stopped") || strings.Contains(m.Error, "context canceled")
}

// YAML calls this after status accepts optionality or an eligible retry. Native
// success, accepted coverage, and retry reservations remain separate evidence.
func (r *Runtime) workerAcknowledge(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput, perform bool) (string, ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return "unknown", output(), e
	}
	l.Close()
	out := claimOutput(c)
	if c.State != "optional" && !(c.State == "incomplete" && (c.Reason == "usage_limit" || c.Reason == "session_limit")) {
		return "completed", out, nil
	}
	if _, e = r.status(ctx, p, in); e != nil {
		return "unknown", out, e
	}
	origin, e := r.origin(ctx, ac)
	if e != nil {
		return "unknown", out, e
	}
	if origin != c.Engine {
		return "unknown", out, fmt.Errorf("failure acknowledgement belongs to another engine run")
	}
	messages, e := r.claimMessages(ctx, c)
	if e != nil {
		return "unknown", out, e
	}
	var msg *workflows.WorkerMessage
	for n := range messages {
		if messages[n].ID == c.Message {
			msg = &messages[n]
			break
		}
	}
	if msg == nil {
		return "unknown", out, fmt.Errorf("submitted native failure is unavailable")
	}
	if msg.Status != workflows.RunStatusFailed {
		return "completed", out, nil
	}
	reason := "optional occurrence failure"
	if c.State == "incomplete" {
		reason = "eligible " + c.Reason + " retry"
	}
	req := workflows.WorkerFailureRequest{RunID: c.Engine, WorkerID: msg.WorkerID, MessageID: msg.ID, Generation: msg.Generation, RequestID: "prfix_" + digest([]any{c.ID, msg.ID, "failure"})[:48], Reason: reason, Evidence: messageRef(c, *msg)}
	if ack := msg.FailureAcknowledgement; ack != nil {
		if strings.HasPrefix(c.Worker, "agent_") && ack.MessageID == msg.ID && ack.Generation == msg.Generation {
			return "completed", out, nil
		}
		if ack.RequestID != req.RequestID || ack.MessageID != req.MessageID || ack.Generation != req.Generation || ack.Reason != req.Reason || ack.Evidence != req.Evidence {
			return "unknown", out, fmt.Errorf("native failure acknowledgement differs from the claim")
		}
		return "completed", out, nil
	}
	run, _, e := r.Store.Load(ctx, c.Engine)
	if e != nil {
		return "unknown", out, e
	}
	// Explicit normal engine resume already retains failures from older generations.
	if msg.Generation < run.Generation {
		return "completed", out, nil
	}
	if !perform {
		return "no_effect", out, nil
	}
	req.Owner = run.Owner
	_, e = r.Workers.AcknowledgeFailure(ctx, req)
	if e != nil {
		return "unknown", out, e
	}
	return "completed", out, nil
}

// reviewAdmission reserves the cycle before any cycle hook can run. Reused
// accepted reviewer claims require neither a new reservation nor hook execution.
func (r *Runtime) reviewAdmission(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	s, err := r.live(ctx, p)
	if err != nil {
		return out, err
	}
	l, err := r.ledger()
	if err != nil {
		return out, err
	}
	defer l.Close()
	err = l.transaction(ctx, func(tx sqlRW) error {
		for _, o := range p.Occurrences {
			if !o.Review {
				continue
			}
			var count int
			if o.Cacheable {
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM claims WHERE run=? AND occurrence=? AND fingerprint=? AND state='complete'", p.LogicalRun, o.ID, occurrenceFingerprint(s, o)).Scan(&count); err != nil {
					return err
				}
			}
			if count == 0 {
				out.Invoke = true
			}
		}
		if !out.Invoke {
			return nil
		}
		if !validText(in.Cycle) {
			return fmt.Errorf("review admission requires retained cycle")
		}
		return reserve(ctx, tx, p.LogicalRun, "review", "all", in.Cycle, "selected reviewer pass")
	})
	return out, err
}

func cycleHook(p Plan, id string) bool {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		for _, group := range p.Groups[phase] {
			if contains(group, id) {
				return true
			}
		}
	}
	return false
}
