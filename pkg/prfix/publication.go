package prfix

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

var operationPermissions = map[string]string{"push": "push_commits", "rewrite": "rewrite_commits", "edit_pr": "edit_pr", "reply": "reply_to_feedback", "reply_inline": "reply_to_feedback", "resolve_thread": "resolve_threads", "split_push": "create_draft_prs", "split_pr": "create_draft_prs", "request_review": "request_review", "ci_rerun": "push_commits", "merge": "merge_pr"}

func intentID(run string, i Intent) string {
	values := []any{run, i.Kind, i.Head, i.Lease, i.Target, i.Branch, i.Payload}
	if i.Replaces != nil {
		values = append(values, i.Replaces)
	}
	return digest(values)
}

func (l *Ledger) PrepareIntent(ctx context.Context, p Plan, intent Intent, source string) error {
	if _, ok := operationPermissions[intent.Kind]; !ok {
		return fmt.Errorf("unsupported effect kind %q", intent.Kind)
	}
	if intent.Run != "" && intent.Run != p.LogicalRun || intent.PR.URL != "" && intent.PR != p.Identity {
		return fmt.Errorf("publication proposal changed PR/run identity")
	}
	if intent.Kind == "ci_rerun" {
		if _, e := rerunAttempt(intent.Payload); e != nil {
			return e
		}
	}
	intent.Run = p.LogicalRun
	intent.PR = p.Identity
	intent.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	permission, ok := p.Authorizations[intent.Kind]
	if !ok && intent.Kind != "merge" {
		permission, ok = p.Authorizations["iterate"]
	}
	if !ok {
		return fmt.Errorf("no retained user authorization for %s", intent.Kind)
	}
	if e := permission.Validate(); e != nil {
		return e
	}
	intent.Authorization = permission
	if !validSHA(intent.Head) || !validSHA(intent.Lease) || !validSHA(intent.Target) || len(intent.Sources) == 0 {
		return fmt.Errorf("exact head/lease/target and original sources required")
	}
	if intent.Kind == "split_pr" {
		pushed, err := l.Intent(ctx, p.LogicalRun, stringValue(intent.Payload, "push_operation"))
		if err != nil {
			return err
		}
		if pushed.Kind != "split_push" || !validSHA(stringValue(pushed.Payload, "head")) {
			return fmt.Errorf("split PR needs its exact prepared branch head")
		}
		intent.CreatedHead = stringValue(pushed.Payload, "head")
	}
	intent.Sources = append(intent.Sources, source)
	intent.AudienceClaim = ""
	if intent.ID == "" {
		intent.ID = intentID(p.LogicalRun, intent)
	}
	if strings.ContainsAny(intent.ID, "/\\\x00") || len(intent.ID) > 128 {
		return fmt.Errorf("invalid operation ID")
	}
	switch intent.Kind {
	case "reply", "reply_inline", "split_pr":
		body := stringValue(intent.Payload, "body")
		if !validText(body) {
			return fmt.Errorf("nonempty public body required")
		}
	}
	b := string(jsonBytes(intent))
	return l.transaction(ctx, func(tx sqlRW) error {
		var saved, state string
		e := tx.QueryRowContext(ctx, "SELECT intent,state FROM effects WHERE run=? AND id=?", p.LogicalRun, intent.ID).Scan(&saved, &state)
		if e == nil {
			var old Intent
			if e = json.Unmarshal([]byte(saved), &old); e != nil {
				return e
			}
			old.Sources = intent.Sources
			if state == "completed" {
				old.Discussion = intent.Discussion
				old.Before = intent.Before
			}
			if old.Fingerprint() != intent.Fingerprint() {
				return fmt.Errorf("conflicting operation identity")
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if intent.Replaces != nil {
			if e := validateReplacementLink(ctx, tx, p, intent); e != nil {
				return e
			}
		} else if handled, err := prepareReplyAlias(ctx, tx, intent); handled || err != nil {
			return err
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO effects(run,id,intent,state) VALUES(?,?,?,'prepared')", p.LogicalRun, intent.ID, b)
		if e == nil && intent.Replaces != nil {
			e = linkReplyReplacement(ctx, tx, intent)
		}
		return e
	})
}
func (l *Ledger) Intent(ctx context.Context, run, id string) (Intent, error) {
	var raw string
	var i Intent
	e := l.db.QueryRowContext(ctx, "SELECT intent FROM effects WHERE run=? AND id=?", run, id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		e = l.db.QueryRowContext(ctx, "SELECT e.intent FROM effect_aliases a JOIN effects e ON e.run=a.canonical_run AND e.id=a.canonical_id WHERE a.run=? AND a.id=?", run, id).Scan(&raw)
	}
	if e == nil {
		e = json.Unmarshal([]byte(raw), &i)
	}
	return i, e
}
func (r *Runtime) intents(ctx context.Context, p Plan, kind string) (ActionOutput, error) {
	out := output()
	l, e := r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	selector := "<>"
	if kind == "merge" {
		selector = "="
	}
	rows, e := l.db.QueryContext(ctx, "SELECT id FROM effects WHERE run=? AND state NOT IN ('completed','superseded') AND json_extract(intent,'$.kind') "+selector+" 'merge' ORDER BY rowid", p.LogicalRun)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return out, e
		}
		out.Items = append(out.Items, IntentRef{id})
	}
	return out, rows.Err()
}
func (r *Runtime) publicationGate(ctx context.Context, p Plan, i Intent, claim string) error {
	if i.Run != p.LogicalRun || i.PR != p.Identity {
		return fmt.Errorf("operation identity mismatch")
	}
	auth, ok := p.Authorizations[i.Kind]
	if !ok && i.Kind != "merge" {
		auth, ok = p.Authorizations["iterate"]
	}
	if !ok || auth != i.Authorization {
		return fmt.Errorf("publication requires exact retained user authority")
	}
	permission := operationPermissions[i.Kind]
	permissions, _ := p.Selection.Config.Options["permissions"].(map[string]any)
	if permission != "merge_pr" {
		allowed, _ := permissions[permission].(bool)
		if !allowed {
			return fmt.Errorf("saved policy forbids %s", i.Kind)
		}
	}
	l, e := r.ledger()
	if e != nil {
		return e
	}
	rows, e := l.db.QueryContext(ctx, "SELECT run,id,json_extract(intent,'$.kind') FROM effects WHERE json_extract(intent,'$.pr.url')=? AND (state IN ('dispatched','unknown') OR (?='request_review' AND state='disposed' AND json_extract(intent,'$.kind')='request_review'))", i.PR.URL, i.Kind)
	if e != nil {
		l.Close()
		return e
	}
	for rows.Next() {
		var run, id, kind string
		if e = rows.Scan(&run, &id, &kind); e != nil {
			break
		}
		if run == i.Run && id == i.ID {
			continue
		}
		if (i.Kind == "reply" || i.Kind == "reply_inline") && kind == "request_review" {
			continue
		}
		e = fmt.Errorf("pending %s operation %s requires read-only reconciliation before another publication", kind, id)
		break
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		l.Close()
		return e
	}
	c, e := getClaim(ctx, l.db, p.LogicalRun, claim)
	l.Close()
	if e != nil {
		return e
	}
	if e = r.verifyReviewRequestDispositions(ctx, p); e != nil {
		return e
	}
	if c.Occurrence != "audience" || c.Payload != i.ID || c.State != "complete" {
		return fmt.Errorf("accepted managed audience review of the exact payload required")
	}
	var assignment map[string]json.RawMessage
	if e = readJSON(c.Assignment, &assignment); e != nil {
		return e
	}
	var reviewed Intent
	if e = json.Unmarshal(assignment["publication_payload"], &reviewed); e != nil {
		return e
	}
	report, e := r.effectiveReport(ctx, c)
	if e != nil {
		return e
	}
	if report.Audience == nil || !report.Audience.Suitable || report.Audience.Fingerprint != i.Fingerprint() || !validText(report.Audience.Audience) || len(report.Audience.Evidence) == 0 {
		return fmt.Errorf("native audience assessment must name the actual payload fingerprint and intended audience")
	}
	for _, ref := range report.Audience.Evidence {
		if e = r.validateSource(ctx, p, ref); e != nil {
			return e
		}
	}
	if reviewed.Fingerprint() != i.Fingerprint() {
		return fmt.Errorf("actual publication payload changed after audience review")
	}
	s, e := r.live(ctx, p)
	if e != nil {
		return e
	}
	if i.Discussion != "" && s.Discussion != i.Discussion {
		allowed, e := r.ownDiscussionDelta(ctx, p, i.Discussion, s)
		if e != nil {
			return e
		}
		if !allowed {
			return fmt.Errorf("incoming discussion changed after operation preparation")
		}
	}
	if s.Head != i.Lease || s.Target != i.Target || s.HeadBranch != i.Branch || s.HeadRepository != i.HeadRepository {
		return fmt.Errorf("live PR head/target/branch lease changed")
	}
	if i.ActorID > 0 || i.Kind == "reply" || i.Kind == "reply_inline" {
		ev, err := readPREvidence(s.Evidence)
		if err != nil {
			return err
		}
		if i.ActorID <= 0 || ev.ViewerID != i.ActorID {
			return fmt.Errorf("authenticated publication actor changed after preparation")
		}
	}
	if i.Kind == "push" || i.Kind == "rewrite" {
		if s.LocalHead != i.Head {
			return fmt.Errorf("publish only the exact checked local head")
		}
	} else if i.Head != s.Head {
		return fmt.Errorf("operation is not bound to the current published head")
	}
	for _, ref := range i.Sources {
		if e = r.validateSource(ctx, p, ref); e != nil {
			return e
		}
	}
	switch i.Kind {
	case "push", "rewrite":
		if len(i.Payload) > 0 {
			return fmt.Errorf("Git publication payload must be empty; remote identity comes from pinned PR")
		}
	case "edit_pr":
		for key, v := range i.Payload {
			if key != "title" && key != "body" {
				return fmt.Errorf("unsupported PR edit field %s", key)
			}
			if _, ok := v.(string); !ok {
				return fmt.Errorf("PR edit fields must be text")
			}
		}
		if len(i.Payload) == 0 {
			return fmt.Errorf("empty PR edit")
		}
	case "reply":
		if i.Replaces != nil {
			if e := r.verifyReplyReplacement(ctx, p, i); e != nil {
				return e
			}
		}
		if len(i.Payload) != 1 {
			return fmt.Errorf("reply payload accepts body only")
		}
	case "reply_inline":
		if i.Replaces != nil {
			if e := r.verifyReplyReplacement(ctx, p, i); e != nil {
				return e
			}
		}
		if len(i.Payload) != 2 || !numeric(stringValue(i.Payload, "comment_id")) {
			return fmt.Errorf("inline reply requires comment_id/body")
		}
	case "resolve_thread":
		return r.threadResolutionGate(ctx, p, i, s)

	case "split_push":
		if len(i.Payload) != 3 {
			return fmt.Errorf("split_push accepts exact branch/head/behavior only")
		}
		b, e := r.splitBoundary(ctx, p, i)
		if e != nil {
			return e
		}
		if b.Branch == s.HeadBranch || b.Base != s.Target {
			return fmt.Errorf("split branch must be new and based on the pinned target")
		}
		paths, e := gitRaw(ctx, r.Root, "diff", "--name-only", "--no-renames", "-z", b.Base, b.Head)
		if e != nil {
			return e
		}
		for _, path := range strings.Split(paths, "\x00") {
			if path != "" && !contains(b.Paths, path) {
				return fmt.Errorf("split branch contains a path outside its accepted ownership: %s", path)
			}
		}
	case "split_pr":
		draft, _ := i.Payload["draft"].(bool)
		if !draft || len(i.Payload) < 6 || len(i.Payload) > 7 || !validText(stringValue(i.Payload, "title")) || stringValue(i.Payload, "base") != s.TargetBranch {
			return fmt.Errorf("split publication requires a draft title/body/head/base and verified push_operation")
		}
		for key := range i.Payload {
			if !contains([]string{"title", "body", "head", "base", "draft", "push_operation", "head_repo"}, key) {
				return fmt.Errorf("unsupported split field %s", key)
			}
		}
		l, e := r.ledger()
		if e != nil {
			return e
		}
		pushed, e := l.Intent(ctx, p.LogicalRun, stringValue(i.Payload, "push_operation"))
		if e != nil {
			l.Close()
			return e
		}
		var state string
		e = l.db.QueryRowContext(ctx, "SELECT state FROM effects WHERE run=? AND id=?", p.LogicalRun, pushed.ID).Scan(&state)
		l.Close()
		if e != nil {
			return e
		}
		repo := strings.Split(s.HeadRepository, "/")
		owner := repo[1]
		if supplied := stringValue(i.Payload, "head_repo"); supplied != "" && supplied != repo[2] {
			return fmt.Errorf("split head_repo differs from the pinned source repository")
		}
		if owner == p.Identity.Owner && repo[2] != p.Identity.Name && stringValue(i.Payload, "head_repo") != repo[2] {
			return fmt.Errorf("same-owner fork splits require the exact head_repo")
		}

		if state != "completed" || pushed.Kind != "split_push" || stringValue(i.Payload, "head") != owner+":"+stringValue(pushed.Payload, "branch") {
			return fmt.Errorf("split PR head differs from its reconciled branch publication")
		}
		if _, e = r.splitBoundary(ctx, p, pushed); e != nil {
			return e
		}

	case "ci_rerun":
		if len(i.Payload) != 2 || !numeric(stringValue(i.Payload, "run_id")) {
			return fmt.Errorf("exact Actions run id required")
		}
		if _, e := rerunAttempt(i.Payload); e != nil {
			return e
		}
		verified := false
		reports, e := r.reports(ctx, p)
		if e != nil {
			return e
		}
		for _, rr := range reports {
			if rr.Occurrence.Kind != "ci" || rr.Claim.State != "complete" {
				continue
			}
			effective, e := r.effectiveReport(ctx, rr.Claim)
			if e != nil {
				return e
			}
			if effective.CI == nil || effective.CI.Head != s.Head || !validText(effective.CI.RerunReason) || len(effective.CI.Evidence) == 0 {
				continue
			}
			for _, op := range effective.Operations {
				if op.ID == i.ID && op.Kind == "ci_rerun" && digest(op.Payload) == digest(i.Payload) {
					verified = true
				}
			}
		}
		if !verified {
			return fmt.Errorf("rerun requires accepted original exact-head CI diagnosis and reason")
		}
		verifier, ok := r.provider().(CIRerunVerifier)
		if !ok {
			return fmt.Errorf("provider cannot verify the rerun head/attempt before dispatch")
		}
		if e = verifier.VerifyRerun(ctx, i); e != nil {
			return e
		}
	case "request_review":
		if len(stringList(i.Payload["reviewers"]))+len(stringList(i.Payload["team_reviewers"])) == 0 {
			return fmt.Errorf("explicit requested reviewers required")
		}
		for key := range i.Payload {
			if key != "reviewers" && key != "team_reviewers" {
				return fmt.Errorf("unsupported review request field")
			}
		}
	case "merge":
		if s.LocalHead != s.Head || s.LocalTree != s.HeadTree {
			return fmt.Errorf("merge requires the reviewed local head and clean tree to be published")
		}
		for key := range i.Payload {
			if !contains([]string{"sha", "merge_method", "commit_title", "commit_message"}, key) {
				return fmt.Errorf("unsupported merge field")
			}
		}
		if stringValue(i.Payload, "sha") != s.Head || !contains([]string{"squash", "merge"}, stringValue(i.Payload, "merge_method")) {
			return fmt.Errorf("exact merge sha and supported method required")
		}
		reports, e := r.reports(ctx, p)
		if e != nil {
			return e
		}
		liveCompletion := false
		for _, rr := range reports {
			if rr.Occurrence.Kind == "completion" && rr.Claim.State == "complete" {
				var pin Snapshot
				if e = readJSON(rr.Claim.Snapshot, &pin); e != nil {
					return e
				}
				liveCompletion = occurrenceFingerprint(pin, rr.Occurrence) == occurrenceFingerprint(s, rr.Occurrence)
			}
		}
		if !liveCompletion {
			return fmt.Errorf("merge follows accepted current managed completion")
		}
		count, e := completedReviewCount(reports, s)
		if e != nil {
			return e
		}
		if count == 0 {
			return fmt.Errorf("merge requires observed current completed review coverage")
		}

		current, _, e := r.coverageExcluding(ctx, p, s, i.ID)
		if e != nil {
			return e
		}
		if !current {
			return fmt.Errorf("merge requires current clean review/verification")
		}

		ci, e := r.provider().ObserveCI(ctx, p.Identity, s.Head)
		if e != nil {
			return e
		}
		if !ci.Success() {
			return fmt.Errorf("merge requires current successful exact-head CI")
		}
		readiness, ok := r.provider().(MergeReadiness)
		if !ok {
			return fmt.Errorf("provider cannot verify live repository merge requirements")
		}
		proof, e := readiness.MergeReady(ctx, p.Identity, s.Head)
		if e != nil {
			return e
		}
		if !proof.Satisfied || proof.Head != s.Head || len(proof.Evidence) == 0 {
			return fmt.Errorf("repository merge requirements are not currently satisfied")
		}
	}
	return nil
}
func (r *Runtime) threadResolutionGate(ctx context.Context, p Plan, i Intent, s Snapshot) error {
	if len(i.Payload) != 2 || stringValue(i.Payload, "thread_id") == "" || stringValue(i.Payload, "reply_operation") == "" {
		return fmt.Errorf("resolution needs exact thread and verified reply operation")
	}
	l, e := r.ledger()
	if e != nil {
		return e
	}
	reply, receipt, e := completedReply(ctx, l, p.LogicalRun, stringValue(i.Payload, "reply_operation"))
	l.Close()
	if e != nil {
		return fmt.Errorf("reply must be reconciled before thread resolution: %w", e)
	}
	ev, e := readPREvidence(s.Evidence)
	if e != nil {
		return e
	}
	return verifyThreadAnswer(ev, i, reply, receipt)
}

func verifyThreadAnswer(ev PREvidence, i, reply Intent, receipt Receipt) error {
	if reply.Kind != "reply_inline" {
		return fmt.Errorf("resolution requires an inline reply to that exact thread")
	}
	root := ""
	for _, thread := range ev.Threads {
		if stringValue(thread, "id") == stringValue(i.Payload, "thread_id") {
			comments, _ := thread["comments"].(map[string]any)
			nodes, _ := comments["nodes"].([]any)
			if len(nodes) > 0 {
				node, _ := nodes[0].(map[string]any)
				root = githubID(node, "databaseId")
			}
		}
	}
	if root == "" || root != stringValue(reply.Payload, "comment_id") {
		return fmt.Errorf("reply does not belong to the exact resolution thread")
	}
	found := false
	for _, comment := range ev.Inline {
		user, _ := comment["user"].(map[string]any)
		if reply.ActorID > 0 && ev.ViewerID == reply.ActorID && githubID(comment, "id") == githubID(receipt.Result, "id") && githubID(user, "id") == strconv.FormatInt(reply.ActorID, 10) && stringValue(user, "login") == ev.Viewer && stringValue(comment, "body") == stringValue(reply.Payload, "body") && githubID(comment, "in_reply_to_id") == root {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("published inline answer is no longer present")
	}
	return nil
}

func numeric(s string) bool { n, e := strconv.ParseUint(s, 10, 64); return e == nil && n > 0 }

func verifyOwnedParent(root, path string, deleted bool) error {
	root = filepath.Clean(root)
	for dir := filepath.Dir(filepath.Join(root, path)); ; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil {
			if !deleted || !os.IsNotExist(err) || dir == root {
				return err
			}
		} else if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("owned change ancestor is not a real directory")
		}
		if dir == root {
			return nil
		}
		if filepath.Dir(dir) == dir {
			return fmt.Errorf("owned change escaped repository")
		}
	}
}

func (r *Runtime) publish(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	unlock, err := r.lockPublication(ctx)
	if err != nil {
		return output(), err
	}
	defer unlock()
	return r.publishLocked(ctx, p, in)
}
func (r *Runtime) publishLocked(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	out := output()
	l, e := r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	i, e := l.Intent(ctx, p.LogicalRun, in.Intent)
	if e != nil {
		return out, e
	}
	var state, path string
	if e = l.db.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state, &path); e != nil {
		return out, e
	}
	if state == "completed" {
		out.Report = path
		var receipt Receipt
		if e = readJSON(path, &receipt); e != nil {
			return out, e
		}
		out.Receipt = &receipt
		return out, nil
	}
	if state == "disposed" {
		if e = r.verifyReviewRequestDisposition(ctx, l, i); e != nil {
			return out, e
		}
		out.Deferred, out.Settled = true, true
		return out, nil
	}
	if state == "dispatched" || state == "unknown" {
		outcome, receipt, e := r.reconcileIntent(ctx, i)
		if e != nil {
			if provider, ok := r.provider().(*GitHub); ok {
				if err := provider.recordObservationFailure(i); err != nil {
					return out, err
				}
			}
			return out, e
		}
		if outcome == "completed" {
			return r.completeEffect(ctx, l, i, receipt)
		}
		return out, fmt.Errorf("uncertain %s requires read-only reconciliation", i.Kind)
	}
	if state != "prepared" {
		return out, fmt.Errorf("publication state %s cannot dispatch", state)
	}
	if e = r.publicationGate(ctx, p, i, in.Claim); e != nil {
		return out, e
	}
	if i.Kind == "ci_rerun" {
		if e = l.Reserve(ctx, p.LogicalRun, "ci_rerun", i.Head, i.ID, "verified transient CI rerun"); e != nil {
			return out, e
		}
	}
	if _, e = l.db.ExecContext(ctx, "UPDATE effects SET state='dispatched' WHERE run=? AND id=?", p.LogicalRun, i.ID); e != nil {
		return out, e
	}
	var receipt Receipt
	if i.Kind == "push" || i.Kind == "rewrite" || i.Kind == "split_push" {
		receipt, e = r.push(ctx, p, i)
	} else {
		receipt, e = r.provider().Execute(ctx, i)
	}
	if e != nil {
		return out, e
	}
	return r.completeEffect(ctx, l, i, receipt)
}
func (r *Runtime) completeEffect(ctx context.Context, l *Ledger, i Intent, receipt Receipt) (ActionOutput, error) {
	out := output()
	if receipt.ID != i.ID || receipt.PR != i.PR.URL || receipt.Head != i.Head || receipt.Kind != i.Kind || len(receipt.Evidence) == 0 {
		return out, fmt.Errorf("publication receipt identity/evidence mismatch")
	}
	path, e := artifact(r.Root, receipt)
	if e != nil {
		return out, e
	}
	e = l.transaction(ctx, func(tx sqlRW) error {
		var state, saved string
		if e := tx.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state, &saved); e != nil {
			return e
		}
		if state == "completed" {
			if saved != path {
				return fmt.Errorf("completed publication receipt is immutable")
			}
			return nil
		}
		if i.Kind == "ci_rerun" {
			if _, e := tx.ExecContext(ctx, "INSERT INTO ci_windows(run,head,operation,started) VALUES(?,?,?,?) ON CONFLICT(run,operation) DO NOTHING", i.Run, i.Head, i.ID, r.now().Format(time.RFC3339Nano)); e != nil {
				return e
			}
		}
		_, e := tx.ExecContext(ctx, "UPDATE effects SET state='completed',receipt=? WHERE run=? AND id=?", path, i.Run, i.ID)
		return e
	})
	out.Report = path
	out.Receipt = &receipt
	out.Settled = e == nil
	return out, e
}
func (r *Runtime) push(ctx context.Context, p Plan, i Intent) (Receipt, error) {
	remote := "https://" + i.HeadRepository + ".git"
	if i.HeadRepository == "" || !strings.HasPrefix(i.HeadRepository, p.Identity.Host+"/") {
		return Receipt{}, fmt.Errorf("head repository must be pinned to PR host")
	}
	branch, head, lease := i.Branch, i.Head, i.Lease
	if i.Kind == "split_push" {
		branch = stringValue(i.Payload, "branch")
		head = stringValue(i.Payload, "head")
		lease = ""
	}
	args := []string{"push", "--porcelain"}
	if i.Kind == "split_push" {
		args = append(args, "--force-with-lease=refs/heads/"+branch+":")
	} else if i.Kind == "rewrite" {
		args = append(args, "--force-with-lease=refs/heads/"+branch+":"+lease)
	} else {
		if _, e := git(ctx, r.Root, "merge-base", "--is-ancestor", i.Lease, i.Head); e != nil {
			return Receipt{}, fmt.Errorf("non-rewrite push requires fast-forward ancestry")
		}
		args = append(args, "--force-with-lease=refs/heads/"+branch+":"+lease)
	}
	args = append(args, remote, head+":refs/heads/"+branch)
	if _, e := git(ctx, r.Root, args...); e != nil {
		return Receipt{}, e
	}
	state, receipt, e := r.reconcileIntent(ctx, i)
	if e != nil {
		return receipt, e
	}
	if state != "completed" {
		return receipt, fmt.Errorf("push not verified")
	}
	return receipt, nil
}
func (r *Runtime) reconcileIntent(ctx context.Context, i Intent) (string, Receipt, error) {
	if i.Kind != "push" && i.Kind != "rewrite" && i.Kind != "split_push" {
		return r.provider().Reconcile(ctx, i)
	}
	branch, head, lease := i.Branch, i.Head, i.Lease
	if i.Kind == "split_push" {
		branch = stringValue(i.Payload, "branch")
		head = stringValue(i.Payload, "head")
		lease = ""
	}
	raw, e := git(ctx, r.Root, "ls-remote", "--refs", "https://"+i.HeadRepository+".git", "refs/heads/"+branch)
	if e != nil {
		return "unknown", Receipt{}, e
	}
	fields := strings.Fields(raw)
	receipt := Receipt{i.ID, i.Kind, i.PR.URL, i.Head, jsonObject(map[string]string{"remote_ref": raw, "branch": branch, "sha": head}), []string{"git ls-remote exact branch"}}
	if len(fields) == 2 && fields[0] == head {
		return "completed", receipt, nil
	}
	if i.Kind == "split_push" && len(fields) == 0 {
		return "no_effect", receipt, nil
	}
	if len(fields) == 2 && fields[0] == lease {
		return "no_effect", receipt, nil
	}
	return "unknown", receipt, nil
}

func (r *Runtime) reconcileCompletedIntent(ctx context.Context, l *Ledger, i Intent) (string, Receipt, error) {
	var path string
	if err := l.db.QueryRowContext(ctx, "SELECT receipt FROM effects WHERE run=? AND id=? AND state='completed'", i.Run, i.ID).Scan(&path); err != nil {
		return "unknown", Receipt{}, err
	}
	var saved Receipt
	if err := readJSON(path, &saved); err != nil {
		return "unknown", saved, err
	}
	if contains([]string{"reply", "reply_inline"}, i.Kind) {
		if settled, err := r.verifyReplySettlement(ctx, l, i); settled || err != nil {
			if err != nil {
				return "unknown", saved, err
			}
			return "completed", saved, nil
		}
		ev, err := r.provider().Capture(ctx, i.PR)
		if err != nil {
			return "unknown", saved, err
		}
		if saved.ID != i.ID || saved.Kind != i.Kind || saved.PR != i.PR.URL || saved.Head != i.Head || !numeric(githubID(saved.Result, "id")) {
			return "unknown", saved, fmt.Errorf("exact historical reply receipt required")
		}
		for _, comment := range matchingReplies(ev, i) {
			if githubID(comment, "id") == githubID(saved.Result, "id") {
				return "completed", saved, nil
			}
		}
		return "unknown", saved, fmt.Errorf("current reply does not match its historical receipt")
	}
	state, _, err := r.reconcileIntent(ctx, i)
	return state, saved, err
}
func (r *Runtime) validateCleanup(ctx context.Context, p Plan, initial, live Snapshot, c *Cleanup) error {
	if c == nil || len(c.BoundaryReasons) == 0 || len(c.Validation) == 0 || c.ReviewedHead != initial.LocalHead || c.FinalHead != live.LocalHead || c.FinalTree != live.LocalTree {
		return fmt.Errorf("cleanup needs complete series, boundaries, validation and exact reviewed/final pins")
	}
	series, e := git(ctx, r.Root, "rev-list", initial.Target+".."+initial.LocalHead)
	if e != nil {
		return e
	}
	commits := strings.Fields(series)
	if len(c.Commits) != len(commits) {
		return fmt.Errorf("cleanup must map the complete exact commit series")
	}
	for _, sha := range commits {
		destination := c.Commits[sha]
		if !validSHA(destination) {
			return fmt.Errorf("cleanup omitted destination for commit %s", sha)
		}
		if _, e = git(ctx, r.Root, "merge-base", "--is-ancestor", destination, c.FinalHead); e != nil {
			return fmt.Errorf("cleanup destination is outside the resulting series")
		}
	}
	if initial.LocalTree != live.LocalTree {
		return fmt.Errorf("commit cleanup changed the reviewed final tree")
	}
	for _, ref := range c.Validation {
		if e = r.validateSource(ctx, p, ref); e != nil {
			return e
		}
	}
	for _, reason := range c.BoundaryReasons {
		if !validText(reason) {
			return fmt.Errorf("empty cleanup boundary reason")
		}
	}
	if initial.LocalHead != live.LocalHead {
		recovery, e := git(ctx, r.Root, "rev-parse", "--verify", c.RecoveryRef+"^{commit}")
		if e != nil || recovery != initial.LocalHead {
			return fmt.Errorf("history cleanup requires an exact original-head recovery ref")
		}
		if c.Outcome == "no_op" {
			return fmt.Errorf("history changes cannot be reported as no-op")
		}
	}
	if c.Outcome == "blocked" {
		return fmt.Errorf("blocked cleanup cannot be complete")
	}
	if c.Outcome == "prepared" {
		return nil
	}
	if !contains([]string{"no_op", "published"}, c.Outcome) || live.LocalHead != live.Head {
		return fmt.Errorf("cleanup needs published matching final head")
	}
	if c.Outcome == "published" {
		l, e := r.ledger()
		if e != nil {
			return e
		}
		defer l.Close()
		var count int
		e = l.db.QueryRowContext(ctx, "SELECT count(*) FROM effects WHERE run=? AND state='completed' AND receipt=? AND json_extract(intent,'$.head')=? AND json_extract(intent,'$.kind') IN ('push','rewrite')", p.LogicalRun, c.Receipt, c.FinalHead).Scan(&count)
		if e != nil {
			return e
		}
		if count != 1 {
			return fmt.Errorf("published cleanup needs the reconciled exact-head push receipt")
		}
	}
	return nil
}
func (r *Runtime) commit(ctx context.Context, p Plan, in ActionInput) (ActionOutput, error) {
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return output(), e
	}
	l.Close()
	o, e := p.Occurrence(c.Occurrence)
	if e != nil {
		return output(), e
	}
	out := claimOutput(c)
	if c.State != "complete" || !mutates(o) || o.Kind == "cleanup" {
		return out, nil
	}
	status, e := gitStatus(ctx, r.Root)
	if e != nil || status == "" {
		return out, e
	}
	// A user assignment to iterate authorizes focused local commits. The complete
	// original report supplies the repository-native message; no shell is used.
	report, e := r.effectiveReport(ctx, c)
	if e != nil {
		return out, e
	}
	var initial Snapshot
	if e = readJSON(c.Snapshot, &initial); e != nil {
		return out, e
	}
	if len(report.Changes) == 0 {
		current, e := r.live(ctx, p)
		if e != nil {
			return out, e
		}
		if current.Diff == initial.Diff && current.LocalHead == initial.LocalHead {
			return out, nil
		}
		return out, fmt.Errorf("new local edits require the worker's exact changed-file manifest")
	}
	if !validText(report.CommitMessage) {
		return out, fmt.Errorf("changed files require a prepared repository-native commit message")
	}
	parent, e := git(ctx, r.Root, "rev-parse", "HEAD")
	if e != nil {
		return out, e
	}
	if parent != initial.LocalHead {
		return out, fmt.Errorf("local head changed before focused commit")
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, change := range report.Changes {
		path := change.Path
		if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.HasPrefix(path, "../") || path == ".." || path == "." || strings.HasPrefix(path, ".git/") || path == ".git" || strings.HasPrefix(path, ".agent-workflow/") || strings.HasPrefix(path, "workflows/.pr-fix/") || seen[path] {
			return out, fmt.Errorf("invalid or repeated owned change path %q", path)
		}
		if e := verifyOwnedParent(r.Root, path, change.After == "deleted"); e != nil {
			return out, fmt.Errorf("owned change parent must exist without symlink aliases: %s", path)
		}
		if len(o.Adapter.Writes) > 0 {
			owned := false
			for _, prefix := range o.Adapter.Writes {
				owned = owned || prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/")
			}
			if !owned {
				return out, fmt.Errorf("manifest path %s is outside the adapter's declared write ownership", path)
			}
		}
		seen[path] = true
		before, e := runCommand(ctx, r.Root, nil, "git", "cat-file", "blob", parent+":"+path)
		beforeHash := "absent"
		if e == nil {
			beforeHash = rawHash(before)
		} else {
			if _, e = git(ctx, r.Root, "cat-file", "-e", parent+":"+path); e == nil {
				return out, fmt.Errorf("cannot capture original blob")
			}
		}
		if beforeHash != change.Before {
			return out, fmt.Errorf("owned change original blob differs for %s", path)
		}
		afterHash, e := fileHash(filepath.Join(r.Root, path))
		if e != nil {
			return out, e
		}
		if afterHash != change.After {
			return out, fmt.Errorf("owned file changed after worker report: %s", path)
		}
		paths = append(paths, path)
	}
	staged, e := gitRaw(ctx, r.Root, "diff", "--cached", "--name-only", "-z")
	if e != nil {
		return out, e
	}
	for _, path := range strings.Split(staged, "\x00") {
		if path != "" && !seen[path] {
			return out, fmt.Errorf("preserving unrelated staged file %s", path)
		}
	}
	l, e = r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	if _, e = l.db.ExecContext(ctx, "INSERT INTO local_effects(run,claim,parent,message) VALUES(?,?,?,?) ON CONFLICT(run,claim) DO NOTHING", p.LogicalRun, c.ID, parent, report.CommitMessage); e != nil {
		return out, e
	}
	if _, e = git(ctx, r.Root, append([]string{"--literal-pathspecs", "add", "--"}, paths...)...); e != nil {
		return out, e
	}
	for _, change := range report.Changes {
		if change.After == "deleted" {
			if _, e = git(ctx, r.Root, "cat-file", "-e", ":"+change.Path); e == nil {
				return out, fmt.Errorf("owned deletion is not staged")
			}
			continue
		}
		blob, err := runCommand(ctx, r.Root, nil, "git", "cat-file", "blob", ":"+change.Path)
		if err != nil {
			return out, err
		}
		if rawHash(blob) != change.After {
			return out, fmt.Errorf("staged owned blob changed: %s", change.Path)
		}
	}
	staged, e = gitRaw(ctx, r.Root, "diff", "--cached", "--name-only", "-z")
	if e != nil {
		return out, e
	}
	for _, path := range strings.Split(staged, "\x00") {
		if path != "" && !seen[path] {
			return out, fmt.Errorf("staged path is outside the accepted manifest: %s", path)
		}
	}
	tree, e := git(ctx, r.Root, "write-tree")
	if e != nil {
		return out, e
	}
	if _, e = l.db.ExecContext(ctx, "UPDATE local_effects SET tree=? WHERE run=? AND claim=?", tree, p.LogicalRun, c.ID); e != nil {
		return out, e
	}
	if _, e = git(ctx, r.Root, "commit", "-m", report.CommitMessage); e != nil {
		return out, e
	}
	head, e := git(ctx, r.Root, "rev-parse", "HEAD")
	if e != nil {
		return out, e
	}
	actualTree, e := git(ctx, r.Root, "rev-parse", "HEAD^{tree}")
	if e != nil {
		return out, e
	}
	actualParent, e := git(ctx, r.Root, "rev-parse", "HEAD^")
	if e != nil {
		return out, e
	}
	if actualTree != tree || actualParent != parent {
		return out, fmt.Errorf("commit outcome differs from the focused prepared tree/parent; reconcile before further edits")
	}
	if _, e = l.db.ExecContext(ctx, "UPDATE local_effects SET head=? WHERE run=? AND claim=?", head, p.LogicalRun, c.ID); e != nil {
		return out, e
	}
	return out, nil
}

// Read-only probes recover effects for which an engine checkpoint was lost.
func (r *Runtime) reconcile(ctx context.Context, name string, ac workflows.ActionContext, in ActionInput) (string, ActionOutput, error) {
	r = r.observer(ctx)
	if err := ctx.Err(); err != nil {
		return "unknown", output(), err
	}
	out := output()
	p, e := r.plan(in)
	if e != nil {
		return "unknown", out, e
	}
	if name == "review_admit" {
		return "no_effect", out, nil // an uncheckpointed admission safely repeats its idempotent reservation
	}
	if name == "worker_acknowledge" {
		return r.workerAcknowledge(ctx, p, ac, in, false)
	}
	if name == "publish" {
		l, e := r.ledger()
		if e != nil {
			return "unknown", out, e
		}
		i, e := l.Intent(ctx, p.LogicalRun, in.Intent)
		if e != nil {
			l.Close()
			return "unknown", out, e
		}
		var status, path string
		e = l.db.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&status, &path)
		if e == nil && status == "completed" {
			l.Close()
			// The application already verified this publication. Recover that exact
			// historical result without identifying a new post-baseline creation.
			var receipt Receipt
			if e = readJSON(path, &receipt); e != nil {
				return "unknown", out, e
			}
			out.Report, out.Receipt, out.Settled = path, &receipt, true
			return "completed", out, nil
		}
		if e == nil && status == "disposed" {
			e = r.verifyReviewRequestDisposition(ctx, l, i)
			l.Close()
			out.Deferred, out.Settled = true, true
			return "completed", out, e
		}
		l.Close()
		if e != nil {
			return "unknown", out, e
		}
		state, receipt, e := r.reconcileIntent(ctx, i)
		if state == "completed" && e == nil {
			out.Receipt = &receipt
			out.Settled = true
			return "completed", out, nil
		}
		if status == "dispatched" || status == "unknown" {
			return "unknown", out, e
		}
		return state, out, e
	}
	if name == "review_request_inspect" || name == "reply_inspect" {
		return "no_effect", out, nil
	}
	if name == "reply_dispose" {
		l, err := r.ledger()
		if err != nil {
			return "unknown", out, err
		}
		defer l.Close()
		i, err := l.Intent(ctx, p.LogicalRun, in.Intent)
		if err != nil {
			return "unknown", out, err
		}
		return r.reconcileReplyDisposition(ctx, l, i, ac, in)
	}
	if name == "review_request_dispose" {
		l, err := r.ledger()
		if err != nil {
			return "unknown", out, err
		}
		defer l.Close()
		err = l.db.QueryRowContext(ctx, "SELECT artifact FROM review_request_dispositions WHERE run=? AND id=?", p.LogicalRun, in.Intent).Scan(&out.Report)
		if errors.Is(err, sql.ErrNoRows) {
			return "no_effect", out, nil
		}
		if err != nil {
			return "unknown", out, err
		}
		var retained retainedReviewRequestDisposition
		if err = readJSON(out.Report, &retained); err != nil {
			return "unknown", out, err
		}
		if retained.Operation != ac.OperationKey || retained.Claim != in.Claim {
			return "no_effect", output(), nil
		}
		out.Settled = err == nil
		return "completed", out, err
	}
	if name == "worker_recover" || name == "worker_adopt" {
		l, c, e := r.record(ctx, p, in)
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		worker := in.Worker
		if name == "worker_recover" {
			worker = "prfix_" + digest([]any{p.LogicalRun, c.ID, in.Round, ac.OperationKey})[:32]
		}
		var engine, claim string
		var round int
		e = l.db.QueryRowContext(ctx, "SELECT engine,claim,round FROM worker_links WHERE id=?", worker).Scan(&engine, &claim, &round)
		if errors.Is(e, sql.ErrNoRows) {
			return "no_effect", out, nil
		}
		if e != nil {
			return "unknown", out, e
		}
		if engine != c.Engine || claim != c.ID || round != in.Round {
			return "unknown", out, fmt.Errorf("recovery identity mismatch")
		}
		_, e = r.Workers.Get(ctx, engine, worker)
		if errors.Is(e, sql.ErrNoRows) {
			return "no_effect", out, nil
		}
		if e != nil {
			return "unknown", out, e
		}
		if name == "worker_adopt" {
			adopted := c.Worker
			if in.Round > 0 {
				e = l.db.QueryRowContext(ctx, "SELECT worker FROM worker_rounds WHERE run=? AND claim=? AND round=?", p.LogicalRun, c.ID, in.Round).Scan(&adopted)
				if errors.Is(e, sql.ErrNoRows) {
					return "no_effect", out, nil
				}
				if e != nil {
					return "unknown", out, e
				}
			}
			if adopted != worker {
				return "no_effect", out, nil
			}
		}
		out = claimOutput(c)
		out.Worker = worker
		return "completed", out, nil
	}

	if name == "worker_spawn" || name == "worker_send" || name == "finish" || name == "commit" {
		l, c, e := r.record(ctx, p, in)
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		out = claimOutput(c)
		switch name {
		case "worker_spawn":
			_, e = r.Workers.Get(ctx, c.Engine, c.Worker)
			if errors.Is(e, sql.ErrNoRows) {
				return "no_effect", out, nil
			}
			if e == nil {
				return "completed", out, nil
			}
		case "worker_send":
			_, e = r.message(ctx, c, in.Round)
			if errors.Is(e, sql.ErrNoRows) {
				return "no_effect", out, nil
			}
			if e == nil {
				return "completed", out, nil
			}
		case "finish":
			var status string
			e = l.db.QueryRowContext(ctx, "SELECT status FROM submissions WHERE run=? AND claim=? AND round=?", p.LogicalRun, c.ID, in.Round).Scan(&status)
			if errors.Is(e, sql.ErrNoRows) {
				return "no_effect", out, nil
			}
			if e == nil {
				return "completed", out, nil
			}
		case "commit":
			var parent, tree, message, head string
			e = l.db.QueryRowContext(ctx, "SELECT parent,tree,message,head FROM local_effects WHERE run=? AND claim=?", p.LogicalRun, c.ID).Scan(&parent, &tree, &message, &head)
			if errors.Is(e, sql.ErrNoRows) {
				return "no_effect", out, nil
			}
			if e != nil {
				return "unknown", out, e
			}
			current, e := git(ctx, r.Root, "rev-parse", "HEAD")
			if e != nil {
				return "unknown", out, e
			}
			if head != "" && current == head {
				return "completed", out, nil
			}
			actualTree, e := git(ctx, r.Root, "rev-parse", "HEAD^{tree}")
			if e != nil {
				return "unknown", out, e
			}
			description, e := git(ctx, r.Root, "log", "-1", "--format=%P%x00%B")
			if e != nil {
				return "unknown", out, e
			}
			parts := strings.SplitN(description, "\x00", 2)
			if len(parts) == 2 && parts[0] == parent && strings.TrimSpace(parts[1]) == strings.TrimSpace(message) && actualTree == tree && tree != "" {
				return "completed", out, nil
			}
			return "unknown", out, nil
		}
		return "unknown", out, e
	}
	if name == "claim" {
		l, e := r.ledger()
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		var id string
		e = l.db.QueryRowContext(ctx, "SELECT id FROM claims WHERE run=? AND operation=?", p.LogicalRun, ac.OperationKey).Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			return "no_effect", out, nil
		}
		if e != nil {
			return "unknown", out, e
		}
		c, e := getClaim(ctx, l.db, p.LogicalRun, id)
		if e != nil {
			return "unknown", out, e
		}
		out = claimOutput(c)
		if c.State == "claimed" {
			b, e := os.ReadFile(c.Assignment)
			if e != nil {
				return "unknown", out, e
			}
			out.Prompt = string(b)
		}
		return "completed", out, nil
	}
	if name == "reserve" {
		l, e := r.ledger()
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		var count int
		e = l.db.QueryRowContext(ctx, "SELECT count(*) FROM reservations WHERE run=? AND kind=? AND operation=?", p.LogicalRun, in.Kind, ac.OperationKey).Scan(&count)
		if e != nil {
			return "unknown", out, e
		}
		if count > 0 {
			return "completed", out, nil
		}
		return "no_effect", out, nil
	}
	if name == "receipt" {
		l, e := r.ledger()
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		var state, path string
		i, e := l.Intent(ctx, p.LogicalRun, in.Intent)
		if e != nil {
			return "unknown", out, e
		}
		e = l.db.QueryRowContext(ctx, "SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state, &path)
		if e != nil {
			return "unknown", out, e
		}
		if state == "completed" {
			var receipt Receipt
			if e = readJSON(path, &receipt); e != nil {
				return "unknown", out, e
			}
			out.Receipt = &receipt
			out.Report = path
			out.Settled = true
			return "completed", out, nil
		}
		return "no_effect", out, nil
	}
	if name == "close" {
		l, e := r.ledger()
		if e != nil {
			return "unknown", out, e
		}
		defer l.Close()
		var status string
		if e = l.db.QueryRowContext(ctx, "SELECT status FROM logical_runs WHERE id=?", p.LogicalRun).Scan(&status); e != nil {
			return "unknown", out, e
		}
		if status != "complete" {
			return "no_effect", out, nil
		}
		engine, e := r.origin(ctx, ac)
		if e != nil {
			return "unknown", out, e
		}
		rows, e := l.db.QueryContext(ctx, "SELECT receipt FROM effects WHERE run=? AND state='completed' ORDER BY rowid", p.LogicalRun)
		if e != nil {
			return "unknown", out, e
		}
		refs := []string{}
		for rows.Next() {
			var path string
			if e = rows.Scan(&path); e != nil {
				rows.Close()
				return "unknown", out, e
			}
			refs = append(refs, path)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return "unknown", out, e
		}
		report := map[string]any{"logical_run": p.LogicalRun, "engine_run": engine, "publication_receipts": refs, "private": true}
		out.Report = filepath.Join(r.Root, ".agent-workflow", "pr-fix", "artifacts", digest(report)+".json")
		var original map[string]any
		if e = readJSON(out.Report, &original); e != nil {
			return "unknown", out, e
		}
		out.Settled = true
		return "completed", out, nil
	}
	// Claims/reservations commit atomically under stable IDs. Absence of a saved
	// receipt alone is not proof of absence; never repeat an uncertain mutation.
	return "unknown", out, nil
}

func validSHA(s string) bool {
	return regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(s)
}

func rawHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func fileHash(path string) (string, error) {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return "deleted", nil
	}
	if e != nil {
		return "", e
	}
	var data []byte
	if st.Mode()&os.ModeSymlink != 0 {
		target, e := os.Readlink(path)
		if e != nil {
			return "", e
		}
		data = []byte(target)
	} else {
		data, e = os.ReadFile(path)
		if e != nil {
			return "", e
		}
	}
	return rawHash(data), nil
}
