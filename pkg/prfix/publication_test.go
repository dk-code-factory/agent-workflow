package prfix

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestSplitPublicationUsesDisposableLocalGitAndReconcilesLostPush(t *testing.T) {
	root, provider, req := fixture(t)
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, e := git(ctx, root, "init", "--bare", bare); e != nil {
		t.Fatal(e)
	}
	original := provider.ev.Metadata.Head.SHA
	tree, e := git(ctx, root, "rev-parse", original+"^{tree}")
	if e != nil {
		t.Fatal(e)
	}
	split, e := git(ctx, root, "commit-tree", tree, "-p", original, "-m", "Prepared split fixture")
	if e != nil {
		t.Fatal(e)
	}
	recovery := "refs/prfix/fixture-recovery"
	if _, e = git(ctx, root, "update-ref", recovery, original); e != nil {
		t.Fatal(e)
	}
	realGit, e := exec.LookPath("git")
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	pushes := filepath.Join(dir, "pushes")
	script := `#!/usr/bin/env python3
import os,sys
args=sys.argv[1:]
if args and args[0]=='push':
 with open(os.environ['PRFIX_TEST_PUSHES'],'a') as f: f.write('push\n')
args=[os.environ['PRFIX_TEST_REMOTE'] if a=='https://github.com/owner/repo.git' else a for a in args]
os.execv(os.environ['PRFIX_TEST_GIT'],[os.environ['PRFIX_TEST_GIT']]+args)
`
	if e = os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRFIX_TEST_REMOTE", bare)
	t.Setenv("PRFIX_TEST_GIT", realGit)
	t.Setenv("PRFIX_TEST_PUSHES", pushes)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-split"}}
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"user-split", "Fix this PR, including automatic scope partitioning and publication.", "chat:1"}}
	prepared, e := Prepare(ctx, root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{failCheckpoint: "publish", reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind != "scope" {
			return
		}
		report.Scope = &ScopeReport{AddedLines: 0, Threshold: 1000, EvacuateMode: "automatic", RelatedMode: "automatic", Enabled: true, Assessment: "Fixture independent boundary.", Evidence: []string{s.Evidence}, Boundaries: []SplitBoundary{{Branch: "split/fixture", Head: split, Base: original, Behavior: "split_related", Paths: []string{"code.txt"}, Reason: "Independently reviewable fixture.", Evidence: []string{s.Evidence}, RecoveryRef: recovery}}}
		report.Operations = []Intent{
			{ID: "split-push", Kind: "split_push", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"branch": "split/fixture", "head": split, "behavior": "split_related"}, Sources: []string{s.Evidence}},
			{ID: "split-pr", Kind: "split_pr", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"title": "Independent fixture", "body": "Review the prepared independent fixture.", "base": "main", "head": "owner:split/fixture", "draft": true, "push_operation": "split-push"}, Sources: []string{s.Evidence}},
		}
	}}
	ex, run, _ := executeFixture(t, root, provider, prepared, session, true)
	run, e = resumeFixture(t, ex, run.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if run.Status != workflows.RunStatusSucceeded || provider.writes != 1 {
		t.Fatal(run.Status, provider.writes)
	}
	actual, e := git(ctx, bare, "rev-parse", "refs/heads/split/fixture")
	if e != nil || actual != split {
		t.Fatal(actual, e)
	}
	current, e := git(ctx, root, "rev-parse", "HEAD")
	if e != nil || current != original {
		t.Fatal("original branch changed", current, e)
	}
	log, e := os.ReadFile(pushes)
	if e != nil || strings.Count(string(log), "push\n") != 1 {
		t.Fatal("lost push was replayed", string(log), e)
	}
}
func TestMergeRequiresSeparateAuthorityAndLiveRepositoryRequirements(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked", true: "ready"}[ready], func(t *testing.T) {
			root, provider, req := fixture(t)
			provider.mergeReady = ready
			req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "dk-workflows:dk-pr-description"}}
			req.Adapters = map[string]Adapter{"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "review", Role: "leaf", Assignment: "Review this exact diff.", Evidence: []string{"original"}}}}}}
			req.Authorizations = map[string]Authorization{"merge": {"user-merge", "Merge this PR when verified ready.", "chat:merge"}}
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Kind == "description" {
					report.Operations = []Intent{{ID: "merge-once", Kind: "merge", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"sha": s.Head, "merge_method": "squash"}, Sources: []string{s.Evidence}}}
				}
			}}
			if ready {
				_, run, _ := executeFixture(t, root, provider, prepared, session)
				if run.Status != workflows.RunStatusSucceeded || provider.writes != 1 {
					t.Fatal(run.Status, provider.writes)
				}
			} else {
				_, _, _ = executeFixture(t, root, provider, prepared, session, true)
				if provider.writes != 0 {
					t.Fatal("merge preceded live requirements", provider.writes)
				}
			}
		})
	}
}
func TestWrongAudienceFingerprintBlocksActualWrite(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description"}}
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"user", "Fix and publish this PR.", "chat"}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "reply", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "Prepared reply."}, Sources: []string{s.Evidence}}}
		}
		if o.Kind == "audience" {
			report.Audience.Fingerprint = "different"
		}
	}}
	_, _, _ = executeFixture(t, root, provider, prepared, session, true)
	if provider.writes != 0 {
		t.Fatal("publication ignored exact payload review", provider.writes)
	}
}

func TestReplyAndNoReplyObligationsRefreshWithoutDuplicateAnswer(t *testing.T) {
	root, provider, req := fixture(t)
	provider.discussionWrites = true
	provider.ev.Comments = []map[string]any{{"id": float64(9), "body": "Please reply when verified.", "user": map[string]any{"login": "author"}}}
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-read"}}
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-reply"}, map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"reply-authority", "Fix this PR and reply to the request.", "chat:reply"}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "inspection" {
			report.ReplyObligations = []ReplyObligation{{ID: "request-9", Text: "Please reply when verified.", Required: true, Reason: "Concrete author request.", Sources: []string{s.Evidence}}, {ID: "optional-10", Text: "Optional discussion question.", Required: false, Reason: "Other participant's optional discussion.", Sources: []string{s.Evidence}}}
		}
	}, reportReplies: func(o Occurrence, s Snapshot, obligations []retainedObligation, report *Report) {
		if o.Kind != "reply" {
			return
		}
		for _, item := range obligations {
			response := ReplyResponse{ID: item.Obligation.ID, Outcome: "no_reply", Reason: "Optional discussion needs no response.", Sources: []string{item.Original}}
			if item.Obligation.Required {
				response.Outcome = "reply"
				response.Operation = "answer-9"
				response.Reason = "Prepared the verified answer."
			}
			report.Responses = append(report.Responses, response)
		}
		report.Operations = []Intent{{ID: "answer-9", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The correction has been verified."}, Sources: []string{s.Evidence}}}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true || provider.writes != 1 {
		t.Fatal("reply obligations did not converge", run.Outputs, provider.writes)
	}
	if session.calls["completion"] != 2 {
		t.Fatal("discussion changes were not refreshed", session.calls)
	}
}
func TestRequiredNoReplyNeedsExactUserException(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocked", true: "explicit-exception"}[allowed], func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-read"}}
			req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-reply"}}
			a := Authorization{"exception", "Do not reply to this specific request.", "chat:exception"}
			if allowed {
				req.Authorizations = map[string]Authorization{"no-reply:request": a}
			}
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Kind == "inspection" {
					report.ReplyObligations = []ReplyObligation{{ID: "request", Text: "Please reply.", Required: true, Reason: "Author requested a response.", Sources: []string{s.Evidence}}}
				}
			}, reportReplies: func(o Occurrence, _ Snapshot, obligations []retainedObligation, report *Report) {
				if o.Kind == "reply" {
					for _, item := range obligations {
						report.Responses = append(report.Responses, ReplyResponse{ID: item.Obligation.ID, Outcome: "no_reply", Reason: "User declined a reply.", Sources: []string{item.Original}, Acceptance: &a})
					}
				}
			}}
			if allowed {
				_, run, _ := executeFixture(t, root, provider, prepared, session)
				if run.Outputs["selected_complete"] != true {
					t.Fatal(run.Outputs)
				}
			} else {
				_, _, _ = executeFixture(t, root, provider, prepared, session, true)
			}
			if provider.writes != 0 {
				t.Fatal("no-reply exception published an answer")
			}
		})
	}
}
