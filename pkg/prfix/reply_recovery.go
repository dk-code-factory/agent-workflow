package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A replacement is a new, separately authorized effect. Its reassessment is
// bound to an immutable observation; the historical intent/receipt stay intact.
type ReplyReplacement struct {
	Run           string        `json:"run"`
	ID            string        `json:"id"`
	Inspection    string        `json:"inspection"`
	Observation   string        `json:"observation"`
	Decision      string        `json:"decision"`
	Reason        string        `json:"reason"`
	Sources       []string      `json:"sources"`
	Authorization Authorization `json:"authorization"`
}

type ReplyObservation struct {
	Intent   Intent         `json:"intent"`
	Receipt  Receipt        `json:"receipt"`
	PR       PREvidence     `json:"pr"`
	Original map[string]any `json:"original"`
	Status   string         `json:"status"`
}

func (o *ReplyObservation) UnmarshalJSON(b []byte) error {
	type observation ReplyObservation
	var decoded observation
	if err := decodeGitHubJSON(b, &decoded); err != nil {
		return err
	}
	*o = ReplyObservation(decoded)
	return nil
}
func (o ReplyObservation) Fingerprint() string { return digest(o) }

type ReplyInspector interface {
	InspectReply(context.Context, Intent, Receipt) (ReplyObservation, error)
}

func (g *GitHub) InspectReply(ctx context.Context, i Intent, receipt Receipt) (ReplyObservation, error) {
	o := ReplyObservation{Intent: i, Receipt: receipt}
	if !contains([]string{"reply", "reply_inline"}, i.Kind) {
		return o, fmt.Errorf("historical reply required")
	}
	var err error
	if o.PR, err = g.Capture(ctx, i.PR); err != nil {
		return o, err
	}
	id := githubID(receipt.Result, "id")
	if !numeric(id) {
		return o, fmt.Errorf("historical numeric reply ID required")
	}
	path := "repos/" + i.PR.Owner + "/" + i.PR.Name + "/issues/comments/" + id
	if i.Kind == "reply_inline" {
		path = "repos/" + i.PR.Owner + "/" + i.PR.Name + "/pulls/comments/" + id
	}
	raw, commandErr := runCommand(ctx, g.Root, nil, "gh", "api", "--hostname", i.PR.Host, "--include", path)
	parts := strings.SplitN(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return o, fmt.Errorf("individual reply HTTP evidence required: %v", commandErr)
	}
	fields := strings.Fields(strings.SplitN(parts[0], "\n", 2)[0])
	if len(fields) >= 2 && fields[1] == "404" {
		o.Status = "unavailable"
		return o, nil
	}
	if commandErr != nil {
		return o, commandErr
	}
	if len(fields) < 2 || fields[1] != "200" {
		return o, fmt.Errorf("individual reply inaccessible")
	}
	if err = decodeGitHubJSON([]byte(parts[1]), &o.Original); err != nil {
		return o, err
	}
	o.Status = "edited"
	if stringValue(o.Original, "body") == stringValue(i.Payload, "body") {
		o.Status = "visible"
	}
	return o, nil
}

func (r *Runtime) inspectReply(ctx context.Context, i Intent, receipt Receipt) (ReplyObservation, error) {
	provider, ok := r.provider().(ReplyInspector)
	if !ok {
		return ReplyObservation{}, fmt.Errorf("provider cannot inspect historical replies")
	}
	o, err := provider.InspectReply(ctx, i, receipt)
	if err != nil {
		return o, err
	}
	if o.Intent.Fingerprint() != i.Fingerprint() || digest(o.Receipt) != digest(receipt) || o.PR.ViewerID <= 0 || o.PR.ViewerID != i.ActorID || o.PR.Metadata.Number != i.PR.Number || o.PR.Metadata.URL != i.PR.URL || !validSHA(o.PR.Metadata.Head.SHA) || o.PR.Comments == nil || o.PR.Inline == nil || len(i.Sources) == 0 {
		return o, fmt.Errorf("complete exact historical reply, current PR/source and actor evidence required")
	}
	comments := o.PR.Comments
	if i.Kind == "reply_inline" {
		comments = o.PR.Inline
	}
	id := githubID(receipt.Result, "id")
	listed := false
	for _, c := range comments {
		if githubID(c, "id") == id {
			listed = true
			if digest(c) != digest(o.Original) {
				return o, fmt.Errorf("reply list/individual observation inconsistent")
			}
		}
	}
	if i.Kind == "reply_inline" {
		found := false
		for _, c := range o.PR.Inline {
			if githubID(c, "id") == stringValue(i.Payload, "comment_id") {
				found = true
			}
		}
		if !found {
			return o, fmt.Errorf("original inline routing source inaccessible")
		}
	}
	switch o.Status {
	case "unavailable":
		if listed || o.Original != nil {
			return o, fmt.Errorf("unavailable reply conflicts with current list/individual evidence")
		}
	case "edited", "visible":
		user, _ := o.Original["user"].(map[string]any)
		if !listed || githubID(o.Original, "id") != id || githubID(user, "id") != strconv.FormatInt(i.ActorID, 10) || (i.Kind == "reply_inline" && githubID(o.Original, "in_reply_to_id") != stringValue(i.Payload, "comment_id")) {
			return o, fmt.Errorf("exact original reply membership/actor/destination required")
		}
		if (o.Status == "visible") != (stringValue(o.Original, "body") == stringValue(i.Payload, "body")) {
			return o, fmt.Errorf("reply state inconsistent with body")
		}
	default:
		return o, fmt.Errorf("unknown reply observation")
	}
	return o, nil
}

func completedReply(ctx context.Context, l *Ledger, run, id string) (Intent, Receipt, error) {
	i, err := l.Intent(ctx, run, id)
	if err != nil {
		return i, Receipt{}, err
	}
	var state, path string
	err = l.db.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state, &path)
	if err != nil {
		return i, Receipt{}, err
	}
	if state != "completed" || !contains([]string{"reply", "reply_inline"}, i.Kind) {
		return i, Receipt{}, fmt.Errorf("only a previously verified reply can be replaced; reconcile uncertain writes first")
	}
	var receipt Receipt
	err = readJSON(path, &receipt)
	if err == nil && (receipt.ID != i.ID || receipt.Kind != i.Kind || receipt.Head != i.Head || receipt.PR != i.PR.URL || !numeric(githubID(receipt.Result, "id"))) {
		err = fmt.Errorf("exact historical receipt required")
	}
	return i, receipt, err
}

func (r *Runtime) replyInspect(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	l, err := r.ledger()
	if err != nil {
		return out, err
	}
	defer l.Close()
	i, receipt, err := completedReply(ctx, l, p.LogicalRun, in.Intent)
	if err != nil {
		return out, err
	}
	o, err := r.inspectReply(ctx, i, receipt)
	if err != nil {
		return out, err
	}
	out.Report, err = artifact(r.Root, o)
	if err == nil {
		_, err = l.db.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", i.Run, i.ID, "reply_inspect", out.Report)
	}
	return out, err
}

func latestReplyInspection(ctx context.Context, l *Ledger, run, id string) (string, ReplyObservation, error) {
	var path string
	var o ReplyObservation
	err := l.db.QueryRowContext(ctx, "SELECT reference FROM progress WHERE run=? AND source=? AND kind='reply_inspect' ORDER BY id DESC LIMIT 1", run, id).Scan(&path)
	if err == nil {
		err = readJSON(path, &o)
	}
	return path, o, err
}

func replacementAuthority(i Intent) string { return "reply-replacement:" + i.Run + ":" + i.ID }

func validateReplacementLink(ctx context.Context, tx sqlRW, p Plan, i Intent) error {
	d := i.Replaces
	if !contains([]string{"reply", "reply_inline"}, i.Kind) || d == nil || d.Decision != "replacement_required" || !validText(d.Reason) || !validText(d.Inspection) || len(d.Sources) == 0 || !contains(d.Sources, d.Inspection) {
		return fmt.Errorf("explicit sourced replacement_required disposition required")
	}
	a, ok := p.Authorizations["reply-replacement:"+d.Run+":"+d.ID]
	if !ok || a != d.Authorization {
		return fmt.Errorf("separate exact replacement authorization required")
	}
	if err := a.Validate(); err != nil {
		return err
	}
	var alias int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM effect_aliases WHERE run=? AND id=?", i.Run, i.ID).Scan(&alias); err != nil {
		return err
	}
	if alias != 0 {
		return fmt.Errorf("replacement requires a fresh operation ID; retained alias remains intact")
	}
	var raw, state string
	if err := tx.QueryRowContext(ctx, "SELECT intent,state FROM effects WHERE run=? AND id=?", d.Run, d.ID).Scan(&raw, &state); err != nil {
		return err
	}
	var old Intent
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		return err
	}
	if state != "completed" || old.PR != i.PR || old.Kind != i.Kind || old.ActorID != i.ActorID || old.ActorID <= 0 || stringValue(old.Payload, "comment_id") != stringValue(i.Payload, "comment_id") {
		return fmt.Errorf("replacement must retain verified original PR/actor/source/destination")
	}
	var run, id string
	err := tx.QueryRowContext(ctx, "SELECT replacement_run,replacement_id FROM reply_replacements WHERE original_run=? AND original_id=? AND observation=?", d.Run, d.ID, d.Observation).Scan(&run, &id)
	if err == nil && (run != i.Run || id != i.ID) {
		return fmt.Errorf("this disposition already links replacement %s/%s; recover it", run, id)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func linkReplyReplacement(ctx context.Context, tx sqlRW, i Intent) error {
	d := i.Replaces
	if _, err := tx.ExecContext(ctx, "UPDATE effects SET state='superseded' WHERE state='prepared' AND (run,id) IN (SELECT replacement_run,replacement_id FROM reply_replacements WHERE original_run=? AND original_id=? AND observation<>?)", d.Run, d.ID, d.Observation); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO reply_replacements VALUES(?,?,?,?,?)", d.Run, d.ID, d.Observation, i.Run, i.ID)
	return err
}

func (r *Runtime) verifyReplyReplacement(ctx context.Context, p Plan, i Intent) error {
	i.Run = p.LogicalRun
	i.PR = p.Identity
	l, err := r.ledger()
	if err != nil {
		return err
	}
	defer l.Close()
	if err = validateReplacementLink(ctx, l.db, p, i); err != nil {
		return err
	}
	d := i.Replaces
	path, _, err := latestReplyInspection(ctx, l, d.Run, d.ID)
	if err != nil {
		return err
	}
	if path != d.Inspection {
		return fmt.Errorf("reply inspection was superseded; reassess current observation")
	}
	old, receipt, err := completedReply(ctx, l, d.Run, d.ID)
	if err != nil {
		return err
	}
	var observed ReplyObservation
	if err = readJSON(d.Inspection, &observed); err != nil {
		return err
	}
	if observed.Fingerprint() != d.Observation || observed.Intent.Fingerprint() != old.Fingerprint() {
		return fmt.Errorf("replacement observation does not match original")
	}
	for _, source := range old.Sources {
		if !contains(d.Sources, source) {
			return fmt.Errorf("replacement must retain exact original feedback provenance")
		}
	}
	current, err := r.inspectReply(ctx, old, receipt)
	if err != nil {
		return err
	}
	if current.Status == "visible" || current.Fingerprint() != d.Observation || i.Head != current.PR.Metadata.Head.SHA {
		return fmt.Errorf("reply recovery changed; inspect and reassess again")
	}
	for _, source := range append(append([]string{}, old.Sources...), d.Sources...) {
		if err = r.validateSource(ctx, p, source); err != nil {
			return err
		}
	}
	// A different uncertain linked answer cannot be bypassed with a new inspection.
	var pending int
	err = l.db.QueryRowContext(ctx, "SELECT count(*) FROM reply_replacements r JOIN effects e ON e.run=r.replacement_run AND e.id=r.replacement_id WHERE r.original_run=? AND r.original_id=? AND e.state IN ('dispatched','unknown','completed') AND NOT(e.run=? AND e.id=?)", d.Run, d.ID, i.Run, i.ID).Scan(&pending)
	if err == nil && pending > 0 {
		return fmt.Errorf("linked replacement requires its own reconciliation/recovery")
	}
	return err
}

func (r *Runtime) acceptReplyReplacement(ctx context.Context, p Plan, c claimRecord, i Intent) error {
	var assignment map[string]json.RawMessage
	if err := readJSON(c.Assignment, &assignment); err != nil {
		return err
	}
	var inspected struct {
		Path        string           `json:"path"`
		Observation ReplyObservation `json:"observation"`
	}
	if err := json.Unmarshal(assignment["reply_inspection"], &inspected); err != nil {
		return fmt.Errorf("replacement reassessment must consume original inspection: %w", err)
	}
	if c.Occurrence != "reply_recovery" || inspected.Path != i.Replaces.Inspection || inspected.Observation.Fingerprint() != i.Replaces.Observation {
		return fmt.Errorf("accepted exact reply recovery reassessment required")
	}
	return r.verifyReplyReplacement(ctx, p, i)
}
