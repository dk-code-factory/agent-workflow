package prfix

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// ManagedRunner preserves every Runner policy/capability method. Only the
// provider session binding changes when YAML explicitly recovers a stopped leaf.
type ManagedRunner struct {
	*clirunner.Runner
	Root string
}

func WrapRunner(root string, runner *clirunner.Runner) *ManagedRunner {
	return &ManagedRunner{runner, root}
}
func (r *ManagedRunner) WithWorkerWorkspace(role, workspace string) (workflows.SessionRunner, error) {
	scoped, err := r.Runner.WithWorkerWorkspace(role, workspace)
	if err != nil {
		return nil, err
	}
	runner, ok := scoped.(*clirunner.Runner)
	if !ok {
		return nil, fmt.Errorf("unexpected scoped CLI runner %T", scoped)
	}
	return WrapRunner(r.Root, runner), nil
}
func (r *ManagedRunner) RunTurn(ctx context.Context, req workflows.SessionRequest, observe func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if e := RestoreSession(ctx, r.Root, &req); e != nil {
		return workflows.SessionResult{}, e
	}
	return r.Runner.RunTurn(ctx, req, observe)
}
func RestoreSession(ctx context.Context, root string, req *workflows.SessionRequest) error {
	if !strings.HasPrefix(req.ParentWorkerID, "prfix_") {
		return nil
	}
	if _, e := os.Stat(filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")); os.IsNotExist(e) {
		return nil
	}
	l, e := OpenLedger(root)
	if e != nil {
		return e
	}
	defer l.Close()
	var session, owner, role string
	e = l.db.QueryRowContext(ctx, "SELECT session,owner,role FROM worker_links WHERE id=? AND engine=?", req.ParentWorkerID, req.RunID).Scan(&session, &owner, &role)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if role != req.AgentID {
		return fmt.Errorf("recovered worker role binding changed")
	}
	if req.SessionID != "" && session != "" && req.SessionID != session {
		return fmt.Errorf("recovered provider session binding changed")
	}
	if req.SessionID == "" {
		req.SessionID = session
	}
	req.SessionOwnerID = owner
	return nil
}
func (r *Runtime) roundWorker(ctx context.Context, c claimRecord, round int) (string, error) {
	if round == 0 {
		return c.Worker, nil
	}
	l, e := r.ledger()
	if e != nil {
		return "", e
	}
	defer l.Close()
	var worker string
	e = l.db.QueryRowContext(ctx, "SELECT worker FROM worker_rounds WHERE run=? AND claim=? AND round=?", c.Run, c.ID, round).Scan(&worker)
	if errors.Is(e, sql.ErrNoRows) {
		return c.Worker, nil
	}
	return worker, e
}
func (r *Runtime) workerRecover(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
	out := output()
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return out, e
	}
	l.Close()
	oldID, e := r.roundWorker(ctx, c, in.Round)
	if e != nil {
		return out, e
	}
	old, e := r.Workers.Get(ctx, c.Engine, oldID)
	if e != nil {
		return out, e
	}
	if old.Status != "closed" && old.Status != workflows.RunStatusNeedsAttention {
		return out, fmt.Errorf("original worker is not stopped")
	}
	messages, e := r.Workers.Messages(ctx, c.Engine, oldID)
	if e != nil {
		return out, e
	}
	if e = r.Store.CheckInvocationCleanup(ctx, c.Engine); e != nil {
		return out, e
	}
	if e = r.Store.CheckToolEffects(ctx, c.Engine); e != nil {
		return out, e
	}
	started := false
	for _, m := range messages {
		if m.Status == workflows.RunStatusRunning || m.Status == workflows.RunStatusQueued {
			return out, fmt.Errorf("original native turn must settle before recovery")
		}
		started = started || !m.StartedAt.IsZero()
	}
	if started && old.SessionID == "" {
		return out, fmt.Errorf("original native session binding is unknown; no fresh analysis is dispatched")
	}
	owner := oldID
	session := old.SessionID
	l, e = r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	var originalOwner, originalSession string
	e = l.db.QueryRowContext(ctx, "SELECT owner,session FROM worker_links WHERE id=?", oldID).Scan(&originalOwner, &originalSession)
	if e == nil {
		owner = originalOwner
		if session == "" {
			session = originalSession
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	stable := "prfix_" + digest([]any{p.LogicalRun, c.ID, in.Round, ac.OperationKey})[:32]
	o, e := p.Occurrence(c.Occurrence)
	if e != nil {
		return out, e
	}
	if _, e = l.db.ExecContext(ctx, "INSERT INTO worker_links(id,engine,claim,round,source,session,owner,role) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", stable, c.Engine, c.ID, in.Round, oldID, session, owner, o.Pass.Role); e != nil {
		return out, e
	}
	worker, e := r.Workers.Spawn(ctx, workflows.WorkerSpec{RunID: c.Engine, Role: o.Pass.Role, StableID: stable, Turns: 5})
	if e != nil {
		return out, e
	}
	out = claimOutput(c)
	out.Worker = worker.ID
	return out, nil
}
func (r *Runtime) workerAdopt(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return out, e
	}
	defer l.Close()
	var engine, claim string
	var round int
	if e = l.db.QueryRowContext(ctx, "SELECT engine,claim,round FROM worker_links WHERE id=?", in.Worker).Scan(&engine, &claim, &round); e != nil {
		return out, e
	}
	if engine != c.Engine || claim != c.ID || round != in.Round {
		return out, fmt.Errorf("recovered worker claim/round identity differs")
	}
	if _, e = r.Workers.Get(ctx, engine, in.Worker); e != nil {
		return out, e
	}
	if in.Round == 0 {
		if c.ReportMessage != "" {
			return out, fmt.Errorf("completed primary analysis cannot be replaced")
		}
		_, e = l.db.ExecContext(ctx, "UPDATE claims SET worker=? WHERE run=? AND id=?", in.Worker, p.LogicalRun, c.ID)
	} else {
		_, e = l.db.ExecContext(ctx, "INSERT INTO worker_rounds VALUES(?,?,?,?) ON CONFLICT(run,claim,round) DO UPDATE SET worker=excluded.worker", p.LogicalRun, c.ID, in.Round, in.Worker)
	}
	out = claimOutput(c)
	out.Worker = in.Worker
	return out, e
}
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
