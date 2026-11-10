package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type ReplyDisposition struct {
	Decision    string   `json:"decision"`
	Reason      string   `json:"reason"`
	Inspection  string   `json:"inspection"`
	Observation string   `json:"observation"`
	Sources     []string `json:"sources"`
}

type retainedReplyDisposition struct {
	Operation   string           `json:"operation"`
	Claim       string           `json:"claim"`
	Disposition ReplyDisposition `json:"disposition"`
	Observed    ReplyObservation `json:"observation"`
}

func latestReplyDisposition(ctx context.Context, l *Ledger, i Intent) (string, retainedReplyDisposition, error) {
	var path string
	var retained retainedReplyDisposition
	err := l.db.QueryRowContext(ctx, "SELECT reference FROM progress WHERE run=? AND source=? AND kind='reply_disposed' ORDER BY id DESC LIMIT 1", i.Run, i.ID).Scan(&path)
	if err == nil {
		err = readJSON(path, &retained)
	}
	return path, retained, err
}

func (r *Runtime) verifyReplyDisposition(ctx context.Context, l *Ledger, i Intent) (bool, error) {
	_, retained, err := latestReplyDisposition(ctx, l, i)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	d := retained.Disposition
	_, receipt, err := completedReply(ctx, l, i.Run, i.ID)
	if err != nil {
		return false, err
	}
	if d.Decision != "no_replacement" || !validText(d.Reason) || retained.Observed.Intent.Fingerprint() != i.Fingerprint() || digest(retained.Observed.Receipt) != digest(receipt) || d.Observation != retained.Observed.Fingerprint() {
		return false, fmt.Errorf("historical reply disposition identity differs")
	}
	var pending int
	if err := l.db.QueryRowContext(ctx, "SELECT count(*) FROM reply_replacements r JOIN effects e ON e.run=r.replacement_run AND e.id=r.replacement_id WHERE r.original_run=? AND r.original_id=? AND e.state IN ('prepared','dispatched','unknown')", i.Run, i.ID).Scan(&pending); err != nil {
		return false, err
	}
	if pending != 0 {
		return false, fmt.Errorf("linked replacement must be reconciled before reusing no_replacement")
	}
	current, err := r.inspectReply(ctx, i, receipt)
	if err != nil {
		return false, err
	}
	if current.Fingerprint() != d.Observation {
		return false, fmt.Errorf("reply disposition context changed; inspect and reassess again")
	}
	return true, nil
}

func (r *Runtime) replyDispose(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
	out := output()
	unlock, err := r.lockPublication(ctx)
	if err != nil {
		return out, err
	}
	defer unlock()
	l, err := r.ledger()
	if err != nil {
		return out, err
	}
	defer l.Close()
	i, receipt, err := completedReply(ctx, l, p.LogicalRun, in.Intent)
	if err != nil {
		return out, err
	}
	c, err := getClaim(ctx, l.db, p.LogicalRun, in.Claim)
	if err != nil {
		return out, err
	}
	if c.State != "complete" || c.Occurrence != "reply_recovery" {
		return out, fmt.Errorf("accepted exact historical reply reassessment required")
	}
	claimed, err := l.Intent(ctx, p.LogicalRun, c.Payload)
	if err != nil || claimed.Run != i.Run || claimed.ID != i.ID {
		return out, fmt.Errorf("reassessment belongs to another historical reply")
	}
	report, err := r.effectiveReport(ctx, c)
	if err != nil {
		return out, err
	}
	if report.ReplyDisposition == nil {
		return out, nil
	}
	d := *report.ReplyDisposition
	if d.Decision != "no_replacement" || !validText(d.Reason) || len(d.Sources) == 0 || !contains(d.Sources, d.Inspection) {
		return out, fmt.Errorf("explicit sourced no_replacement disposition required")
	}
	path, observed, err := latestReplyInspection(ctx, l, i.Run, i.ID)
	if err != nil {
		return out, err
	}
	var assignment map[string]json.RawMessage
	if err = readJSON(c.Assignment, &assignment); err != nil {
		return out, err
	}
	var inspection struct {
		Path        string           `json:"path"`
		Observation ReplyObservation `json:"observation"`
	}
	if err = json.Unmarshal(assignment["reply_inspection"], &inspection); err != nil {
		return out, err
	}
	if path != d.Inspection || observed.Fingerprint() != d.Observation || inspection.Path != path || inspection.Observation.Fingerprint() != d.Observation || digest(receipt) != digest(observed.Receipt) {
		return out, fmt.Errorf("no_replacement must consume exact original reply inspection")
	}
	current, err := r.inspectReply(ctx, i, receipt)
	if err != nil {
		return out, err
	}
	if current.Fingerprint() != d.Observation {
		return out, fmt.Errorf("reply context changed; inspect and reassess again")
	}
	for _, source := range i.Sources {
		if !contains(d.Sources, source) {
			return out, fmt.Errorf("no_replacement must retain original feedback sources")
		}
	}
	for _, source := range d.Sources {
		if err := r.validateSource(ctx, p, source); err != nil {
			return out, err
		}
	}
	out.Report, err = artifact(r.Root, retainedReplyDisposition{ac.OperationKey, c.ID, d, observed})
	if err != nil {
		return out, err
	}
	err = l.transaction(ctx, func(tx sqlRW) error {
		var pending int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM reply_replacements r JOIN effects e ON e.run=r.replacement_run AND e.id=r.replacement_id WHERE r.original_run=? AND r.original_id=? AND e.state IN ('dispatched','unknown')", i.Run, i.ID).Scan(&pending); err != nil {
			return err
		}
		if pending != 0 {
			return fmt.Errorf("reconcile uncertain linked replacement before disposition")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE effects SET state='superseded' WHERE state='prepared' AND (run,id) IN (SELECT replacement_run,replacement_id FROM reply_replacements WHERE original_run=? AND original_id=?)", i.Run, i.ID); err != nil {
			return err
		}
		var prior int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM progress WHERE run=? AND source=? AND kind='reply_disposed' AND reference=?", i.Run, i.ID, out.Report).Scan(&prior); err != nil {
			return err
		}
		if prior != 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", i.Run, i.ID, "reply_disposed", out.Report)
		return err
	})
	out.Settled = err == nil
	return out, err
}

func (r *Runtime) reconcileReplyDisposition(ctx context.Context, l *Ledger, i Intent, ac workflows.ActionContext, in ActionInput) (string, ActionOutput, error) {
	out := output()
	path, retained, err := latestReplyDisposition(ctx, l, i)
	if errors.Is(err, sql.ErrNoRows) {
		return "no_effect", out, nil
	}
	if err != nil {
		return "unknown", out, err
	}
	if retained.Operation != ac.OperationKey || retained.Claim != in.Claim {
		return "no_effect", out, nil
	}
	out.Report, out.Settled = path, true
	return "completed", out, nil
}
