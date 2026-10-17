package development

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (r *Runtime) gh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = r.Root
	var out, stderr limited
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cleanup, err := process.Supervise(cmd)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("GitHub request failed: %w: %s", err, workflows.Redact(stderr.String(), r.secretValues()))
	}
	if out.overflow || stderr.overflow {
		return "", fmt.Errorf("GitHub output exceeds limit")
	}
	return strings.TrimSpace(out.String()), nil
}
func remoteRepository(remote string) string {
	if strings.HasPrefix(remote, "git@github.com:") {
		return strings.TrimSuffix(strings.TrimPrefix(remote, "git@github.com:"), ".git")
	}
	u, err := url.Parse(remote)
	if err != nil || u.Hostname() != "github.com" || u.User != nil && u.Scheme != "ssh" {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "ssh" {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
}
func (r *Runtime) remoteHead(ctx context.Context, remote, branch string) (string, error) {
	out, err := r.git(ctx, r.Root, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[1] != "refs/heads/"+branch {
		return "", fmt.Errorf("unexpected remote ref response")
	}
	return fields[0], nil
}

type pullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (r *Runtime) findPR(ctx context.Context, branch, sha string) (*pullRequest, error) {
	owner, _, _ := strings.Cut(r.Policy.Repository, "/")
	endpoint := "repos/" + r.Policy.Repository + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+branch) + "&base=" + url.QueryEscape(r.Policy.Base) + "&per_page=100"
	out, err := r.gh(ctx, "api", "--method", "GET", endpoint)
	if err != nil {
		return nil, err
	}
	var prs []pullRequest
	if err = json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, err
	}
	if len(prs) > 1 {
		return nil, fmt.Errorf("multiple PRs exist for run branch")
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	if pr.State != "open" || pr.Head.SHA != sha || pr.Head.Ref != branch || pr.Head.Repo.FullName != r.Policy.Repository || pr.Base.Ref != r.Policy.Base {
		return nil, fmt.Errorf("existing PR differs from publication intent")
	}
	return &pr, nil
}
func (r *Runtime) publish(ctx context.Context, req workflows.ActionRequest) (map[string]any, error) {
	policy := r.Policy
	if !repositoryPattern.MatchString(policy.Repository) || policy.Base == "" || len(policy.Checks) == 0 || len(policy.Reviewers) < 2 {
		return nil, fmt.Errorf("publication requires a configured repository, base, checks, and two reviewers")
	}
	if _, err := r.git(ctx, r.Root, "check-ref-format", "refs/heads/"+policy.Base); err != nil {
		return nil, err
	}
	path, err := r.managed(origin(req), str(req.With, "path"))
	if err != nil {
		return nil, err
	}
	sha, err := r.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if supplied := str(req.With, "commit"); supplied == "" || supplied != sha {
		return nil, fmt.Errorf("publication commit must equal the managed workspace HEAD")
	}
	status, err := r.git(ctx, path, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if status != "" {
		return nil, fmt.Errorf("publication workspace is dirty")
	}
	seen := map[string]bool{}
	for _, id := range policy.Checks {
		job, ok := req.Needs[id]
		if !ok || job.Status != workflows.RunStatusSucceeded || job.Outputs["checked_commit"] != sha {
			return nil, fmt.Errorf("check %s has not passed for final commit", id)
		}
	}
	for _, id := range policy.Reviewers {
		job, ok := req.Needs[id]
		clean, _ := job.Outputs["clean"].(bool)
		if seen[id] || !ok || job.Status != workflows.RunStatusSucceeded || !clean || job.Outputs["reviewed_commit"] != sha {
			return nil, fmt.Errorf("review %s is missing, stale, or not clean", id)
		}
		seen[id] = true
	}
	remote := policy.Remote
	if remote == "" {
		remote = "origin"
	}
	remoteURL, err := r.git(ctx, r.Root, "remote", "get-url", remote)
	if err != nil {
		return nil, err
	}
	if remoteRepository(remoteURL) != policy.Repository {
		return nil, fmt.Errorf("Git remote does not match allowlisted repository")
	}
	base, err := r.remoteHead(ctx, remote, policy.Base)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return nil, fmt.Errorf("configured base branch is absent")
	}
	branch := "agent-workflow/" + req.RunID
	// One publication identity per run, independent of action attempts.
	req.JobID = "__controller"
	req.StepID = "__publication"
	req.Iteration = 0
	title := str(req.With, "title")
	if title == "" {
		return nil, fmt.Errorf("PR title is required")
	}
	intent, err := r.begin(ctx, req, map[string]any{"repository": policy.Repository, "base": policy.Base, "base_commit": base, "branch": branch, "commit": sha, "title": title, "body": str(req.With, "body")})
	if err != nil {
		return nil, err
	}
	if str(intent.Payload, "commit") != sha || str(intent.Payload, "base") != policy.Base || str(intent.Payload, "repository") != policy.Repository {
		return nil, fmt.Errorf("publication candidate changed; reconcile the existing intent")
	}
	if expected := str(intent.Payload, "base_commit"); expected != "" && expected != base {
		return nil, fmt.Errorf("unexpected remote base branch change; publication stopped")
	}
	remoteSHA, err := r.remoteHead(ctx, remote, branch)
	if err != nil {
		return nil, err
	}
	if remoteSHA != "" && remoteSHA != sha {
		return nil, fmt.Errorf("unexpected remote branch change; publication stopped")
	}
	if remoteSHA == "" {
		if intent.Status == "completed" {
			return nil, fmt.Errorf("published remote branch was deleted")
		}
		pushRequested, _ := intent.Payload["push_requested"].(bool)
		createRequested, _ := intent.Payload["create_requested"].(bool)
		if pushRequested || createRequested {
			return nil, fmt.Errorf("uncertain or deleted publication branch requires manual attention before another push")
		}
		intent.Payload["push_requested"] = true
		intent.Status = "effect_pending"
		if err = r.Store.PutIntent(ctx, intent); err != nil {
			return nil, err
		}
		if err = r.pushNewBranch(ctx, remote, branch, sha); err != nil {
			return nil, err
		}
		remoteSHA, err = r.remoteHead(ctx, remote, branch)
		if err != nil {
			return nil, err
		}
		if remoteSHA != sha {
			return nil, fmt.Errorf("push did not produce intended remote commit")
		}
	}
	if confirmed, _ := intent.Payload["push_confirmed"].(bool); !confirmed && intent.Status != "completed" {
		intent.Payload["push_confirmed"] = true
		intent.Status = "effect_pending"
		if err = r.Store.PutIntent(ctx, intent); err != nil {
			return nil, err
		}
	}
	pr, err := r.findPR(ctx, branch, sha)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		if intent.Status == "completed" {
			return nil, fmt.Errorf("completed publication is absent from reconciliation; manual attention required")
		}
		if requested, _ := intent.Payload["create_requested"].(bool); requested {
			return nil, fmt.Errorf("uncertain PR creation is absent from reconciliation; manual attention required before any new create request")
		}
		intent.Payload["create_requested"] = true
		intent.Status = "effect_pending"
		if err = r.Store.PutIntent(ctx, intent); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(filepath.Join(r.Root, ".agent-workflow"), "pr-body-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		_, err = tmp.WriteString(str(intent.Payload, "body"))
		closeErr := tmp.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if _, err = r.gh(ctx, "pr", "create", "--repo", policy.Repository, "--base", policy.Base, "--head", branch, "--title", str(intent.Payload, "title"), "--body-file", tmp.Name()); err != nil {
			return nil, err
		}
		pr, err = r.findPR(ctx, branch, sha)
		if err != nil {
			return nil, err
		}
		if pr == nil {
			return nil, fmt.Errorf("PR creation returned without a reconcilable PR; intent retained")
		}
	}
	return r.finish(ctx, intent, map[string]any{"url": pr.URL, "number": pr.Number, "branch": branch, "commit": sha, "base": policy.Base})
}
