package prfix

import (
	"context"
)

// Inspection candidates remain obligations when the configured reviewer set
// changes. Citation of an incomplete native output never establishes coverage.
func (r *Runtime) fixCandidates(ctx context.Context, p Plan) ([]Candidate, error) {
	reports, e := r.reports(ctx, p)
	if e != nil {
		return nil, e
	}
	result := []Candidate{}
	seen := map[string]bool{}
	for _, rr := range reports {
		if !rr.Occurrence.Review && rr.Occurrence.Kind != "inspection" && rr.Occurrence.Kind != "verification" {
			continue
		}
		for _, f := range rr.Report.Findings {
			if f.Disposition == "no_reply" || seen[f.ID] {
				continue
			}
			result = append(result, Candidate{f, rr.Reference})
			seen[f.ID] = true
		}
	}
	return result, nil
}

type retainedObligation struct {
	Obligation ReplyObligation `json:"obligation"`
	Original   string          `json:"original"`
	claim      string
}

func (r *Runtime) replyObligations(ctx context.Context, p Plan) ([]retainedObligation, error) {
	reports, e := r.reports(ctx, p)
	if e != nil {
		return nil, e
	}
	byID := map[string]retainedObligation{}
	for _, rr := range reports {
		for _, obligation := range rr.Report.ReplyObligations {
			old, ok := byID[obligation.ID]
			if ok && rr.Claim.State != "complete" {
				// Retain newly discovered requests from incomplete outputs, but
				// only accepted evidence can revise an existing obligation.
				continue
			}
			if ok && old.claim == rr.Claim.ID {
				continue
			}
			byID[obligation.ID] = retainedObligation{obligation, rr.Reference, rr.Claim.ID}
		}
	}
	result := []retainedObligation{}
	for _, id := range sortedKeys(byID) {
		result = append(result, byID[id])
	}
	return result, nil
}
func (r *Runtime) replyCoverage(ctx context.Context, p Plan, s Snapshot, reports []retainedReport) ([]string, error) {
	obligations, e := r.replyObligations(ctx, p)
	if e != nil {
		return nil, e
	}
	if len(obligations) == 0 {
		return nil, nil
	}
	responses := map[string]ReplyResponse{}
	for _, rr := range reports {
		if rr.Claim.State != "complete" || rr.Occurrence.Kind != "reply" {
			continue
		}
		for _, response := range rr.Report.Responses {
			responses[response.ID] = response
		}
	}
	l, e := r.ledger()
	if e != nil {
		return nil, e
	}
	defer l.Close()
	ev, e := readPREvidence(s.Evidence)
	if e != nil {
		return nil, e
	}
	blockers := []string{}
	for _, item := range obligations {
		obligation := item.Obligation
		response, ok := responses[obligation.ID]
		if !ok || !contains(response.Sources, item.Original) {
			blockers = append(blockers, "feedback "+obligation.ID+": sourced reply/no-reply disposition required")
			continue
		}
		if response.Outcome == "no_reply" {
			if obligation.Required {
				auth, ok := p.Authorizations["no-reply:"+obligation.ID]
				if !ok || response.Acceptance == nil || auth != *response.Acceptance {
					blockers = append(blockers, "feedback "+obligation.ID+": explicit no-reply authority required")
				}
			}
			continue
		}
		intent, e := l.Intent(ctx, p.LogicalRun, response.Operation)
		if e != nil {
			blockers = append(blockers, "feedback "+obligation.ID+": prepared reply absent")
			continue
		}
		var state, path string
		if e = l.db.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", intent.Run, intent.ID).Scan(&state, &path); e != nil {
			return nil, e
		}
		if state != "completed" || !contains([]string{"reply", "reply_inline"}, intent.Kind) {
			blockers = append(blockers, "feedback "+obligation.ID+": reply receipt incomplete")
			continue
		}
		if obligation.CommentID != "" && (intent.Kind != "reply_inline" || stringValue(intent.Payload, "comment_id") != obligation.CommentID) {
			blockers = append(blockers, "feedback "+obligation.ID+": reply targets a different comment")
			continue
		}
		var receipt Receipt
		if e = readJSON(path, &receipt); e != nil {
			return nil, e
		}
		if settled, err := r.verifyReplySettlement(ctx, l, intent); settled || err != nil {
			if err != nil {
				blockers = append(blockers, "feedback "+obligation.ID+": "+err.Error())
			}
			continue
		}
		comments := ev.Comments
		if intent.Kind == "reply_inline" {
			comments = ev.Inline
		}
		found := false
		for _, comment := range comments {
			if githubID(comment, "id") != "" && githubID(comment, "id") == githubID(receipt.Result, "id") && stringValue(comment, "body") == stringValue(intent.Payload, "body") {
				if intent.Kind != "reply_inline" || githubID(comment, "in_reply_to_id") == stringValue(intent.Payload, "comment_id") {
					found = true
				}
			}
		}
		if !found {
			blockers = append(blockers, "feedback "+obligation.ID+": actual published answer is absent")
		}
	}
	return blockers, nil
}
