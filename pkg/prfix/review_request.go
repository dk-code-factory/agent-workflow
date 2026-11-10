package prfix

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Inspection establishes current context, never whether the historical POST
// reached GitHub. Timeline events and reviews cannot prove delivery identity.
type ReviewRequestObservation struct {
	Intent    Intent           `json:"intent"`
	PR        PREvidence       `json:"pr"`
	Requested map[string]any   `json:"requested"`
	Timeline  []map[string]any `json:"timeline"`
}

func (o *ReviewRequestObservation) UnmarshalJSON(b []byte) error {
	type observation ReviewRequestObservation
	var decoded observation
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	*o = ReviewRequestObservation(decoded)
	return nil
}

func (o ReviewRequestObservation) Fingerprint() string { return digest(o) }

type ReviewRequestInspector interface {
	InspectReviewRequest(context.Context, Intent) (ReviewRequestObservation, error)
}

type ReviewRequestDisposition struct {
	Decision    string   `json:"decision"`
	Reason      string   `json:"reason"`
	Observation string   `json:"observation"`
	Sources     []string `json:"sources"`
}

type retainedReviewRequestDisposition struct {
	Operation       string                   `json:"operation"`
	Claim           string                   `json:"claim"`
	Disposition     ReviewRequestDisposition `json:"disposition"`
	Observation     ReviewRequestObservation `json:"observation"`
	HistoricalState string                   `json:"historical_state"`
	Delivery        string                   `json:"delivery"`
}

func (g *GitHub) InspectReviewRequest(ctx context.Context, i Intent) (ReviewRequestObservation, error) {
	if i.Kind != "request_review" {
		return ReviewRequestObservation{}, fmt.Errorf("review request required")
	}
	o := ReviewRequestObservation{Intent: i}
	var err error
	if o.PR, err = g.Capture(ctx, i.PR); err != nil {
		return o, err
	}
	if err = g.api(ctx, i.PR, "GET", prPath(i.PR)+"/requested_reviewers", nil, &o.Requested); err != nil {
		return o, err
	}
	o.Timeline, err = g.pages(ctx, i.PR, "repos/"+i.PR.Owner+"/"+i.PR.Name+"/issues/"+fmt.Sprint(i.PR.Number)+"/timeline?per_page=100")
	return o, err
}

func (r *Runtime) inspectReviewRequest(ctx context.Context, i Intent) (ReviewRequestObservation, error) {
	provider, ok := r.provider().(ReviewRequestInspector)
	if !ok {
		return ReviewRequestObservation{}, fmt.Errorf("provider cannot inspect uncertain review requests")
	}
	o, err := provider.InspectReviewRequest(ctx, i)
	if err != nil {
		return o, err
	}
	if o.Intent.Fingerprint() != i.Fingerprint() || o.PR.ViewerID <= 0 || o.PR.ViewerID != i.ActorID || !validSHA(o.PR.Metadata.Head.SHA) || o.PR.Metadata.Number != i.PR.Number || o.Requested == nil || o.Timeline == nil || o.PR.Reviews == nil {
		return o, fmt.Errorf("complete current PR, actor, requested set, timeline and reviews required")
	}
	return o, nil
}

func (r *Runtime) reviewRequestInspect(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	l, err := r.ledger()
	if err != nil {
		return out, err
	}
	defer l.Close()
	i, err := l.Intent(ctx, p.LogicalRun, in.Intent)
	if err != nil {
		return out, err
	}
	var state string
	if err = l.db.QueryRowContext(ctx, "SELECT state FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state); err != nil {
		return out, err
	}
	if i.Kind != "request_review" || !contains([]string{"dispatched", "unknown", "disposed"}, state) {
		if in.Kind == "uncertain" {
			return out, fmt.Errorf("recovery requires the original uncertain review-request operation")
		}
		out.Settled = true
		return out, nil
	}
	o, err := r.inspectReviewRequest(ctx, i)
	if err != nil {
		return out, err
	}
	out.Report, err = artifact(r.Root, o)
	if err == nil {
		_, err = l.db.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, i.ID, "review_request_inspect", out.Report)
	}
	return out, err
}

func latestReviewRequestInspection(ctx context.Context, l *Ledger, run, id string) (string, ReviewRequestObservation, error) {
	var path string
	var o ReviewRequestObservation
	err := l.db.QueryRowContext(ctx, "SELECT reference FROM progress WHERE run=? AND source=? AND kind='review_request_inspect' ORDER BY id DESC LIMIT 1", run, id).Scan(&path)
	if err == nil {
		err = readJSON(path, &o)
	}
	return path, o, err
}

// YAML calls this after the same bounded audience leaf explicitly reassesses
// current code/discussion. It records an obligation disposition, not a receipt.
func (r *Runtime) reviewRequestDispose(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
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
	i, err := l.Intent(ctx, p.LogicalRun, in.Intent)
	if err != nil {
		return out, err
	}
	if i.Kind != "request_review" {
		if in.Kind == "no_resend" {
			return out, fmt.Errorf("review-request recovery cannot dispose another kind of operation")
		}
		return out, nil
	}
	c, err := getClaim(ctx, l.db, p.LogicalRun, in.Claim)
	if err != nil {
		return out, err
	}
	if c.State != "complete" || c.Occurrence != "audience" || c.Payload != i.ID {
		return out, fmt.Errorf("accepted exact review-request reassessment required")
	}
	report, err := r.effectiveReport(ctx, c)
	if err != nil {
		return out, err
	}
	if report.ReviewRequestDisposition == nil {
		if in.Kind == "no_resend" {
			return out, fmt.Errorf("review obligation remains incomplete: explicit no_resend reassessment required")
		}
		return out, nil
	}
	d := *report.ReviewRequestDisposition
	if d.Decision != "no_resend" || !validText(d.Reason) || len(d.Sources) == 0 {
		return out, fmt.Errorf("explicit sourced no_resend reassessment required")
	}
	path, observed, err := latestReviewRequestInspection(ctx, l, i.Run, i.ID)
	if err != nil {
		return out, err
	}
	if !contains(d.Sources, path) || d.Observation != observed.Fingerprint() {
		return out, fmt.Errorf("no_resend must cite its exact inspected observation")
	}
	var assignment map[string]json.RawMessage
	if err = readJSON(c.Assignment, &assignment); err != nil {
		return out, err
	}
	var inspection struct {
		Path        string                   `json:"path"`
		Observation ReviewRequestObservation `json:"observation"`
	}
	if err = json.Unmarshal(assignment["review_request_inspection"], &inspection); err != nil {
		return out, err
	}
	if inspection.Path != path || inspection.Observation.Fingerprint() != d.Observation {
		return out, fmt.Errorf("reassessment did not consume this exact inspection")
	}
	current, err := r.inspectReviewRequest(ctx, i)
	if err != nil {
		return out, err
	}
	if current.Fingerprint() != d.Observation {
		return out, fmt.Errorf("review request context changed; inspect and reassess again")
	}
	for _, source := range d.Sources {
		if err = r.validateSource(ctx, p, source); err != nil {
			return out, err
		}
	}
	var state string
	if err = l.db.QueryRowContext(ctx, "SELECT state FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state); err != nil {
		return out, err
	}
	historical := state
	if state == "disposed" {
		var oldPath string
		if err = l.db.QueryRowContext(ctx, "SELECT artifact FROM review_request_dispositions WHERE run=? AND id=?", i.Run, i.ID).Scan(&oldPath); err != nil {
			return out, err
		}
		var old retainedReviewRequestDisposition
		if err = readJSON(oldPath, &old); err != nil {
			return out, err
		}
		historical = old.HistoricalState
	}
	if !contains([]string{"dispatched", "unknown", "disposed"}, state) {
		return out, fmt.Errorf("only uncertain dispatched review requests can be disposed")
	}
	out.Report, err = artifact(r.Root, retainedReviewRequestDisposition{Operation: ac.OperationKey, Claim: c.ID, Disposition: d, Observation: observed, HistoricalState: historical, Delivery: "unknown"})
	if err != nil {
		return out, err
	}
	err = l.transaction(ctx, func(tx sqlRW) error {
		result, err := tx.ExecContext(ctx, "UPDATE effects SET state='disposed' WHERE run=? AND id=? AND state=?", i.Run, i.ID, state)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return fmt.Errorf("review request state changed during disposition")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO review_request_dispositions(run,id,artifact) VALUES(?,?,?) ON CONFLICT(run,id) DO UPDATE SET artifact=excluded.artifact", i.Run, i.ID, out.Report); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", i.Run, i.ID, "review_request_disposed", out.Report)
		return err
	})
	out.Settled = err == nil
	return out, err
}

func (r *Runtime) verifyReviewRequestDisposition(ctx context.Context, l *Ledger, i Intent) error {
	var path string
	if err := l.db.QueryRowContext(ctx, "SELECT artifact FROM review_request_dispositions WHERE run=? AND id=?", i.Run, i.ID).Scan(&path); err != nil {
		return err
	}
	var retained retainedReviewRequestDisposition
	if err := readJSON(path, &retained); err != nil {
		return err
	}
	current, err := r.inspectReviewRequest(ctx, i)
	if err != nil {
		return err
	}
	if retained.Disposition.Decision != "no_resend" || retained.Delivery != "unknown" || retained.Disposition.Observation != current.Fingerprint() {
		return fmt.Errorf("retained no_resend context changed; fresh inspection and reassessment required")
	}
	return nil
}

func (r *Runtime) verifyReviewRequestDispositions(ctx context.Context, p Plan) error {
	l, err := r.ledger()
	if err != nil {
		return err
	}
	defer l.Close()
	rows, err := l.db.QueryContext(ctx, "SELECT intent FROM effects WHERE run=? AND state='disposed'", p.LogicalRun)
	if err != nil {
		return err
	}
	var intents []Intent
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var i Intent
		if err = json.Unmarshal([]byte(raw), &i); err != nil {
			break
		}
		intents = append(intents, i)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, i := range intents {
		if err = r.verifyReviewRequestDisposition(ctx, l, i); err != nil {
			return err
		}
	}
	return nil
}

// Keep inspection absence distinct from a malformed or inaccessible artifact.
func missingReviewRequestInspection(err error) bool { return errors.Is(err, sql.ErrNoRows) }
