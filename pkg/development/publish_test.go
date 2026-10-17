package development

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func fakeGitHub(t *testing.T, r *Runtime, run, sha string, lost bool) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if _, err = r.git(context.Background(), r.Root, "clone", "--bare", r.Root, remote); err != nil {
		t.Fatal(err)
	}
	if _, err = r.git(context.Background(), r.Root, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	_ = os.Mkdir(bin, 0700)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	gitScript := "#!/bin/sh\nif [ \"$1\" = remote ] && [ \"$2\" = get-url ]; then echo https://github.com/owner/repository.git; else exec " + quote(realGit) + " \"$@\"; fi\n"
	_ = os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0700)
	payload := []map[string]any{{"number": 1, "html_url": "https://github.com/owner/repository/pull/1", "state": "open", "head": map[string]any{"sha": sha, "ref": "agent-workflow/" + run, "repo": map[string]string{"full_name": "owner/repository"}}, "base": map[string]string{"ref": "main"}}}
	b, _ := json.Marshal(payload)
	_ = os.WriteFile(filepath.Join(dir, "payload.json"), b, 0600)
	script := fmt.Sprintf("#!/bin/sh\nstate=%s\nif [ \"$1\" = api ]; then if [ -f \"$state/pr.json\" ]; then cat \"$state/pr.json\"; else echo '[]'; fi; exit 0; fi\nif [ \"$1\" = pr ] && [ \"$2\" = create ]; then printf x >> \"$state/creates\"; cp \"$state/payload.json\" \"$state/pr.json\"; %s; fi\nexit 2\n", quote(dir), map[bool]string{true: "echo 'response lost' >&2; exit 1", false: "echo https://github.com/owner/repository/pull/1; exit 0"}[lost])
	_ = os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}
func publicationRequest(run string, w map[string]any) workflows.ActionRequest {
	sha := w["commit"]
	return workflows.ActionRequest{RunID: run, JobID: "publication", StepID: "pr", Iteration: 1, Uses: "github/pr", With: map[string]any{"path": w["path"], "commit": sha, "title": "Implement change", "body": "A multiline body.\n\nVerified by independent reviews."}, Needs: map[string]workflows.JobExecution{"test": {Status: "succeeded", Outputs: map[string]any{"checked_commit": sha}}, "review_one": {Status: "succeeded", Outputs: map[string]any{"reviewed_commit": sha, "clean": true}}, "review_two": {Status: "succeeded", Outputs: map[string]any{"reviewed_commit": sha, "clean": true}}}}
}
func TestPublicationReconcilesLostCreateResponseExactlyOnce(t *testing.T) {
	r, run := fixture(t)
	w := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(w, "commit"), true)
	req := publicationRequest(run, w)
	if _, err := r.RunAction(context.Background(), req); err == nil {
		t.Fatal("lost response was ignored")
	}
	out, err := r.RunAction(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out["number"] != 1 {
		t.Fatal(out)
	}
	if _, err = r.RunAction(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "creates"))
	if err != nil || string(b) != "x" {
		t.Fatalf("duplicate publication: %q %v", b, err)
	}
}
func TestUncertainAbsentPRNeverCreatesAgain(t *testing.T) {
	r, run := fixture(t)
	w := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(w, "commit"), true)
	req := publicationRequest(run, w)
	_, _ = r.RunAction(context.Background(), req)
	_ = os.Remove(filepath.Join(dir, "pr.json"))
	_, err := r.RunAction(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "manual attention") {
		t.Fatal("uncertain create automatically retried", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "creates"))
	if string(b) != "x" {
		t.Fatal("second create attempted")
	}
}
func TestUnexpectedRemoteChangeStopsPublication(t *testing.T) {
	r, run := fixture(t)
	w := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(w, "commit"), false)
	ctx := context.Background()
	_ = os.WriteFile(filepath.Join(r.Root, "changed.txt"), []byte("other"), 0600)
	_, _ = r.git(ctx, r.Root, "add", "changed.txt")
	if _, err := r.git(ctx, r.Root, "commit", "-m", "Other writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.git(ctx, r.Root, "push", "origin", "HEAD:refs/heads/agent-workflow/"+run); err != nil {
		t.Fatal(err)
	}
	_, err := r.RunAction(ctx, publicationRequest(run, w))
	if err == nil || !strings.Contains(err.Error(), "unexpected remote") {
		t.Fatal("remote change accepted", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "creates")); !os.IsNotExist(err) {
		t.Fatal("PR was created after unexpected remote change")
	}
}

func TestPushRejectsBranchCreatedDuringNegotiation(t *testing.T) {
	r, run := fixture(t)
	ctx := context.Background()
	base, _ := r.git(ctx, r.Root, "rev-parse", "HEAD")
	w := create(t, r, run, "integration")
	_ = os.WriteFile(filepath.Join(str(w, "path"), "candidate.txt"), []byte("candidate"), 0600)
	committed, err := r.RunAction(ctx, workflows.ActionRequest{RunID: run, JobID: "integration", StepID: "commit", Iteration: 1, Uses: "git/commit", With: map[string]any{"path": w["path"], "message": "Candidate"}})
	if err != nil {
		t.Fatal(err)
	}
	w["commit"] = committed["commit"]
	realGit, _ := exec.LookPath("git")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(w, "commit"), false)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	remote := filepath.Join(dir, "remote.git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = remote ] && [ "$2" = get-url ]; then echo https://github.com/owner/repository.git; exit 0; fi
for arg in "$@"; do
  if [ "$arg" = push ] && [ ! -f %s ]; then
    : > %s
    %s --git-dir %s update-ref %s %s
  fi
done
exec %s "$@"
`, quote(filepath.Join(dir, "raced")), quote(filepath.Join(dir, "raced")), quote(realGit), quote(remote), quote("refs/heads/agent-workflow/"+run), quote(base), quote(realGit))
	if err = os.WriteFile(filepath.Join(dir, "bin", "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = r.RunAction(ctx, publicationRequest(run, w))
	if err == nil || !strings.Contains(err.Error(), "during push negotiation") {
		t.Fatal("negotiation race was accepted", err)
	}
	head, err := r.remoteHead(ctx, "origin", "agent-workflow/"+run)
	if err != nil || head != base {
		t.Fatal("unexpected branch was changed", head, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "creates")); !os.IsNotExist(err) {
		t.Fatal("PR was created after negotiation race")
	}
}

func TestGuardedPushPreservesExistingPrePushHook(t *testing.T) {
	r, run := fixture(t)
	w := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(w, "commit"), false)
	hook := filepath.Join(r.Root, ".git", "hooks", "pre-push")
	script := "#!/bin/sh\necho original-hook-denied >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := r.RunAction(context.Background(), publicationRequest(run, w))
	if err == nil || !strings.Contains(err.Error(), "original-hook-denied") {
		t.Fatal("existing hook was bypassed", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "creates")); !os.IsNotExist(err) {
		t.Fatal("PR created despite hook rejection")
	}
}

func TestLostPushResponseReconcilesWithoutAnotherPush(t *testing.T) {
	r, run := fixture(t)
	workspace := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(workspace, "commit"), false)
	git := filepath.Join(dir, "bin", "git")
	saved := git + "-original"
	if err := os.Rename(git, saved); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nfor arg in \"$@\"; do if [ \"$arg\" = push ]; then printf x >> " + quote(filepath.Join(dir, "pushes")) + "; " + quote(saved) + " \"$@\" || exit $?; echo response-lost >&2; exit 1; fi; done\nexec " + quote(saved) + " \"$@\"\n"
	if err := os.WriteFile(git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	req := publicationRequest(run, workspace)
	if _, err := r.RunAction(context.Background(), req); err == nil {
		t.Fatal("lost push response ignored")
	}
	if _, err := r.RunAction(context.Background(), req); err != nil {
		t.Fatal("push was not reconciled", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pushes")); err != nil || string(data) != "x" {
		t.Fatal("push was replayed", string(data), err)
	}
}

func TestDeletedPublishedBranchAndClosedPRRemainVisible(t *testing.T) {
	for _, change := range []string{"deleted-branch", "closed-pr"} {
		t.Run(change, func(t *testing.T) {
			r, run := fixture(t)
			workspace := create(t, r, run, "integration")
			r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
			dir := fakeGitHub(t, r, run, str(workspace, "commit"), false)
			req := publicationRequest(run, workspace)
			if _, err := r.RunAction(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if change == "deleted-branch" {
				if _, err := r.git(context.Background(), r.Root, "push", "origin", ":refs/heads/agent-workflow/"+run); err != nil {
					t.Fatal(err)
				}
			} else {
				data, err := os.ReadFile(filepath.Join(dir, "pr.json"))
				if err != nil {
					t.Fatal(err)
				}
				data = []byte(strings.Replace(string(data), `"state":"open"`, `"state":"closed"`, 1))
				if err = os.WriteFile(filepath.Join(dir, "pr.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.RunAction(context.Background(), req); err == nil {
				t.Fatal("changed publication silently accepted")
			}
			if data, err := os.ReadFile(filepath.Join(dir, "creates")); err != nil || string(data) != "x" {
				t.Fatal("another PR was created", string(data), err)
			}
		})
	}
}

func TestChangedRemoteBaseStopsPublicationRecovery(t *testing.T) {
	r, run := fixture(t)
	workspace := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(workspace, "commit"), false)
	req := publicationRequest(run, workspace)
	ctx := context.Background()
	if _, err := r.RunAction(ctx, req); err != nil {
		t.Fatal(err)
	}
	tree, err := r.git(ctx, r.Root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.remoteHead(ctx, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := r.git(ctx, r.Root, "commit-tree", tree, "-p", base, "-m", "Independent base update")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.git(ctx, r.Root, "push", "origin", changed+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RunAction(ctx, req); err == nil || !strings.Contains(err.Error(), "base branch change") {
		t.Fatal("changed base accepted", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "creates")); err != nil || string(data) != "x" {
		t.Fatal("extra publication", err)
	}
}

func TestCancellationAfterCreateEffectReconcilesExactlyOnce(t *testing.T) {
	r, run := fixture(t)
	workspace := create(t, r, run, "integration")
	r.Policy = PublicationPolicy{Repository: "owner/repository", Base: "main", Checks: []string{"test"}, Reviewers: []string{"review_one", "review_two"}}
	dir := fakeGitHub(t, r, run, str(workspace, "commit"), false)
	path := filepath.Join(dir, "bin", "gh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "echo https://github.com/owner/repository/pull/1; exit 0", "sleep 30; exit 0", 1))
	if err = os.WriteFile(path, script, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	req := publicationRequest(run, workspace)
	go func() { _, err := r.RunAction(ctx, req); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "pr.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake create effect did not occur")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancellation ignored")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("publication did not stop")
	}
	if _, err = r.RunAction(context.Background(), req); err != nil {
		t.Fatal("lost create result was not reconciled", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "creates")); err != nil || string(data) != "x" {
		t.Fatal("create replayed after cancellation", err)
	}
}
