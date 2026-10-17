package development

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func fixture(t *testing.T) (*Runtime, string) {
	t.Helper()
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	root := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Workflow Test")
	git("config", "user.email", "workflow@example.invalid")
	_ = os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0600)
	git("add", "file.txt")
	git("commit", "-m", "baseline")
	_ = os.Mkdir(filepath.Join(root, "workflows"), 0700)
	_ = os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {x: {steps: [{argv: [true]}]}}"), 0600)
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e := &workflows.Executor{WorkspaceDir: root, Store: store}
	run, err := e.Prepare(context.Background(), workflows.RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	return &Runtime{Root: root, Store: store}, run.ID
}
func create(t *testing.T, r *Runtime, run, job string) map[string]any {
	t.Helper()
	ctx := context.Background()
	sha, err := r.git(ctx, r.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.RunAction(ctx, workflows.ActionRequest{RunID: run, JobID: job, StepID: "workspace", Iteration: 1, Uses: "workspace/create", Metadata: map[string]any{"git_head": sha}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestWritersIntegrateSeriallyAndCallerDirtIsPreserved(t *testing.T) {
	r, run := fixture(t)
	ctx := context.Background()
	_ = os.WriteFile(filepath.Join(r.Root, "caller-dirt"), []byte("keep"), 0600)
	var commits []any
	for _, name := range []string{"one", "two"} {
		writer := create(t, r, run, name)
		path := str(writer, "path")
		_ = os.WriteFile(filepath.Join(path, name+".txt"), []byte(name), 0600)
		out, err := r.RunAction(ctx, workflows.ActionRequest{RunID: run, JobID: name, StepID: "commit", Iteration: 1, Uses: "git/commit", With: map[string]any{"path": path, "message": "Add " + name}})
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, out["commit"])
	}
	integration := create(t, r, run, "integration")
	req := workflows.ActionRequest{RunID: run, JobID: "integration", StepID: "merge", Iteration: 1, Uses: "workspace/integrate", With: map[string]any{"path": integration["path"], "commits": commits}}
	out, err := r.RunAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.RunAction(ctx, req)
	if err != nil || again["commit"] != out["commit"] {
		t.Fatal("integration was not idempotent", again, err)
	}
	for _, name := range []string{"one", "two"} {
		if _, err = os.Stat(filepath.Join(str(integration, "path"), name+".txt")); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(r.Root, "caller-dirt")); err != nil || string(b) != "keep" {
		t.Fatal("caller dirt changed")
	}
	head, _ := r.git(ctx, r.Root, "rev-parse", "HEAD")
	if head == out["commit"] {
		t.Fatal("caller branch moved")
	}
}
func TestCommitReconcilesLostCompletion(t *testing.T) {
	r, run := fixture(t)
	ctx := context.Background()
	writer := create(t, r, run, "writer")
	path := str(writer, "path")
	head := str(writer, "commit")
	_ = os.WriteFile(filepath.Join(path, "new.txt"), []byte("new"), 0600)
	req := workflows.ActionRequest{RunID: run, JobID: "writer", StepID: "commit", Iteration: 1, Uses: "git/commit", With: map[string]any{"path": path, "message": "change"}}
	intent, err := r.begin(ctx, req, map[string]any{"path": path, "parent": head, "message": "change"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.git(ctx, path, "add", "-A")
	_, err = r.git(ctx, path, "commit", "-m", "change\n\nAgent-Workflow-Action: "+intent.Key)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := r.git(ctx, path, "rev-parse", "HEAD")
	out, err := r.RunAction(ctx, req)
	if err != nil || out["commit"] != before {
		t.Fatal(out, err)
	}
	count, _ := r.git(ctx, path, "rev-list", "--count", head+"..HEAD")
	if count != "1" {
		t.Fatal("duplicate commit")
	}
}
func TestIntegrationConflictAndCrossRunActionsStop(t *testing.T) {
	r, run := fixture(t)
	ctx := context.Background()
	var commits []any
	for _, name := range []string{"one", "two"} {
		w := create(t, r, run, name)
		_ = os.WriteFile(filepath.Join(str(w, "path"), "file.txt"), []byte(name), 0600)
		out, err := r.RunAction(ctx, workflows.ActionRequest{RunID: run, JobID: name, StepID: "commit", Iteration: 1, Uses: "git/commit", With: map[string]any{"path": w["path"], "message": name}})
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, out["commit"])
	}
	integration := create(t, r, run, "integration")
	_, err := r.RunAction(ctx, workflows.ActionRequest{RunID: run, JobID: "integration", StepID: "merge", Iteration: 1, Uses: "workspace/integrate", With: map[string]any{"path": integration["path"], "commits": commits}})
	if err == nil {
		t.Fatal("conflict ignored")
	}
	if _, err = r.git(ctx, str(integration, "path"), "rev-parse", "--verify", "CHERRY_PICK_HEAD"); err != nil {
		t.Fatal("conflict artifacts lost")
	}
	if _, err = r.managed("other-run", str(integration, "path")); err == nil {
		t.Fatal("cross-run write accepted")
	}
}
func TestStaleReviewBlocksPublicationBeforeEffects(t *testing.T) {
	r, run := fixture(t)
	integration := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"tests"}, Reviewers: []string{"one", "two"}}
	req := workflows.ActionRequest{RunID: run, JobID: "publish", StepID: "pr", Uses: "github/pr", With: map[string]any{"path": integration["path"], "commit": integration["commit"], "title": "Change"}, Needs: map[string]workflows.JobExecution{"tests": {Status: "succeeded", Outputs: map[string]any{"checked_commit": integration["commit"]}}, "one": {Status: "succeeded", Outputs: map[string]any{"clean": true, "reviewed_commit": "stale"}}, "two": {Status: "succeeded", Outputs: map[string]any{"clean": true, "reviewed_commit": integration["commit"]}}}}
	_, err := r.RunAction(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatal("stale review accepted", err)
	}
}
func TestManagedActionsThroughExecutor(t *testing.T) {
	r, _ := fixture(t)
	ctx := context.Background()
	source := `jobs:
  writer:
    outputs:
      path: ${{ steps.workspace.outputs.path }}
      commit: ${{ steps.commit.outputs.commit }}
    steps:
      - id: workspace
        uses: workspace/create
      - shell: sh
        working-directory: ${{ steps.workspace.outputs.path }}
        run: printf 'change' > added.txt
      - id: commit
        uses: git/commit
        with: {path: '${{ steps.workspace.outputs.path }}', message: 'Add change'}
`
	if err := os.WriteFile(filepath.Join(r.Root, "workflows", "development.yml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	e := &workflows.Executor{WorkspaceDir: r.Root, Store: r.Store, Actions: r}
	run, err := e.Run(ctx, workflows.RunRequest{Ref: "workflows/development.yml"})
	if err != nil {
		b, _ := json.Marshal(run)
		t.Fatal(string(b), err)
	}
	if run.Jobs["writer"].Outputs["commit"] == "" {
		t.Fatal("missing committed output")
	}
	_, snap, err := r.Store.Load(ctx, run.ID)
	if err != nil || snap.Metadata["git_head"] == nil {
		t.Fatal("baseline not pinned", err)
	}
}

func TestRetentionRemovesOnlyTerminalOwnedWorkspaces(t *testing.T) {
	r, run := fixture(t)
	writer := create(t, r, run, "writer")
	ctx := context.Background()
	record, _, err := r.Store.Load(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = workflows.RunStatusSucceeded
	if err = r.Store.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	caller := filepath.Join(r.Root, "keep.txt")
	_ = os.WriteFile(caller, []byte("caller"), 0600)
	if err = r.Cleanup(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(str(writer, "path")); !os.IsNotExist(err) {
		t.Fatal("managed worktree retained")
	}
	if _, _, err = r.Store.Load(ctx, run); err == nil {
		t.Fatal("terminal record retained")
	}
	if b, err := os.ReadFile(caller); err != nil || string(b) != "caller" {
		t.Fatal("caller workspace changed")
	}
}
