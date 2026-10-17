// Package development implements controller-owned Git and publication actions.
package development

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type PublicationPolicy struct {
	Repository string   `yaml:"repository"`
	Base       string   `yaml:"base"`
	Remote     string   `yaml:"remote"`
	Checks     []string `yaml:"checks"`
	Reviewers  []string `yaml:"reviewers"`
}
type Runtime struct {
	retentionHook func(string, string) error
	Secrets       map[string]string
	Root          string
	Store         *workflows.SQLiteStore
	Policy        PublicationPolicy
	mu            sync.Mutex
}

func (r *Runtime) git(ctx context.Context, dir string, args ...string) (string, error) {
	return r.gitEnv(ctx, dir, nil, args...)
}
func (r *Runtime) gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, stderr limited
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cleanup, err := process.Supervise(cmd)
	if err != nil {
		return "", err
	}
	defer cleanup()
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, workflows.Redact(stderr.String(), r.secretValues()))
	}
	if out.overflow || stderr.overflow {
		return "", fmt.Errorf("Git output exceeds limit")
	}
	return strings.TrimSpace(out.String()), nil
}

type limited struct {
	strings.Builder
	overflow bool
}

func (b *limited) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (16 << 20) - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.Builder.Write(p)
	return n, nil
}
func (r *Runtime) Admission(ctx context.Context, all map[string]*workflows.Workflow) (map[string]any, error) {
	needed := false
	for _, w := range all {
		for _, j := range w.Jobs {
			for _, s := range j.Steps {
				needed = needed || workflows.IsControllerAction(s.Uses)
			}
		}
	}
	if !needed {
		return nil, nil
	}
	head, err := r.git(ctx, r.Root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	return map[string]any{"git_head": head}, nil
}
func key(req workflows.ActionRequest) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d", req.RunID, req.JobID, req.StepID, req.Iteration)))
	return hex.EncodeToString(h[:])
}
func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func (r *Runtime) begin(ctx context.Context, req workflows.ActionRequest, payload map[string]any) (workflows.ExternalIntent, error) {
	allSecrets := r.secretValues()
	for k, v := range req.Secrets {
		allSecrets[k] = v
	}
	redacted := workflows.RedactValue(payload, allSecrets)
	a, _ := json.Marshal(payload)
	b, _ := json.Marshal(redacted)
	if string(a) != string(b) {
		return workflows.ExternalIntent{}, fmt.Errorf("controller action payload contains a credential value; use a reference")
	}

	if err := r.Store.InitIntents(ctx); err != nil {
		return workflows.ExternalIntent{}, err
	}
	id := key(req)
	intent, err := r.Store.Intent(ctx, id)
	if err == nil {
		if intent.Kind != req.Uses {
			return intent, fmt.Errorf("action kind changed")
		}
		return intent, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return intent, err
	}
	intent = workflows.ExternalIntent{Key: id, RunID: req.RunID, Kind: req.Uses, Payload: payload, Status: "pending"}
	return intent, r.Store.PutIntent(ctx, intent)
}
func (r *Runtime) finish(ctx context.Context, intent workflows.ExternalIntent, out map[string]any) (map[string]any, error) {
	intent.Status = "completed"
	intent.Result = out
	return out, r.Store.PutIntent(context.WithoutCancel(ctx), intent)
}
func (r *Runtime) RunAction(ctx context.Context, req workflows.ActionRequest) (map[string]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch req.Uses {
	case "workspace/create", "workspace/snapshot":
		return r.create(ctx, req)
	case "git/commit":
		return r.commit(ctx, req)
	case "workspace/integrate":
		return r.integrate(ctx, req)
	case "github/pr":
		return r.publish(ctx, req)
	}
	return nil, fmt.Errorf("unsupported controller action")
}
func (r *Runtime) managed(run, path string) (string, error) {
	canonicalRoot, err := r.canonicalRoot()
	if err != nil {
		return "", err
	}
	root := filepath.Join(canonicalRoot, ".agent-workflow", "workspaces", run)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, real)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("action requires an engine-managed workspace")
	}
	return real, nil
}
func (r *Runtime) create(ctx context.Context, req workflows.ActionRequest) (map[string]any, error) {
	base := str(req.With, "commit")
	if base == "" {
		base = str(req.Metadata, "git_head")
	}
	resolved, err := r.git(ctx, r.Root, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return nil, err
	}
	root, err := r.canonicalRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".agent-workflow", "workspaces", origin(req), key(req)[:16])
	intent, err := r.begin(ctx, req, map[string]any{"path": path, "base": resolved, "independent": req.With["independent"]})
	if err != nil {
		return nil, err
	}
	if intent.Status == "completed" {
		return intent.Result, nil
	}
	path = str(intent.Payload, "path")
	resolved = str(intent.Payload, "base")
	if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		independent, _ := intent.Payload["independent"].(bool)
		if req.Uses == "workspace/create" && !independent {
			_, err = r.git(ctx, r.Root, "worktree", "add", "--detach", path, resolved)
		} else {
			_, err = r.git(ctx, r.Root, "clone", "--no-hardlinks", "--no-checkout", "--", root, path)
			if err == nil {
				_, err = r.git(ctx, path, "checkout", "--detach", resolved)
			}
		}
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	head, err := r.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if head != resolved {
		return nil, fmt.Errorf("workspace head changed during uncertain creation")
	}
	return r.finish(ctx, intent, map[string]any{"path": path, "commit": head, "mode": strings.TrimPrefix(req.Uses, "workspace/")})
}
func (r *Runtime) commit(ctx context.Context, req workflows.ActionRequest) (map[string]any, error) {
	path, err := r.managed(origin(req), str(req.With, "path"))
	if err != nil {
		return nil, err
	}
	message := str(req.With, "message")
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("commit message is required")
	}
	head, err := r.git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	intent, err := r.begin(ctx, req, map[string]any{"path": path, "parent": head, "message": message})
	if err != nil {
		return nil, err
	}
	if intent.Status == "completed" {
		return intent.Result, nil
	}
	footer := "Agent-Workflow-Action: " + intent.Key
	if head != str(intent.Payload, "parent") {
		body, err := r.git(ctx, path, "show", "-s", "--format=%B", "HEAD")
		if err != nil || !strings.Contains(body, footer) {
			return nil, fmt.Errorf("unexpected workspace head after uncertain commit")
		}
		if _, err = r.git(ctx, r.Root, "cat-file", "-e", head+"^{commit}"); err != nil {
			if _, err = r.git(ctx, r.Root, "fetch", "--no-tags", "--no-write-fetch-head", path, head); err != nil {
				return nil, err
			}
		}
		return r.finish(ctx, intent, map[string]any{"path": path, "commit": head})
	}
	if _, err = r.git(ctx, path, "add", "-A"); err != nil {
		return nil, err
	}
	staged, err := r.git(ctx, path, "diff", "--cached", "--name-only")
	if err != nil {
		return nil, err
	}
	if staged != "" {
		if _, err = r.git(ctx, path, "commit", "-m", str(intent.Payload, "message")+"\n\n"+footer); err != nil {
			return nil, err
		}
		head, err = r.git(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
	}
	if _, err = r.git(ctx, r.Root, "cat-file", "-e", head+"^{commit}"); err != nil {
		if _, err = r.git(ctx, r.Root, "fetch", "--no-tags", "--no-write-fetch-head", path, head); err != nil {
			return nil, err
		}
	}
	return r.finish(ctx, intent, map[string]any{"path": path, "commit": head})
}
func stringsArg(v any) ([]string, error) {
	switch a := v.(type) {
	case []any:
		out := []string{}
		for _, x := range a {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("expected string list")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return a, nil
	case string:
		return []string{a}, nil
	}
	return nil, fmt.Errorf("expected string list")
}
func (r *Runtime) integrate(ctx context.Context, req workflows.ActionRequest) (map[string]any, error) {
	path, err := r.managed(origin(req), str(req.With, "path"))
	if err != nil {
		return nil, err
	}
	commits, err := stringsArg(req.With["commits"])
	if err != nil || len(commits) == 0 {
		return nil, fmt.Errorf("integration requires commits")
	}
	for i, source := range commits {
		sha, err := r.git(ctx, r.Root, "rev-parse", "--verify", source+"^{commit}")
		if err != nil {
			return nil, err
		}
		head, err := r.git(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		part := req
		part.StepID += fmt.Sprintf("-commit-%d", i)
		intent, err := r.begin(ctx, part, map[string]any{"path": path, "parent": head, "source": sha})
		if err != nil {
			return nil, err
		}
		if intent.Status == "completed" {
			continue
		}
		if head != str(intent.Payload, "parent") {
			body, err := r.git(ctx, path, "show", "-s", "--format=%B", "HEAD")
			if err != nil || !strings.Contains(body, "(cherry picked from commit "+sha+")") {
				return nil, fmt.Errorf("unexpected integration head; resolution required")
			}
		} else {
			if _, err = r.git(ctx, path, "rev-parse", "--verify", "CHERRY_PICK_HEAD"); err == nil {
				return nil, fmt.Errorf("integration conflict retained; resolve it before resuming")
			}
			if _, err = r.git(ctx, path, "cherry-pick", "-x", sha); err != nil {
				return nil, err
			}
		}
		head, err = r.git(ctx, path, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		if _, err = r.finish(ctx, intent, map[string]any{"path": path, "commit": head}); err != nil {
			return nil, err
		}
	}
	head, err := r.git(ctx, path, "rev-parse", "HEAD")
	return map[string]any{"path": path, "commit": head}, err
}

func origin(req workflows.ActionRequest) string {
	if id := str(req.Metadata, "origin_run_id"); id != "" {
		return id
	}
	return req.RunID
}

func (r *Runtime) secretValues() map[string]string {
	out := map[string]string{}
	for k, v := range r.Secrets {
		out[k] = v
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			out[key] = v
		}
	}
	return out
}

func (r *Runtime) canonicalRoot() (string, error) {
	root, err := filepath.Abs(r.Root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}
