package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"github.com/google/jsonschema-go/jsonschema"
)

type claimRecord struct {
	Run, ID, Occurrence, Fingerprint, Cycle, Engine, Worker, Message, State string
	Checks                                                                  int
	ReportMessage, Reason, Snapshot, Assignment, Payload, Operation         string
}

func getClaim(ctx context.Context, tx sqlRW, run, id string) (claimRecord, error) {
	var c claimRecord
	e := tx.QueryRowContext(ctx, "SELECT run,id,occurrence,fingerprint,cycle,engine,worker,message,state,checks,report_message,reason,snapshot,assignment,payload,operation FROM claims WHERE run=? AND id=?", run, id).Scan(&c.Run, &c.ID, &c.Occurrence, &c.Fingerprint, &c.Cycle, &c.Engine, &c.Worker, &c.Message, &c.State, &c.Checks, &c.ReportMessage, &c.Reason, &c.Snapshot, &c.Assignment, &c.Payload, &c.Operation)
	return c, e
}
func (r *Runtime) claim(ctx context.Context, p Plan, ac workflows.ActionContext, in ActionInput) (ActionOutput, error) {
	out := output()
	o, e := p.Occurrence(in.Occurrence)
	if e != nil {
		return out, e
	}
	s, e := r.live(ctx, p)
	if e != nil {
		return out, e
	}
	engine, e := r.origin(ctx, ac)
	if e != nil {
		return out, e
	}
	_, admitted, e := r.Store.Load(ctx, engine)
	if e != nil {
		return out, e
	}
	if digest(admitted.Roles) != digest(p.Roles) {
		return out, fmt.Errorf("role configuration differs from pinned PR-fix preparation")
	}

	if in.Cycle == "" || in.Attempt < 1 || in.Attempt > 2 {
		return out, fmt.Errorf("cycle and bounded attempt required")
	}
	fp := occurrenceFingerprint(s, o)
	if cycleHook(p, o.ID) {
		fp = digest([]string{fp, in.Cycle})
	}
	if o.Kind == "fix" || o.Kind == "verification" || o.Kind == "completion" || o.Kind == "publication" || o.Kind == "ci_repair" {
		sources, e := r.reportSources(ctx, p, o.Kind)
		if e != nil {
			return out, e
		}
		fp = digest([]any{fp, sources})
	}
	var policy *CommitPolicyHandoff
	if o.Kind == "cleanup" {
		policy, e = r.commitPolicy(ctx, p, o, s)
		if e != nil {
			return out, e
		}
		fp = digest([]any{fp, policy})
	}
	var payload any
	var requestInspection any
	var replyInspection any
	if o.Kind == "audience" || o.ID == "reply_recovery" {
		l, e := r.ledger()
		if e != nil {
			return out, e
		}
		intent, e := l.Intent(ctx, p.LogicalRun, in.Payload)
		if e != nil {
			l.Close()
			return out, e
		}
		payload = intent
		fp = digest([]string{fp, intent.Fingerprint()})
		if intent.Kind == "request_review" {
			path, observed, err := latestReviewRequestInspection(ctx, l, intent.Run, intent.ID)
			if err != nil && !missingReviewRequestInspection(err) {
				l.Close()
				return out, err
			}
			if err == nil {
				requestInspection = map[string]any{"path": path, "observation": observed, "fingerprint": observed.Fingerprint(), "assignment": "Reassess current code and complete discussion. If no further review request is needed, return review_request_disposition with decision no_resend, concrete reason, this observation fingerprint and sourced inspection path. Preserve historical delivery as unknown; never resend or claim a receipt. Otherwise leave the obligation incomplete."}
				fp = digest([]any{fp, requestInspection})
			}
		}
		if o.ID == "reply_recovery" {
			path, observed, err := latestReplyInspection(ctx, l, intent.Run, intent.ID)
			if err != nil {
				l.Close()
				return out, err
			}
			replyInspection = map[string]any{"path": path, "observation": observed, "fingerprint": observed.Fingerprint()}
			fp = digest([]any{fp, replyInspection})
		}
		l.Close()
	}
	id := digest([]any{p.LogicalRun, o.ID, ac.OperationKey, in.Attempt, fp})
	snapshot, e := artifact(r.Root, s)
	if e != nil {
		return out, e
	}
	assignment := map[string]any{"assignment": o.Pass.Assignment, "additional_instructions": o.Call.Invocation.Instructions, "inherited_invocation_instructions": o.InheritedInstructions, "user_instruction": p.Instructions, "owning_invocation_instructions": p.Selection.Invocation.Instructions, "repository_instruction_resources": s.RepositoryInstructions, "checklist": o.Checklist, "snapshot": s, "selected_plan": in.Plan, "occurrence": o, "publication_payload": payload, "permissions": p.Selection.Config.Options["permissions"], "authorizations": p.Authorizations, "preferences": p.Selection.Preferences, "settings": p.Selection.Settings, "governing_contract": "Only this leaf assignment is authorized. Read the pinned repository_instruction_resources. Apply owning_invocation_instructions, inherited_invocation_instructions in order and then additional_instructions within this bounded assignment. Never coordinate/spawn agents or choose workflow sequences. Do not publish. Operational reports, handles, diagnostics and receipts remain private. Return one JSON report with explicit complete, findings with IDs/text/sources/dispositions, evidence, blockers, and every checklist item with completed/evidence or unfinished/reason. Every applicable conditional preference needs sourced applicability decisions. Local edits require changes with repository-relative paths and raw SHA-256 before/after hashes (absent/deleted for new/removed files), plus a repository-native commit_message. Preserve unrelated files. Engine-observed execution and original outputs are required; empty findings alone prove nothing."}
	sources, e := r.reportSources(ctx, p, o.Kind)
	if e != nil {
		return out, e
	}
	assignment["original_result_references"] = sources
	if requestInspection != nil {
		assignment["review_request_inspection"] = requestInspection
	}
	if replyInspection != nil {
		assignment["reply_inspection"] = replyInspection
	}
	if policy != nil {
		assignment["commit_policy"] = policy
	}
	if o.Kind == "reply" {
		assignment["reply_obligations"], e = r.replyObligations(ctx, p)
		if e != nil {
			return out, e
		}
	}
	if o.Kind == "completion" {
		assignment["completion_instructions"] = p.Selection.Config.Options["completion_instructions"]
	}
	base, e := materializeSource(r.Root)
	if e != nil {
		return out, e
	}
	assignment["reference_contract_root"] = filepath.Join(base, "source")
	assignment["reference_contract_usage"] = "Read applicable pinned source contracts for duties, evidence and public-text rules. Only this bounded leaf is authorized; source orchestration, delegation and publication instructions are owned by the YAML controller. Do not invoke plugin scripts or agents."
	if o.Kind == "scope" {
		assignment["scope_accounting"], e = r.scopeAccounting(ctx, p, o)
		if e != nil {
			return out, e
		}
	}
	if o.Kind == "verification" || o.Kind == "fix" {
		if o.Kind == "fix" {
			assignment["candidates"], e = r.fixCandidates(ctx, p)
		} else {
			assignment["candidates"], e = r.candidates(ctx, p)
		}
		if e != nil {
			return out, e
		}
	}
	current, e := resolveSelection(ctx, r.Root, p.Identity, p.Upstream, p.Options, s.Paths)
	if e != nil {
		return out, e
	}
	mode := ""
	if o.Review || o.Kind == "verification" {
		mode = "review"
	} else if mutates(o) {
		mode = "implementation"
	}
	assignment["preferences"] = current.Preferences[mode]
	if o.Kind == "ci" || o.Kind == "ci_repair" {
		assignment["ci_observation"], _ = r.latestCI(ctx, p, s.Head)
	}
	schema, e := jsonschema.For[Report](nil)
	if e != nil {
		return out, e
	}
	assignment["report_schema"] = schema
	assignment["report_limits"] = map[string]int{"bytes": 16 << 20, "collection_entries": maxReportCollection, "aggregate_entries": maxReportEntries, "nesting_levels": maxReportDepth}
	prompt := string(jsonBytes(assignment))
	if len(prompt) > 1<<20 || len(checklistPrompt([]byte(prompt), o, o.MaxChecks)) > 1<<20 {
		return out, fmt.Errorf("bounded leaf assignment and checklist envelope exceed 1 MiB")
	}
	assignmentPath, e := artifact(r.Root, assignment)
	if e != nil {
		return out, e
	}
	l, e := r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	e = l.transaction(ctx, func(tx sqlRW) error {
		c, e := getClaim(ctx, tx, p.LogicalRun, id)
		if e == nil {
			out = claimOutput(c)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if o.Cacheable {
			var reusable string
			e = tx.QueryRowContext(ctx, "SELECT id FROM claims WHERE run=? AND occurrence=? AND fingerprint=? AND state='complete' ORDER BY rowid DESC LIMIT 1", p.LogicalRun, o.ID, fp).Scan(&reusable)
			if e == nil {
				c, e = getClaim(ctx, tx, p.LogicalRun, reusable)
				out = claimOutput(c)
				out.Invoke = false
				return e
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		var active int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM claims WHERE run=? AND occurrence=? AND state IN ('claimed','recheck','recover_required')", p.LogicalRun, o.ID).Scan(&active); e != nil {
			return e
		}
		if active > 0 {
			return fmt.Errorf("unfinished occurrence %s requires reconciliation; claims never expire", o.ID)
		}
		if in.Attempt > 1 {
			var previous, reason, cycle string
			if e = tx.QueryRowContext(ctx, "SELECT id,reason,cycle FROM claims WHERE run=? AND occurrence=? AND fingerprint=? AND state='incomplete' ORDER BY rowid DESC LIMIT 1", p.LogicalRun, o.ID, fp).Scan(&previous, &reason, &cycle); e != nil {
				return fmt.Errorf("eligible same-input incomplete attempt required: %w", e)
			}
			if reason != "usage_limit" && reason != "session_limit" || cycle != in.Cycle {
				return fmt.Errorf("only usage/session limits permit same-cycle retries")
			}
			if e = reserve(ctx, tx, p.LogicalRun, "retry", o.ID+":"+in.Cycle, id, reason); e != nil {
				return e
			}
		}
		if o.Review {
			if e = reserve(ctx, tx, p.LogicalRun, "review", "all", in.Cycle, "selected reviewer pass"); e != nil {
				return e
			}
		}
		worker := "prfix_" + digest([]string{p.LogicalRun, id})[:32]
		_, e = tx.ExecContext(ctx, "INSERT INTO claims(run,id,occurrence,fingerprint,cycle,engine,worker,state,snapshot,assignment,payload,operation) VALUES(?,?,?,?,?,?,?,'claimed',?,?,?,?)", p.LogicalRun, id, o.ID, fp, in.Cycle, engine, worker, snapshot, assignmentPath, in.Payload, ac.OperationKey)
		out = output()
		out.Claim = id
		out.Invoke = true
		out.Worker = worker
		out.Prompt = prompt
		return e
	})
	return out, e
}
func claimOutput(c claimRecord) ActionOutput {
	out := output()
	out.Claim = c.ID
	out.Worker = c.Worker
	out.Invoke = c.State == "claimed"
	out.Recheck = c.State == "recheck"
	out.Settled = c.State == "complete" || c.State == "optional"
	out.State = c.State
	return out
}
func (r *Runtime) record(ctx context.Context, p Plan, in ActionInput) (*Ledger, claimRecord, error) {
	l, e := r.ledger()
	if e != nil {
		return nil, claimRecord{}, e
	}
	c, e := getClaim(ctx, l.db, p.LogicalRun, in.Claim)
	if e != nil {
		l.Close()
		return nil, c, e
	}
	return l, c, nil
}
func (r *Runtime) workerSpawn(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	defer l.Close()
	if r.Workers == nil {
		return output(), fmt.Errorf("managed workers unavailable")
	}
	o, e := p.Occurrence(c.Occurrence)
	if e != nil {
		return output(), e
	}
	w, e := r.Workers.Spawn(ctx, workflows.WorkerSpec{RunID: c.Engine, Role: o.Pass.Role, StableID: c.Worker, Turns: 5, Tools: nil})
	out := claimOutput(c)
	out.Worker = w.ID
	return out, e
}
func messageRequest(c claimRecord, round int) string {
	return "prfix_" + digest([]any{c.ID, round})[:48]
}
func (r *Runtime) workerSend(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	defer l.Close()
	if r.Workers == nil {
		return output(), fmt.Errorf("managed workers unavailable")
	}
	if c.State == "complete" || c.State == "optional" {
		return claimOutput(c), nil
	}
	if in.Round > 0 && (c.State != "recheck" || in.Round != c.Checks) {
		return output(), fmt.Errorf("checklist round is not the retained pending round")
	}
	b, e := os.ReadFile(c.Assignment)
	if e != nil {
		return output(), e
	}
	prompt := string(b)
	if in.Round > 0 {
		o, _ := p.Occurrence(c.Occurrence)
		prompt = checklistPrompt(b, o, in.Round)
	}
	worker, e := r.roundWorker(ctx, c, in.Round)
	if e != nil {
		return output(), e
	}
	w, e := r.Workers.Get(ctx, c.Engine, worker)
	if e != nil {
		return output(), e
	}
	existing, existingErr := r.message(ctx, c, in.Round)
	if w.Status == "closed" || w.Status == workflows.RunStatusNeedsAttention {
		if errors.Is(existingErr, sql.ErrNoRows) || existingErr == nil && resumableNative(existing) {
			out := claimOutput(c)
			out.Deferred = true
			return out, nil
		}
	}
	_, e = r.Workers.Send(ctx, c.Engine, worker, messageRequest(c, in.Round), prompt)
	return claimOutput(c), e
}
func (r *Runtime) message(ctx context.Context, c claimRecord, round int) (workflows.WorkerMessage, error) {
	if r.Workers == nil {
		return workflows.WorkerMessage{}, fmt.Errorf("managed workers unavailable")
	}
	worker, e := r.roundWorker(ctx, c, round)
	if e != nil {
		return workflows.WorkerMessage{}, e
	}
	messages, e := r.Workers.Messages(ctx, c.Engine, worker)
	if e != nil {
		return workflows.WorkerMessage{}, e
	}
	for _, m := range messages {
		if m.RequestID == messageRequest(c, round) {
			return m, nil
		}
	}
	return workflows.WorkerMessage{}, sql.ErrNoRows
}
func (r *Runtime) workerWait(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	l.Close()
	if c.State == "complete" || c.State == "optional" {
		return claimOutput(c), nil
	}
	interval := r.PollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	for {
		worker, e := r.roundWorker(ctx, c, in.Round)
		if e != nil {
			return output(), e
		}
		w, e := r.Workers.Get(ctx, c.Engine, worker)
		if e != nil {
			return output(), e
		}
		// Observe the producer before its mailbox: ordinary native failures
		// persist the terminal message before changing worker status.
		m, e := r.message(ctx, c, in.Round)
		if e != nil {
			return output(), e
		}
		switch m.Status {
		case "interrupted", workflows.RunStatusSucceeded, workflows.RunStatusFailed, workflows.RunStatusCanceled, workflows.RunStatusNeedsAttention:
			return claimOutput(c), nil
		}
		if w.Status == "closed" || w.Status == workflows.RunStatusNeedsAttention {
			// Setup can fail before the core dequeues the message. Preserve that
			// unresolved mailbox and surface the stopped producer instead of
			// waiting for a terminal message it can no longer produce.
			return output(), fmt.Errorf("native worker stopped with unresolved message (%s): %s", w.FailureClass, w.Error)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return output(), ctx.Err()
		case <-timer.C:
		}
	}
}
func parseReport(m workflows.WorkerMessage) (Report, error) {
	var report Report
	text, _ := m.Result.Outputs["text"].(string)
	if strings.TrimSpace(text) == "" {
		if raw, ok := m.Result.Outputs["structured"]; ok {
			text = string(jsonBytes(raw))
		}
	}
	if !validText(text) {
		return report, fmt.Errorf("original worker output required")
	}
	if e := validateReportShape(text); e != nil {
		return report, e
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.DisallowUnknownFields()
	if e := d.Decode(&report); e != nil {
		return report, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return report, fmt.Errorf("one original JSON report required")
	}
	return report, nil
}
func checklistPrompt(assignment []byte, o Occurrence, round int) string {
	return string(jsonBytes(map[string]any{"assignment": json.RawMessage(assignment), "round": round, "checklist": o.Checklist, "instruction": "Verify every item in this same session; finish missing work. Preserve the original analysis and findings. Return complete, every checklist ID with evidence or an unfinished reason, additional findings and blockers. No new analysis/review invocation is authorized."}))
}

func messageRef(c claimRecord, m workflows.WorkerMessage) string {
	return "engine://" + c.Engine + "/workers/" + m.WorkerID + "/messages/" + m.ID
}
func (r *Runtime) reportSources(ctx context.Context, p Plan, kind string) ([]string, error) {

	reports, e := r.reports(ctx, p)
	if e != nil {
		return nil, e
	}
	refs := map[string]bool{}
	for _, rr := range reports {
		o := rr.Occurrence
		relevant := false
		switch kind {
		case "fix":
			relevant = o.Review || o.Kind == "inspection" || o.Kind == "verification"
		case "verification":
			relevant = o.Review
		case "completion":
			relevant = o.Kind != "completion" && o.Kind != "audience"
		case "cleanup":
			relevant = o.Kind == "commit_policy"
		case "publication":
			relevant = o.Kind == "description" || o.Kind == "reply" || o.Kind == "decisions" || o.Kind == "cleanup"
		}
		if relevant {
			refs[rr.Reference] = true
		}
	}
	result := sortedKeys(refs)

	return result, nil
}

func (r *Runtime) validateSource(ctx context.Context, p Plan, source string) error {
	if strings.HasPrefix(source, "engine://") {
		parts := strings.Split(strings.TrimPrefix(source, "engine://"), "/")
		if len(parts) != 5 || parts[1] != "workers" || parts[3] != "messages" || r.Workers == nil {
			return fmt.Errorf("invalid engine evidence reference")
		}
		messages, e := r.Workers.Messages(ctx, parts[0], parts[2])
		if e != nil {
			return e
		}
		for _, m := range messages {
			text, _ := m.Result.Outputs["text"].(string)
			if m.ID == parts[4] && m.Status != workflows.RunStatusRunning && m.Status != workflows.RunStatusQueued && (validText(text) || m.Result.Outputs["structured"] != nil) {
				return nil
			}
		}
		return fmt.Errorf("missing original engine evidence")
	}
	if source == p.Identity.URL || strings.HasPrefix(source, p.Identity.URL+"#") || strings.HasPrefix(source, p.Identity.URL+"/") {
		return nil
	}
	path := source
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.Root, path)
	}
	abs, e := filepath.EvalSymlinks(path)
	if e != nil {
		return fmt.Errorf("missing evidence %s: %w", source, e)
	}
	root, e := filepath.EvalSymlinks(r.Root)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(root, abs)
	if e != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("evidence outside captured repository")
	}
	st, e := os.Stat(abs)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("evidence must be a regular file")
	}
	return nil
}
func (r *Runtime) validateReport(ctx context.Context, p Plan, o Occurrence, report Report) (bool, error) {
	if report.Complete && len(report.Blockers) > 0 {
		return false, fmt.Errorf("a complete report cannot have blockers")
	}
	if !report.Complete && len(report.Checklist) == 0 {
		return false, nil
	}
	if len(report.Evidence) == 0 {
		return false, fmt.Errorf("complete reports require original evidence")
	}
	for _, ref := range report.Evidence {
		if e := r.validateSource(ctx, p, ref); e != nil {
			return false, e
		}
	}
	if report.Complete {
		if e := r.validatePreferenceDecisions(ctx, p, o, report); e != nil {
			return false, e
		}
	}
	for _, obligation := range report.ReplyObligations {
		if !validText(obligation.ID) || !validText(obligation.Text) || !validText(obligation.Reason) || len(obligation.Sources) == 0 {
			return false, fmt.Errorf("reply obligation needs identity, text, reason and original sources")
		}
		for _, ref := range obligation.Sources {
			if e := r.validateSource(ctx, p, ref); e != nil {
				return false, e
			}
		}
	}
	for _, response := range report.Responses {
		if !validText(response.ID) || !validText(response.Reason) || !contains([]string{"reply", "no_reply"}, response.Outcome) || len(response.Sources) == 0 {
			return false, fmt.Errorf("response needs sourced reply/no-reply disposition")
		}
		for _, ref := range response.Sources {
			if e := r.validateSource(ctx, p, ref); e != nil {
				return false, e
			}
		}
	}

	for _, f := range report.NoReply {
		if !validText(f.ID) || !validText(f.Text) || !validText(f.Reason) || f.Disposition != "no_reply" || len(f.Sources) == 0 {
			return false, fmt.Errorf("no-reply assessments require original source and reason")
		}
		for _, ref := range f.Sources {
			if e := r.validateSource(ctx, p, ref); e != nil {
				return false, e
			}
		}
	}
	for _, f := range report.Findings {
		if !validText(f.ID) || !validText(f.Text) || len(f.Sources) == 0 {
			return false, fmt.Errorf("sourced finding identity/text required")
		}
		for _, ref := range f.Sources {
			if e := r.validateSource(ctx, p, ref); e != nil {
				return false, e
			}
		}
		if f.Disposition == "accepted" || f.Disposition == "deferred" {
			a, ok := p.Authorizations["accept:"+f.ID]
			if !ok || f.Acceptance == nil || a != *f.Acceptance {
				return false, fmt.Errorf("finding acceptance must cite the exact retained user authorization")
			}
		}
	}
	if report.Complete && (o.Kind == "fix" || o.Kind == "verification") {
		var candidates []Candidate
		var e error
		if o.Kind == "fix" {
			candidates, e = r.fixCandidates(ctx, p)
		} else {
			candidates, e = r.candidates(ctx, p)
		}
		if e != nil {
			return false, e
		}
		for _, candidate := range candidates {
			covered := false
			for _, f := range report.Findings {
				allowed := contains([]string{"fixed", "not_actionable", "accepted", "deferred"}, f.Disposition)
				if o.Kind == "verification" {
					allowed = contains([]string{"confirmed", "dismissed", "duplicate"}, f.Disposition)
				}
				if f.ID == candidate.Finding.ID && contains(f.Sources, candidate.Original) && validText(f.Reason) && allowed {
					covered = true
				}
			}
			if !covered {
				return false, nil
			}
		}
	}
	if o.Kind == "commit_policy" && (len(report.Changes) > 0 || len(report.Operations) > 0 || report.Cleanup != nil) {
		return false, fmt.Errorf("commit-policy provider is read-only")
	}
	if report.Complete && o.Kind == "commit_policy" && !validText(report.PolicyPrompt) {
		return false, fmt.Errorf("selected commit-policy provider requires nonempty policy_prompt")
	}
	if report.Complete && o.Kind == "scope" {
		if e := r.validateScope(ctx, p, o, report.Scope); e != nil {
			return false, e
		}
	}
	if report.Complete && o.Kind == "audience" && report.Audience == nil {
		return false, nil
	}
	if report.Complete && o.Kind == "ci" && report.CI == nil {
		return false, nil
	}

	if len(report.Checklist) == 0 {
		return false, nil
	}
	expected := map[string]bool{}
	for _, item := range o.Checklist {
		expected[item.ID] = false
	}
	complete := true
	for _, check := range report.Checklist {
		done, ok := expected[check.ID]
		if !ok || done {
			return false, fmt.Errorf("unknown or duplicate checklist item %s", check.ID)
		}
		expected[check.ID] = true
		if check.Status == "completed" {
			if len(check.Evidence) == 0 {
				return false, fmt.Errorf("completed checklist items require evidence")
			}
			for _, ref := range check.Evidence {
				if e := r.validateSource(ctx, p, ref); e != nil {
					return false, e
				}
			}
		} else if check.Status == "unfinished" && validText(check.Reason) {
			complete = false
		} else {
			return false, fmt.Errorf("malformed checklist outcome")
		}
	}
	for _, seen := range expected {
		if !seen {
			return false, fmt.Errorf("every prepared checklist ID must be covered")
		}
	}
	if report.Complete != complete {
		return false, fmt.Errorf("complete must agree with checklist outcomes")
	}
	return complete, nil
}
func (r *Runtime) finish(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	defer l.Close()
	if c.State == "complete" || c.State == "optional" || c.State == "incomplete" {
		return claimOutput(c), nil
	}
	o, e := p.Occurrence(c.Occurrence)
	if e != nil {
		return output(), e
	}
	if in.Round == 0 && c.ReportMessage != "" {
		return claimOutput(c), nil
	}
	m, e := r.message(ctx, c, in.Round)
	if e != nil {
		return output(), e
	}
	if m.Status == workflows.RunStatusRunning || m.Status == workflows.RunStatusQueued {
		return output(), fmt.Errorf("cannot finish an active worker")
	}
	report, parseErr := parseReport(m)
	if in.Round > 0 && parseErr == nil {
		base, e := r.effectiveReport(ctx, c)
		if e != nil {
			return output(), e
		}
		report = mergeReport(base, report)
	}
	accepted := false
	reason := "native task did not provide accepted completion evidence"
	state := "incomplete"
	checks := c.Checks
	if m.Status == workflows.RunStatusSucceeded && m.Result.SessionID != "" && parseErr == nil {
		accepted, e = r.validateReport(ctx, p, o, report)
		if e != nil {
			return output(), e
		}
		if report.Complete || in.Round > 0 {
			if accepted {
				state = "complete"
			} else {
				state = "recheck"
				checks++
				if checks > o.MaxChecks {
					state = "incomplete"
					reason = "mandatory checklist exhausted"
				}
			}
		}
	}

	if m.Status != workflows.RunStatusSucceeded {
		reason = m.FailureClass + ": " + m.Error
		if in.Round == 0 {
			switch m.FailureClass {
			case "storage", "authentication", "needs_attention", "action_integrity", "canceled", "timeout":
			default:
				text := strings.ToLower(m.Error)
				if m.FailureClass == "usage_exhausted" || strings.Contains(text, "usage limit") || strings.Contains(text, "usage_limit") || m.Result.ProviderStatus == "error_max_budget_usd" {
					reason = "usage_limit"
				}
				if strings.Contains(text, "session limit") || strings.Contains(text, "session_limit") || m.Result.ProviderStatus == "error_max_turns" {
					reason = "session_limit"
				}
			}
		}
	} else if parseErr != nil {
		reason = "invalid original report: " + parseErr.Error()
	}

	if !o.Required && state == "incomplete" {
		state = "optional"
	}
	live, liveErr := r.live(ctx, p)
	if liveErr != nil {
		state = "stale"
		reason = "governing evidence unavailable: " + liveErr.Error()
	}
	var initial Snapshot
	if e = readJSON(c.Snapshot, &initial); e != nil {
		return output(), e
	}
	contextChanged := initial.Discussion != live.Discussion || initial.Settings != live.Settings || initial.Instructions != live.Instructions || initial.Resources != live.Resources || initial.TargetTree != live.TargetTree || initial.BaseTree != live.BaseTree
	for _, path := range o.resourcePaths() {
		contextChanged = contextChanged || initial.ResourceDigests[path] != live.ResourceDigests[path]
	}
	contextChanged = contextChanged || initial.RoleDigests[o.Pass.Role] != live.RoleDigests[o.Pass.Role]
	if mutates(o) {
		contextChanged = contextChanged || initial.ImplementationPreferences != live.ImplementationPreferences
	}
	if liveErr == nil && (contextChanged || !mutates(o) && occurrenceFingerprint(initial, o) != occurrenceFingerprint(live, o)) {
		state = "stale"
		reason = "governing inputs changed during execution"
	}
	if state == "complete" && o.Kind == "cleanup" {
		if e = r.validateCleanup(ctx, p, initial, live, report.Cleanup); e != nil {
			return output(), e
		}
	}
	// Record proposals only after accepted primary execution/checklist. Every actual
	// write still needs a separate audience occurrence and current publication gate.
	if state == "complete" {
		for _, intent := range report.Operations {
			intent.Discussion = live.Discussion
			if intent.Kind != "push" && intent.Kind != "rewrite" && intent.Kind != "split_push" {
				ev, err := readPREvidence(live.Evidence)
				if err != nil {
					return output(), err
				}
				if ev.ViewerID <= 0 {
					return output(), fmt.Errorf("verified numeric publication actor required")
				}
				intent.ActorID = ev.ViewerID
			}
			if intent.Kind == "edit_pr" {
				var evidence struct {
					PR PREvidence `json:"pr"`
				}
				b, e := os.ReadFile(live.Evidence)
				if e != nil {
					return output(), e
				}
				if e = json.Unmarshal(b, &evidence); e != nil {
					return output(), e
				}
				intent.Before = map[string]any{}
				for key := range intent.Payload {
					switch key {
					case "body":
						intent.Before[key] = evidence.PR.Metadata.Body
					case "title":
						intent.Before[key] = evidence.PR.Metadata.Title
					}
				}
			}
			if intent.Kind == "reply" || intent.Kind == "reply_inline" || intent.Kind == "split_pr" {
				ev, err := readPREvidence(live.Evidence)
				if err != nil {
					return output(), err
				}
				if intent.Kind == "split_pr" {
					inventory, ok := r.provider().(PullRequestInventory)
					if !ok {
						return output(), fmt.Errorf("provider cannot capture a complete split creation baseline")
					}
					numbers, err := inventory.PullRequestNumbers(ctx, p.Identity)
					if err != nil {
						return output(), err
					}
					intent.Before = map[string]any{"pr_numbers": numbers}
				} else {
					comments := ev.Comments
					if intent.Kind == "reply_inline" {
						comments = ev.Inline
					}
					ids := []string{}
					for _, comment := range comments {
						id := githubID(comment, "id")
						if !numeric(id) {
							return output(), fmt.Errorf("complete numeric comment baseline required")
						}
						ids = append(ids, id)
					}
					intent.Before = map[string]any{"comment_ids": ids}
					if matches := matchingReplies(ev, intent); len(matches) == 1 {
						intent.Before["satisfied_comment_id"] = githubID(matches[0], "id")
					}
				}
			}
			if intent.Replaces != nil {
				if e = r.acceptReplyReplacement(ctx, p, c, intent); e != nil {
					return output(), e
				}
			}
			if e = l.PrepareIntent(ctx, p, intent, messageRef(c, m)); e != nil {
				return output(), e
			}
			id := intent.ID
			if id == "" {
				id = intentID(p.LogicalRun, intent)
			}
			retained, err := l.Intent(ctx, p.LogicalRun, id)
			if err != nil {
				return output(), err
			}
			var existingState string
			if err = l.db.QueryRowContext(ctx, "SELECT state FROM effects WHERE run=? AND id=?", retained.Run, retained.ID).Scan(&existingState); err != nil {
				return output(), err
			}
			if existingState == "completed" {
				if intent.Kind == "ci_rerun" {
					return output(), fmt.Errorf("a completed rerun cannot be proposed as a new attempt; use current CI evidence")
				}
				observed, _, err := r.reconcileCompletedIntent(ctx, l, retained)
				if err != nil {
					return output(), err
				}
				if observed != "completed" {
					return output(), fmt.Errorf("completed operation no longer matches live evidence; reconcile its retained obligation")
				}
			}
		}
	}
	ref := messageRef(c, m)
	e = l.transaction(ctx, func(tx sqlRW) error {
		var previous, status string
		e := tx.QueryRowContext(ctx, "SELECT message,status FROM submissions WHERE run=? AND claim=? AND round=?", p.LogicalRun, c.ID, in.Round).Scan(&previous, &status)
		if e == nil {
			if previous != m.ID {
				return fmt.Errorf("conflicting checklist submission")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if in.Round > 0 && (c.State != "recheck" || in.Round != c.Checks) {
			return fmt.Errorf("pending checklist round mismatch")
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO submissions VALUES(?,?,?,?,?)", p.LogicalRun, c.ID, in.Round, m.ID, state); e != nil {
			return e
		}
		reportMessage := c.ReportMessage
		if in.Round == 0 {
			reportMessage = m.ID
		}
		_, e = tx.ExecContext(ctx, "UPDATE claims SET state=?,checks=?,report_message=?,message=?,reason=? WHERE run=? AND id=?", state, min(checks, o.MaxChecks), reportMessage, m.ID, reason, p.LogicalRun, c.ID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, ref, "report", ref)
		return e
	})
	if e != nil {
		return output(), e
	}
	c.State = state
	c.Checks = min(checks, o.MaxChecks)
	c.ReportMessage = m.ID
	return claimOutput(c), nil
}
func (r *Runtime) status(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	defer l.Close()
	o, e := p.Occurrence(c.Occurrence)
	if e != nil {
		return output(), e
	}
	out := claimOutput(c)
	if out.Settled {
		return out, nil
	}
	if c.State == "incomplete" && (c.Reason == "usage_limit" || c.Reason == "session_limit") {
		var used int
		e = l.db.QueryRowContext(ctx, "SELECT count(*) FROM reservations WHERE run=? AND kind='retry' AND scope=?", p.LogicalRun, o.ID+":"+c.Cycle).Scan(&used)
		if e != nil {
			return out, e
		}
		var raw string
		if e = l.db.QueryRowContext(ctx, "SELECT limits FROM logical_runs WHERE id=?", p.LogicalRun).Scan(&raw); e != nil {
			return out, e
		}
		var limits Limits
		if e = json.Unmarshal([]byte(raw), &limits); e != nil {
			return out, e
		}
		if used < limits.Retries {
			return out, nil
		}
	}
	return out, fmt.Errorf("required occurrence %s is %s: %s", c.Occurrence, c.State, c.Reason)
}

func (r *Runtime) validatePreferenceDecisions(ctx context.Context, p Plan, o Occurrence, report Report) error {
	mode := ""
	if o.Review || o.Kind == "verification" {
		mode = "review"
	} else if mutates(o) {
		mode = "implementation"
	}
	if mode == "" {
		return nil
	}
	s, e := r.live(ctx, p)
	if e != nil {
		return e
	}
	selected, e := resolveSelection(ctx, r.Root, p.Identity, p.Upstream, p.Options, s.Paths)
	if e != nil {
		return e
	}
	handoff, _ := selected.Preferences[mode].(map[string]any)
	records, _ := handoff["records"].([]any)
	required := map[string]bool{}
	matched := map[string][]string{}
	for _, value := range records {
		item, _ := value.(map[string]any)
		conditional, _ := item["requires_condition"].(bool)
		if !conditional {
			continue
		}
		record, _ := item["record"].(map[string]any)
		id := stringValue(record, "id")
		required[id] = false
		matched[id] = stringList(item["paths"])
	}
	for _, decision := range report.PreferenceDecisions {
		seen, ok := required[decision.ID]
		if !ok || seen || !validText(decision.Reason) || len(decision.Sources) == 0 {
			return fmt.Errorf("invalid or unsourced conditional preference decision %s", decision.ID)
		}
		for _, ref := range decision.Sources {
			if e = r.validateSource(ctx, p, ref); e != nil {
				return e
			}
		}
		if !decision.Applicable && len(decision.Paths) > 0 || decision.Applicable && len(matched[decision.ID]) > 0 && len(decision.Paths) == 0 {
			return fmt.Errorf("preference decision applicability and matched paths disagree")
		}
		seenPaths := map[string]bool{}
		for _, path := range decision.Paths {
			if !contains(matched[decision.ID], path) || seenPaths[path] {
				return fmt.Errorf("preference decision path outside matched preference scope or duplicated")
			}
			seenPaths[path] = true
		}
		required[decision.ID] = true
	}
	for id, seen := range required {
		if !seen {
			return fmt.Errorf("conditional preference %s requires a sourced applicability decision", id)
		}
	}
	return nil
}
