package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"strings"
	"time"
)

func ciPollInterval(opts map[string]any, override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if n, ok := opts["poll_interval_seconds"].(float64); ok && n > 0 {
		return time.Duration(int64(n)) * time.Second
	}
	return 30 * time.Second
}

type retainedReport struct {
	Claim      claimRecord
	Occurrence Occurrence
	Report     Report
	Reference  string
}

func (r *Runtime) reports(ctx context.Context, p Plan) ([]retainedReport, error) {
	l, e := r.ledger()
	if e != nil {
		return nil, e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT id FROM claims WHERE run=? AND report_message<>'' ORDER BY rowid", p.LogicalRun)
	if e != nil {
		l.Close()
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			l.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		l.Close()
		return nil, e
	}
	claims := []claimRecord{}
	for _, id := range ids {
		c, e := getClaim(ctx, l.db, p.LogicalRun, id)
		if e != nil {
			l.Close()
			return nil, e
		}
		claims = append(claims, c)
	}
	l.Close()
	results := []retainedReport{}
	originIDs := map[string]bool{}
	for _, c := range claims {
		var assigned struct {
			Occurrence Occurrence `json:"occurrence"`
		}
		raw, e := os.ReadFile(c.Assignment)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(raw, &assigned); e != nil {
			return nil, e
		}
		o := assigned.Occurrence
		if o.ID != c.Occurrence {
			return nil, fmt.Errorf("original occurrence identity changed")
		}

		messages, e := r.claimMessages(ctx, c)
		if e != nil {
			return nil, e
		}
		submissionIDs := map[string]bool{}
		l, e := r.ledger()
		if e != nil {
			return nil, e
		}
		rows, e := l.db.QueryContext(ctx, "SELECT message FROM submissions WHERE run=? AND claim=? ORDER BY round", p.LogicalRun, c.ID)
		if e != nil {
			l.Close()
			return nil, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				l.Close()
				return nil, e
			}
			submissionIDs[id] = true
		}
		e = rows.Err()
		rows.Close()
		l.Close()
		if e != nil {
			return nil, e
		}
		for _, m := range messages {
			if submissionIDs[m.ID] {
				report, e := parseReport(m)
				if e != nil {
					continue
				}
				if m.ID != c.ReportMessage {
					for _, prior := range results {
						if prior.Claim.ID == c.ID {
							if report.Cleanup == nil {
								report.Cleanup = prior.Report.Cleanup
							}
							if report.CI == nil {
								report.CI = prior.Report.CI
							}
							if report.PolicyPrompt == "" {
								report.PolicyPrompt = prior.Report.PolicyPrompt
							}
						}
					}
				}
				for i := range report.Findings {
					f := &report.Findings[i]
					if o.Review || o.Kind == "inspection" || o.Kind == "verification" && !originIDs[f.ID] {
						f.ID = "finding_" + digest([]string{c.ID, f.ID})
					}
					if o.Review || o.Kind == "inspection" || o.Kind == "verification" {
						originIDs[f.ID] = true
					}
				}
				results = append(results, retainedReport{c, o, report, messageRef(c, m)})
			}
		}
	}
	return results, nil
}
func (r *Runtime) coverage(ctx context.Context, p Plan, s Snapshot) (bool, []string, error) {
	return r.coverageExcluding(ctx, p, s, "")
}
func (r *Runtime) coverageExcluding(ctx context.Context, p Plan, s Snapshot, ignoreEffect string) (bool, []string, error) {
	reports, e := r.reports(ctx, p)
	if e != nil {
		return false, nil, e
	}
	latest := map[string]retainedReport{}
	for _, rr := range reports {
		latest[rr.Occurrence.ID] = rr
	}
	blockers := []string{}
	verificationCandidates, e := r.candidates(ctx, p)
	if e != nil {
		return false, nil, e
	}
	if len(verificationCandidates) > 0 {
		rr, ok := latest["finding_verification"]
		if !ok || rr.Claim.State != "complete" {
			blockers = append(blockers, "independent finding verification incomplete")
		} else {
			refs, e := r.reportSources(ctx, p, "verification")
			if e != nil {
				return false, nil, e
			}
			var assignment struct {
				References []string `json:"original_result_references"`
			}
			b, e := os.ReadFile(rr.Claim.Assignment)
			if e != nil {
				return false, nil, e
			}
			if e = json.Unmarshal(b, &assignment); e != nil {
				return false, nil, e
			}
			if digest(refs) != digest(assignment.References) {
				blockers = append(blockers, "independent finding verification evidence stale")
			}
		}
	}
	hookCycle := ""
	l, err := r.ledger()
	if err != nil {
		return false, nil, err
	}
	err = l.db.QueryRowContext(ctx, "SELECT operation FROM reservations WHERE run=? AND kind='review' ORDER BY rowid DESC LIMIT 1", p.LogicalRun).Scan(&hookCycle)
	l.Close()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, nil, err
	}
	hooks := map[string]bool{}
	selected := map[string]bool{}
	for _, phase := range []string{"tasks", "finalizers", "before_cycle", "after_cycle"} {
		for _, g := range p.Groups[phase] {
			for _, id := range g {
				if phase == "before_cycle" || phase == "after_cycle" {
					hooks[id] = true
					if hookCycle == "" {
						continue
					}
				}
				selected[id] = true
			}
		}
	}
	for _, o := range p.Occurrences {
		if !selected[o.ID] || !o.Required {
			continue
		}
		rr, ok := latest[o.ID]
		if !ok || rr.Claim.State != "complete" {
			blockers = append(blockers, o.ID+": missing accepted original execution/checklist")
			continue
		}
		var initial Snapshot
		if e := readJSON(rr.Claim.Snapshot, &initial); e != nil {
			return false, nil, e
		}
		fresh := occurrenceFingerprint(initial, o) == occurrenceFingerprint(s, o)
		if hooks[o.ID] {
			fresh = fresh && rr.Claim.Cycle == hookCycle
		}
		if o.Kind == "cleanup" {
			fresh = cleanupContextCurrent(initial, s, o, rr.Report.Cleanup)
		}
		if !fresh && o.Kind == "commit_policy" {
			fresh, e = r.policyConsumedByCleanup(rr, reports, s)
			if e != nil {
				return false, nil, e
			}
		}
		if !fresh {
			blockers = append(blockers, o.ID+": governing evidence stale")
		}
		if o.Kind == "cleanup" && (rr.Report.Cleanup == nil || rr.Report.Cleanup.Outcome != "no_op" && rr.Report.Cleanup.Outcome != "published" || rr.Report.Cleanup.FinalHead != s.Head) {
			blockers = append(blockers, o.ID+": cleanup/publication not complete")
		}
	}
	// Findings remain obligations after their original review becomes stale. A
	// disposition must cite the actual original result, with exact user authority
	// for acceptance. A later empty review never erases an unresolved candidate.
	candidates := map[string]retainedReport{}
	dispositions := map[string]Finding{}
	for _, rr := range reports {
		for _, f := range rr.Report.Findings {
			if rr.Occurrence.Review || rr.Occurrence.Kind == "verification" || rr.Occurrence.Kind == "inspection" && f.Disposition != "no_reply" {
				if _, ok := candidates[f.ID]; !ok {
					candidates[f.ID] = rr
				}
			}
			if rr.Claim.State == "complete" && (rr.Occurrence.Kind == "fix" || rr.Occurrence.Kind == "ci_repair" || rr.Occurrence.Kind == "verification" && contains([]string{"dismissed", "duplicate"}, f.Disposition)) {
				if f.Disposition != "" {
					dispositions[f.ID] = f
				}
			}
		}
	}
	for id, original := range candidates {
		f, ok := dispositions[id]
		if !ok || !contains(f.Sources, original.Reference) || !contains([]string{"fixed", "not_actionable", "accepted", "deferred", "dismissed", "duplicate"}, f.Disposition) || !validText(f.Reason) {
			blockers = append(blockers, "finding "+id+": sourced disposition required")
			continue
		}
		if f.Disposition == "deferred" || f.Disposition == "accepted" {
			a, ok := p.Authorizations["accept:"+id]
			if !ok || f.Acceptance == nil || a != *f.Acceptance {
				blockers = append(blockers, "finding "+id+": exact user acceptance required")
			}
		}
	}
	replyBlockers, e := r.replyCoverage(ctx, p, s, reports)
	if e != nil {
		return false, nil, e
	}
	blockers = append(blockers, replyBlockers...)

	l, e = r.ledger()
	if e != nil {
		return false, nil, e
	}
	defer l.Close()
	if e = r.verifyReviewRequestDispositions(ctx, p); e != nil {
		return false, nil, e
	}
	var pending int
	if e = l.db.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM claims WHERE run=? AND state IN ('claimed','recheck','recover_required'))+(SELECT count(*) FROM effects WHERE run=? AND state NOT IN ('completed','superseded','disposed') AND id<>? AND json_extract(intent,'$.kind')<>'merge')", p.LogicalRun, p.LogicalRun, ignoreEffect).Scan(&pending); e != nil {
		return false, nil, e
	}
	if pending > 0 {
		blockers = append(blockers, fmt.Sprintf("%d unfinished claims or publication effects", pending))
	}
	return len(blockers) == 0, blockers, nil
}
func (r *Runtime) latestCI(ctx context.Context, p Plan, head string) (string, error) {
	l, e := r.ledger()
	if e != nil {
		return "", e
	}
	defer l.Close()
	var path string
	e = l.db.QueryRowContext(ctx, "SELECT reference FROM progress WHERE run=? AND kind=? ORDER BY id DESC LIMIT 1", p.LogicalRun, "ci:"+head).Scan(&path)
	return path, e
}
func (r *Runtime) ci(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	s, e := r.live(ctx, p)
	if e != nil {
		return out, e
	}
	if s.Head != s.LocalHead {
		out.Deferred = true
		return out, nil
	}
	current, _, e := r.reviewCurrent(ctx, p, s)
	if e != nil {
		return out, e
	}
	if !current {
		out.Deferred = true
		return out, nil
	}
	o, e := p.Occurrence(in.Occurrence)
	if e != nil {
		return out, e
	}
	opts := o.Call.Invocation.Options
	if o.Call.Configuration != nil {
		opts = o.Call.Configuration.Options
	}
	if e := validateCIOptions(opts); e != nil {
		return out, e
	}
	timeout := ciSeconds(opts, "timeout_seconds", 1800*time.Second)
	l, e := r.ledger()
	if e != nil {
		return out, e
	}
	deadline, e := l.Deadline(ctx, p.LogicalRun, s.Head, r.now(), timeout)
	if e != nil {
		l.Close()
		return out, e
	}
	started, e := l.Started(ctx, p.LogicalRun, s.Head)
	l.Close()
	if e != nil {
		return out, e
	}
	grace := ciSeconds(opts, "no_checks_grace_seconds", 60*time.Second)
	extension := ciSeconds(opts, "pending_extension_seconds", 1800*time.Second)
	maximum := 1
	if n, ok := opts["max_pending_extensions"].(float64); ok {
		maximum = int(n)
	}
	interval := ciPollInterval(opts, r.PollInterval)
	for {
		remaining := deadline.Sub(r.now())
		// Like the pinned source, permit one bounded current observation before
		// deciding that an elapsed wait window cannot continue. An already
		// completed check can still be observed after interruption; no polling
		// allowance is reset. In-flight reads always have a local bound.
		if remaining <= 0 {
			remaining = time.Minute
		}
		observeCtx, cancel := context.WithTimeout(ctx, min(time.Minute, remaining))
		obs, e := r.provider().ObserveCI(observeCtx, p.Identity, s.Head)
		cancel()
		if e != nil {
			return out, e
		}
		path, e := artifact(r.Root, obs)
		if e != nil {
			return out, e
		}
		l, e := r.ledger()
		if e != nil {
			return out, e
		}
		_, e = l.db.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, s.Evidence, "ci:"+s.Head, path)
		l.Close()
		if e != nil {
			return out, e
		}
		out.Report = path
		if obs.Success() {
			return out, nil
		}
		failed := false
		pending := len(obs.Checks) == 0
		for _, check := range obs.Checks {
			if check.Status != "completed" {
				pending = true
			} else if !contains([]string{"success", "neutral", "skipped"}, check.Conclusion) {
				failed = true
			}
		}
		if failed && !pending {
			out.Repair = true
			return out, nil
		}
		now := r.now()
		if len(obs.Checks) == 0 && !now.Before(started.Add(grace)) {
			return out, fmt.Errorf("no checks observed during retained CI grace period")
		}
		if !now.Before(deadline) {
			extended := false
			if pending && !failed && len(obs.Checks) > 0 {
				l, e := r.ledger()
				if e != nil {
					return out, e
				}
				deadline, extended, e = l.Extend(ctx, p.LogicalRun, s.Head, extension, maximum)
				l.Close()
				if e != nil {
					return out, e
				}
			}
			if !extended {
				return out, fmt.Errorf("exact-head CI observation deadline exceeded; pending or missing checks never pass")
			}
		}
		timer := time.NewTimer(min(interval, deadline.Sub(r.now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, ctx.Err()
		case <-timer.C:
		}
	}
}
func (r *Runtime) reviewCurrent(ctx context.Context, p Plan, s Snapshot) (bool, []string, error) {
	reports, e := r.reports(ctx, p)
	if e != nil {
		return false, nil, e
	}
	latest := map[string]retainedReport{}
	for _, rr := range reports {
		latest[rr.Occurrence.ID] = rr
	}
	missing := []string{}
	for _, o := range p.Occurrences {
		if !o.Review || !o.Required {
			continue
		}
		rr, ok := latest[o.ID]
		if !ok || rr.Claim.State != "complete" {
			missing = append(missing, o.ID)
			continue
		}
		var original Snapshot
		if e = readJSON(rr.Claim.Snapshot, &original); e != nil {
			return false, nil, e
		}
		if occurrenceFingerprint(original, o) != occurrenceFingerprint(s, o) {
			missing = append(missing, o.ID)
		}
	}
	return len(missing) == 0, missing, nil
}

// Optional failures may finish selected work, but only accepted current native
// reviews can supply readiness or merge coverage.
func completedReviewCount(reports []retainedReport, current Snapshot) (int, error) {
	latest := map[string]retainedReport{}
	for _, rr := range reports {
		if rr.Occurrence.Review {
			latest[rr.Occurrence.ID] = rr
		}
	}
	count := 0
	for _, rr := range latest {
		if rr.Claim.State != "complete" {
			continue
		}
		var initial Snapshot
		if err := readJSON(rr.Claim.Snapshot, &initial); err != nil {
			return 0, err
		}
		if occurrenceFingerprint(initial, rr.Occurrence) == occurrenceFingerprint(current, rr.Occurrence) {
			count++
		}
	}
	return count, nil
}

func (r *Runtime) completion(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return out, e
	}
	l.Close()
	if c.Occurrence != "completion" || c.State != "complete" {
		return out, fmt.Errorf("managed completion/checklist must be accepted, including for an empty plan")
	}
	s, e := r.live(ctx, p)
	if e != nil {
		return out, e
	}
	complete, blockers, e := r.coverage(ctx, p, s)
	if e != nil {
		return out, e
	}
	var initial Snapshot
	if e = readJSON(c.Snapshot, &initial); e != nil {
		return out, e
	}
	o, e := p.Occurrence("completion")
	if e != nil {
		return out, e
	}
	if occurrenceFingerprint(initial, o) != occurrenceFingerprint(s, o) {
		complete = false
		blockers = append(blockers, "managed completion evidence stale")
	}
	out.Complete = complete
	readiness := []string{}
	if s.LocalHead != s.Head {
		readiness = append(readiness, "local head is unpublished")
	}
	status, e := gitStatus(ctx, r.Root)
	if e != nil {
		return out, e
	}
	if status != "" {
		readiness = append(readiness, "worktree is not clean")
	}
	selectedReviews := 0
	for _, o := range p.Occurrences {
		if o.Review {
			selectedReviews++
		}
	}
	if selectedReviews == 0 {
		readiness = append(readiness, "review coverage was not selected")
	} else {
		reports, err := r.reports(ctx, p)
		if err != nil {
			return out, err
		}
		count, err := completedReviewCount(reports, s)
		if err != nil {
			return out, err
		}
		if count == 0 {
			readiness = append(readiness, "observed current completed review coverage unavailable")
		}
	}
	for _, duty := range p.Omitted {
		readiness = append(readiness, "omitted "+duty)
	}
	// The live gate does not secretly select CI when it was omitted. It only
	// accepts retained controller-observed exact-head evidence for selected CI.
	ciNeedsRefresh := false
	ciSelected := false
	for _, o := range p.Occurrences {
		ciSelected = ciSelected || o.Kind == "ci"
	}
	if ciSelected {
		path, e := r.latestCI(ctx, p, s.Head)
		if e != nil {
			readiness = append(readiness, "exact-head CI evidence missing")
			out.Complete = false
		} else {
			var ci CIObservation
			if e = readJSON(path, &ci); e != nil {
				return out, e
			}
			if ci.Head != s.Head || !ci.Success() {
				readiness = append(readiness, "exact-head CI has not passed")
				out.Complete = false
				ciNeedsRefresh = true
			}
		}
	}
	out.Ready = out.Complete && len(readiness) == 0
	l, e = r.ledger()
	if e != nil {
		return out, e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT occurrence,reason,engine,worker,message FROM claims WHERE run=? AND state='optional' AND rowid IN (SELECT max(rowid) FROM claims WHERE run=? GROUP BY occurrence) ORDER BY rowid", p.LogicalRun, p.LogicalRun)
	if e != nil {
		l.Close()
		return out, e
	}
	omissions := []string{}
	for rows.Next() {
		var occurrence, reason, engine, worker, message string
		if e = rows.Scan(&occurrence, &reason, &engine, &worker, &message); e != nil {
			break
		}
		if _, err := p.Occurrence(occurrence); err != nil {
			continue
		}
		omissions = append(omissions, occurrence+": "+reason+" (engine://"+engine+"/workers/"+worker+"/messages/"+message+")")
	}
	e = errors.Join(e, rows.Err(), rows.Close(), l.Close())
	if e != nil {
		return out, e
	}

	report := map[string]any{"logical_run": p.LogicalRun, "selected_workflow_complete": out.Complete, "pr_ready": out.Ready, "head": s.Head, "target": s.Target, "blockers": blockers, "readiness": readiness, "omitted_default_duties": p.Omitted, "optional_incomplete": omissions, "live_evidence": s.Evidence, "completion_claim": c.ID}
	out.Report, e = artifact(r.Root, report)
	if e != nil {
		return out, e
	}
	if !out.Complete {
		refreshable := ciNeedsRefresh
		for _, blocker := range blockers {
			refreshable = refreshable || strings.Contains(blocker, "stale")
		}
		if !refreshable {
			return out, fmt.Errorf("live completion blocked; private report %s", out.Report)
		}
	}
	return out, nil
}

type Candidate struct {
	Finding  Finding `json:"finding"`
	Original string  `json:"original"`
}

func (r *Runtime) candidates(ctx context.Context, p Plan) ([]Candidate, error) {
	s, e := r.live(ctx, p)
	if e != nil {
		return nil, e
	}
	reports, e := r.reports(ctx, p)
	if e != nil {
		return nil, e
	}

	latest := map[string]string{}
	for _, rr := range reports {
		if rr.Occurrence.Review {
			latest[rr.Occurrence.ID] = rr.Claim.ID
		}
	}
	result := []Candidate{}
	seen := map[string]bool{}
	for _, rr := range reports {
		if !rr.Occurrence.Review || latest[rr.Occurrence.ID] != rr.Claim.ID || rr.Claim.State != "complete" {
			continue
		}
		var original Snapshot
		if e = readJSON(rr.Claim.Snapshot, &original); e != nil {
			return nil, e
		}
		if occurrenceFingerprint(original, rr.Occurrence) != occurrenceFingerprint(s, rr.Occurrence) {
			continue
		}
		for _, f := range rr.Report.Findings {
			if !seen[f.ID] {
				result = append(result, Candidate{f, rr.Reference})
				seen[f.ID] = true
			}
		}
	}

	return result, nil
}

func (r *Runtime) close(ctx context.Context, p Plan, ac workflows.ActionContext) (ActionOutput, error) {
	out := output()
	if e := r.verifyReviewRequestDispositions(ctx, p); e != nil {
		return out, e
	}
	engine, e := r.origin(ctx, ac)
	if e != nil {
		return out, e
	}
	l, e := r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	rows, e := l.db.QueryContext(ctx, "SELECT receipt FROM effects WHERE run=? AND state='completed' ORDER BY rowid", p.LogicalRun)
	if e != nil {
		return out, e
	}
	receipts := []string{}
	for rows.Next() {
		var path string
		if e = rows.Scan(&path); e != nil {
			rows.Close()
			return out, e
		}
		receipts = append(receipts, path)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	out.Report, e = artifact(r.Root, map[string]any{"logical_run": p.LogicalRun, "engine_run": engine, "publication_receipts": receipts, "private": true})
	if e != nil {
		return out, e
	}
	e = l.transaction(ctx, func(tx sqlRW) error {
		var pending int
		if e := tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM claims WHERE run=? AND state IN ('claimed','recheck','recover_required'))+(SELECT count(*) FROM effects WHERE run=? AND state NOT IN ('completed','superseded','disposed'))", p.LogicalRun, p.LogicalRun).Scan(&pending); e != nil {
			return e
		}
		if pending > 0 {
			return fmt.Errorf("close blocked by %d unsettled claims/effects", pending)
		}
		if _, e := tx.ExecContext(ctx, "UPDATE admissions SET state='complete' WHERE run=? AND request IN (SELECT request FROM executions WHERE run=? AND engine=?)", p.LogicalRun, p.LogicalRun, engine); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE logical_runs SET status='complete' WHERE id=?", p.LogicalRun)
		return e
	})
	out.Settled = e == nil
	return out, e
}

func mergeReport(base, next Report) Report {
	if next.ReviewRequestDisposition == nil {
		next.ReviewRequestDisposition = base.ReviewRequestDisposition
	}
	if next.PolicyPrompt == "" {
		next.PolicyPrompt = base.PolicyPrompt
	}
	next.Responses = append(append([]ReplyResponse{}, base.Responses...), next.Responses...)
	if len(next.ReplyObligations) == 0 {
		next.ReplyObligations = base.ReplyObligations
	}
	next.Findings = append(append([]Finding{}, base.Findings...), next.Findings...)
	next.Operations = append(append([]Intent{}, base.Operations...), next.Operations...)
	next.NoReply = append(append([]Finding{}, base.NoReply...), next.NoReply...)
	if len(next.Changes) == 0 {
		next.Changes = base.Changes
	}
	if len(next.Deliverables) == 0 {
		next.Deliverables = base.Deliverables
	}
	if next.CommitMessage == "" {
		next.CommitMessage = base.CommitMessage
	}
	if len(next.Evidence) == 0 {
		next.Evidence = base.Evidence
	}
	if next.Audience == nil {
		next.Audience = base.Audience
	}
	if next.Scope == nil {
		next.Scope = base.Scope
	}
	if next.Cleanup == nil {
		next.Cleanup = base.Cleanup
	}
	if next.CI == nil {
		next.CI = base.CI
	}
	if len(next.PreferenceDecisions) == 0 {
		next.PreferenceDecisions = base.PreferenceDecisions
	}
	return next
}
func (r *Runtime) effectiveReport(ctx context.Context, c claimRecord) (Report, error) {
	l, e := r.ledger()
	if e != nil {
		return Report{}, e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT message FROM submissions WHERE run=? AND claim=? ORDER BY round", c.Run, c.ID)
	if e != nil {
		l.Close()
		return Report{}, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			l.Close()
			return Report{}, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	l.Close()
	if e != nil {
		return Report{}, e
	}
	messages, e := r.claimMessages(ctx, c)
	if e != nil {
		return Report{}, e
	}
	byID := map[string]workflows.WorkerMessage{}
	for _, m := range messages {
		byID[m.ID] = m
	}
	var result Report
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			return result, fmt.Errorf("original report message missing")
		}
		next, e := parseReport(m)
		if e != nil {
			return result, e
		}
		result = mergeReport(result, next)
	}
	return result, nil
}

func (r *Runtime) claimMessages(ctx context.Context, c claimRecord) ([]workflows.WorkerMessage, error) {
	messages, e := r.Workers.Messages(ctx, c.Engine, c.Worker)
	if e != nil {
		return nil, e
	}
	l, e := r.ledger()
	if e != nil {
		return nil, e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT worker FROM worker_rounds WHERE run=? AND claim=? ORDER BY round", c.Run, c.ID)
	if e != nil {
		l.Close()
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			l.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	l.Close()
	if e != nil {
		return nil, e
	}
	for _, id := range ids {
		if id == c.Worker {
			continue
		}
		part, e := r.Workers.Messages(ctx, c.Engine, id)
		if e != nil {
			return nil, e
		}
		messages = append(messages, part...)
	}
	return messages, nil
}
