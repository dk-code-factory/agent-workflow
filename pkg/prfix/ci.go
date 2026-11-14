package prfix

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type CIClassification struct {
	CheckID   string `json:"check_id"`
	RunID     string `json:"run_id"`
	JobID     string `json:"job_id"`
	Attempt   int    `json:"attempt"`
	Kind      string `json:"kind"`
	Workflow  string `json:"workflow"`
	Job       string `json:"job"`
	Test      string `json:"test,omitempty"`
	Signature string `json:"signature,omitempty"`
	Issue     string `json:"issue,omitempty"`
	Rule      string `json:"rule,omitempty"`
}
type CIProof struct {
	Head            string             `json:"head"`
	CheckID         string             `json:"check_id"`
	RunID           string             `json:"run_id"`
	JobID           string             `json:"job_id"`
	Attempt         int                `json:"attempt"`
	Classifications []CIClassification `json:"classifications"`
	Evidence        []string           `json:"evidence"`
	Exempt          bool               `json:"exempt"`
}
type CIVerifier interface {
	VerifyCI(context.Context, Identity, string, []CIClassification, map[string]map[string]any) (CIProof, error)
}

func (r *Runtime) classifyCI(ctx context.Context, p Plan, in ActionInput) (out ActionOutput, err error) {
	out = output()
	l, c, e := r.record(ctx, p, in)
	if e != nil {
		return out, e
	}
	l.Close()
	o, err := p.Occurrence(c.Occurrence)
	if err != nil {
		return out, err
	}
	options := o.Call.Invocation.Options
	if o.Call.Configuration != nil {
		options = o.Call.Configuration.Options
	}
	if e := validateCIOptions(options); e != nil {
		return out, e
	}
	timeout := ciSeconds(options, "classification_timeout_seconds", 60*time.Second)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A canceled Git read can report an exit error rather than the context error.
	// Retain both so callers still recognize the classification deadline.
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
	}()
	if c.State != "complete" {
		return out, fmt.Errorf("CI classification requires accepted native execution/checklist")
	}
	s, e := r.live(ctx, p)
	if e != nil {
		return out, e
	}
	path, e := r.latestCI(ctx, p, s.Head)
	if e != nil {
		return out, e
	}
	var obs CIObservation
	if e = readJSON(path, &obs); e != nil {
		return out, e
	}
	report, e := r.effectiveReport(ctx, c)
	if e != nil {
		return out, e
	}
	if report.CI == nil || report.CI.Head != s.Head || len(report.CI.Evidence) == 0 {
		return out, fmt.Errorf("classification requires exact-head CI report and original evidence")
	}
	for _, ref := range report.CI.Evidence {
		if e = r.validateSource(ctx, p, ref); e != nil {
			return out, e
		}
	}
	for _, check := range obs.Checks {
		if !contains(report.CI.Checks, check.ID) {
			return out, fmt.Errorf("CI classification omitted exact check %s", check.ID)
		}
	}
	grouped := map[string][]CIClassification{}
	for _, classification := range report.CI.Classifications {
		grouped[classification.CheckID] = append(grouped[classification.CheckID], classification)
	}
	rules := map[string]map[string]any{}
	settings, _ := p.Selection.Settings["ci_exceptions"].(map[string]any)
	entries, _ := settings["entries"].([]any)
	for _, entry := range entries {
		value, _ := entry.(map[string]any)
		rules[stringValue(value, "id")] = value
	}
	for _, check := range obs.Checks {
		classes := grouped[check.ID]
		if len(classes) == 0 {
			continue
		}
		if check.Status != "completed" || contains([]string{"success", "neutral", "skipped"}, check.Conclusion) {
			return out, fmt.Errorf("CI exceptions cannot replace pending or successful checks")
		}
		verifier, ok := r.provider().(CIVerifier)
		if !ok {
			return out, fmt.Errorf("provider cannot verify exact-attempt CI classifications")
		}
		proof, e := verifier.VerifyCI(ctx, p.Identity, s.Head, classes, rules)
		if e != nil {
			return out, e
		}
		if !proof.Exempt || proof.Head != s.Head || proof.CheckID != check.ID || proof.Attempt < 1 || len(proof.Evidence) == 0 {
			return out, fmt.Errorf("CI exception proof identity/evidence mismatch")
		}
		obs.Verified = append(obs.Verified, proof)
	}
	path, e = artifact(r.Root, obs)
	if e != nil {
		return out, e
	}
	l, e = r.ledger()
	if e != nil {
		return out, e
	}
	defer l.Close()
	if _, e = l.db.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, "engine://"+c.Engine+"/workers/"+c.Worker+"/messages/"+c.Message, "ci:"+s.Head, path); e != nil {
		return out, e
	}
	out.Report = path
	if obs.Success() {
		out.Settled = true
		return out, nil
	}
	if report.CI.Repair {
		if !validText(report.CI.RepairReason) {
			return out, fmt.Errorf("CI repair requires a sourced concrete failure diagnosis")
		}
		out.Repair = true
		return out, nil
	}
	// A prepared rerun may be dispatched by a later selected publication stage.
	// Unknown/unclassified failures remain blockers at the live completion gate.
	hasRerun := false
	for _, op := range report.Operations {
		hasRerun = hasRerun || op.Kind == "ci_rerun"
	}
	if !hasRerun || !validText(report.CI.RerunReason) {
		return out, fmt.Errorf("unknown CI failure blocks repair and completion")
	}
	return out, nil
}
func (g *GitHub) VerifyCI(ctx context.Context, id Identity, head string, classes []CIClassification, rules map[string]map[string]any) (CIProof, error) {
	if len(classes) == 0 {
		return CIProof{}, fmt.Errorf("CI candidates required")
	}
	first := classes[0]
	proof := CIProof{Head: head, CheckID: first.CheckID, RunID: first.RunID, JobID: first.JobID, Attempt: first.Attempt, Classifications: classes}
	if !numeric(first.CheckID) || !numeric(first.RunID) || !numeric(first.JobID) || first.Attempt < 1 {
		return proof, fmt.Errorf("exact check/run/job/attempt required")
	}
	var run struct {
		Head    string `json:"head_sha"`
		Attempt int    `json:"run_attempt"`
		Path    string `json:"path"`
	}
	prefix := "repos/" + id.Owner + "/" + id.Name
	if e := g.api(ctx, id, "GET", prefix+"/actions/runs/"+first.RunID, nil, &run); e != nil {
		return proof, e
	}
	if run.Head != head || run.Attempt != first.Attempt {
		return proof, fmt.Errorf("CI evidence belongs to another head or attempt")
	}
	var job struct {
		Steps      []ciStep `json:"steps"`
		RunID      int64    `json:"run_id"`
		Attempt    int      `json:"run_attempt"`
		Name       string   `json:"name"`
		Conclusion string   `json:"conclusion"`
		CheckURL   string   `json:"check_run_url"`
	}
	if e := g.api(ctx, id, "GET", prefix+"/actions/jobs/"+first.JobID, nil, &job); e != nil {
		return proof, e
	}
	if strconv.FormatInt(job.RunID, 10) != first.RunID || job.Attempt != first.Attempt || !strings.HasSuffix(job.CheckURL, "/check-runs/"+first.CheckID) || job.Conclusion != "failure" {
		return proof, fmt.Errorf("failed job does not match the exact check/run/attempt")
	}
	logs, e := runCommand(ctx, g.Root, nil, "gh", "run", "view", first.RunID, "--job", first.JobID, "--attempt", strconv.Itoa(first.Attempt), "--log-failed", "--repo", id.Repository)
	if e != nil {
		return proof, e
	}
	path, e := artifact(g.Root, map[string]any{"head": head, "run": run, "job": job, "logs": string(logs)})
	if e != nil {
		return proof, e
	}
	proof.Evidence = []string{path}
	logText := string(logs)
	bodyLines := []string{}
	for _, line := range strings.Split(logText, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) == 3 {
			line = fields[2]
		}
		bodyLines = append(bodyLines, line)
	}
	logBody := strings.Join(bodyLines, "\n")
	covered := map[string]bool{}
	pipeline := false
	for _, c := range classes {
		if c.CheckID != first.CheckID || c.RunID != first.RunID || c.JobID != first.JobID || c.Attempt != first.Attempt || c.Workflow != strings.Split(run.Path, "@")[0] || c.Job != job.Name {
			return proof, fmt.Errorf("classification scope differs from observed attempt")
		}
		if c.Kind == "exception" {
			rule, ok := rules[c.Rule]
			if !ok || stringValue(rule, "workflow") != c.Workflow {
				return proof, fmt.Errorf("CI exception not present in original scoped settings")
			}
			if name := stringValue(rule, "job"); name != "" && name != c.Job {
				return proof, fmt.Errorf("CI exception job mismatch")
			}
			if rule["kind"] == "pipeline" {
				pipeline = true
				continue
			}
			if stringValue(rule, "test") != c.Test || stringValue(rule, "signature") != c.Signature {
				return proof, fmt.Errorf("CI test exception differs from saved policy")
			}
		} else if c.Kind == "flake" {
			u, e := url.Parse(c.Issue)
			if e != nil || u.Scheme != "https" || u.Host != id.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
				return proof, fmt.Errorf("canonical issue evidence required")
			}
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) != 4 || !strings.EqualFold(parts[0], id.Owner) || !strings.EqualFold(parts[1], id.Name) || parts[2] != "issues" || !numeric(parts[3]) {
				return proof, fmt.Errorf("open issue URL in the exact PR repository required")
			}
			var issue struct {
				State       string    `json:"state"`
				Title       string    `json:"title"`
				Body        string    `json:"body"`
				URL         string    `json:"html_url"`
				PullRequest *struct{} `json:"pull_request"`
			}
			if e = g.api(ctx, id, "GET", "repos/"+parts[0]+"/"+parts[1]+"/issues/"+parts[3], nil, &issue); e != nil {
				return proof, e
			}
			text := issue.Title + "\n" + issue.Body
			if issue.State != "open" || !strings.EqualFold(issue.URL, c.Issue) || issue.PullRequest != nil || !affirmativeFlakeIssue(text, c) {
				return proof, fmt.Errorf("issue does not affirmatively verify this exact workflow/job/test/signature as flaky")
			}
			issuePath, e := artifact(g.Root, issue)
			if e != nil {
				return proof, e
			}
			proof.Evidence = append(proof.Evidence, issuePath)
		} else {
			return proof, fmt.Errorf("unsupported CI classification kind")
		}
		if !validText(c.Test) || !strings.Contains(logBody, c.Test) || c.Signature != "" && !strings.Contains(logBody, c.Signature) {
			return proof, fmt.Errorf("test/signature not observed in exact job logs")
		}
		covered[c.Test] = true
	}
	if !pipeline {
		stepLogs, e := failedStepLogs(logText, job.Name, job.Steps)
		if e != nil {
			return proof, e
		}
		for _, stepLog := range stepLogs {
			if e := verifyTestLogInventory(stepLog, classes, covered); e != nil {
				return proof, e
			}
		}
	}
	proof.Exempt = true
	return proof, nil
}

// Reruns are guarded before dispatch, not merely recognized after the write.
type CIRerunVerifier interface {
	VerifyRerun(context.Context, Intent) error
}

func (g *GitHub) VerifyRerun(ctx context.Context, i Intent) error {
	attempt, e := rerunAttempt(i.Payload)
	if e != nil {
		return e
	}
	var run struct {
		Head       string `json:"head_sha"`
		Attempt    int    `json:"run_attempt"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	if e := g.api(ctx, i.PR, "GET", "repos/"+i.PR.Owner+"/"+i.PR.Name+"/actions/runs/"+stringValue(i.Payload, "run_id"), nil, &run); e != nil {
		return e
	}
	if run.Head != i.Head || int64(run.Attempt) != attempt || run.Status != "completed" || !contains([]string{"failure", "timed_out", "cancelled"}, run.Conclusion) {
		return fmt.Errorf("rerun requires the exact failed completed run/head/attempt")
	}
	return nil
}

// A JSON number must identify one real Actions attempt and its exact successor.
func rerunAttempt(payload map[string]any) (int64, error) {
	n, ok := payload["attempt"].(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || math.Trunc(n) != n || n >= (1<<53)-1 {
		return 0, fmt.Errorf("exact integral Actions attempt with representable successor required")
	}
	return int64(n), nil
}
