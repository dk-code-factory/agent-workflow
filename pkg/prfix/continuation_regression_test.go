package prfix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestQueuedWorkerSetupFailureStopsApplicationWait(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	// Spawn and Send preflight succeed; the scheduled native turn's setup fails.
	session := &fakeSession{factoryError: errors.New("fixture runner setup failed"), factoryErrorAfter: 2, failureTimeout: 30 * time.Second}
	_, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if run.Status == workflows.RunStatusSucceeded || !strings.Contains(run.Error, "native worker stopped with unresolved message") || !strings.Contains(run.Error, "fixture runner setup failed") {
		t.Fatal("setup failure was not surfaced before the fixture deadline", run.Status, run.Error)
	}
	workers, err := runtime.Workers.List(context.Background(), run.ID)
	if err != nil || len(workers) != 1 {
		t.Fatal(workers, err)
	}
	messages, err := runtime.Workers.Messages(context.Background(), run.ID, workers[0].ID)
	if err != nil || len(messages) != 1 || !messages[0].StartedAt.IsZero() || messages[0].FailureAcknowledgement != nil || workers[0].FailureAcknowledgement != nil || workers[0].Error != "fixture runner setup failed" {
		t.Fatal("unstarted native evidence was replaced or acknowledged", workers, messages, err)
	}
	if len(session.calls) != 0 {
		t.Fatal("setup failure dispatched a native turn", session.calls)
	}
}

func TestFlakeIssueRequiresExactRepositoryAndAffirmativeScope(t *testing.T) {
	root, _, req := fixture(t)
	id, err := ParseIdentity(req.PR)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	classification := CIClassification{CheckID: "33", RunID: "11", JobID: "22", Attempt: 2, Workflow: ".github/workflows/test.yml", Job: "unit", Kind: "flake", Test: "TestIntermittent", Signature: "connection reset", Issue: "https://github.com/owner/repo/issues/99"}
	contextText := "`.github/workflows/test.yml` `unit` `TestIntermittent` `connection reset`"
	declaration := "```dk-flaky\n" + string(jsonBytes(map[string]any{"workflow": classification.Workflow, "job": classification.Job, "test": classification.Test, "signature": classification.Signature, "flaky": true})) + "\n```"
	responses := map[string]any{
		"repos/owner/repo/actions/runs/11":      map[string]any{"head_sha": head, "run_attempt": 2, "path": classification.Workflow + "@main"},
		"repos/owner/repo/actions/jobs/22":      map[string]any{"run_id": 11, "run_attempt": 2, "name": "unit", "conclusion": "failure", "steps": []ciStep{{Number: 1, Name: "Run tests", Status: "completed", Conclusion: "failure"}}, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/33"},
		"repos/owner/repo/actions/jobs/22/logs": "--- FAIL: TestIntermittent (0.0s)\nconnection reset\nFAIL\n",
	}
	update := fakeGH(t, root, responses)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name, title, body string
		accept            bool
	}{
		{"affirmative prose", "Known flaky test.", contextText, true},
		{"structured declaration", "TestIntermittent", declaration, true},
		{"explicit structured decision", "General non-flaky discussion", declaration, true},
		{"negative", "This test is not flaky", contextText, false},
		{"uncertain", "Known flaky test.", contextText + " Maybe this is intermittent?", false},
		{"vague", "Intermittent TestIntermittent", contextText, false},
		{"unscoped", "Known flaky test.", "TestIntermittent connection reset", false},
		{"other signature", "Known flaky test.", strings.ReplaceAll(contextText, "connection reset", "other error"), false},
		{"explicit false", "Known flaky test.", strings.ReplaceAll(declaration, `"flaky":true`, `"flaky":false`) + "\n" + contextText, false},
		{"another repository", "Known flaky test.", contextText, false},
		{"wrong observed identity", "Known flaky test.", contextText, false},
		{"pull request", "Known flaky test.", contextText, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := classification
			issue := map[string]any{"state": "open", "html_url": c.Issue, "title": tc.title, "body": tc.body}
			if tc.name == "another repository" {
				c.Issue = "https://github.com/other/project/issues/99"
				issue["html_url"] = c.Issue
			}
			if tc.name == "wrong observed identity" {
				issue["html_url"] = "https://github.com/owner/repo/issues/100"
			}
			if tc.name == "pull request" {
				issue["pull_request"] = map[string]any{"url": "https://api.github.com/repos/owner/repo/pulls/99"}
			}
			responses["repos/owner/repo/issues/99"] = issue
			responses["repos/other/project/issues/99"] = issue
			update(responses)
			proof, err := (&GitHub{Root: root}).VerifyCI(ctx, id, head, []CIClassification{c}, nil)
			if tc.accept && (err != nil || !proof.Exempt) || !tc.accept && (err == nil || proof.Exempt) {
				t.Fatal("incorrect affirmative flake disposition", proof, err)
			}
		})
	}
}

func TestSplitBoundaryRejectsSameSizeContentDrift(t *testing.T) {
	root, provider, req := fixture(t)
	ctx := context.Background()
	base := provider.ev.Metadata.Base.SHA
	if err := os.WriteFile(filepath.Join(root, "code.txt"), []byte("first version\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "add", "code.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "commit", "-m", "Prepare original scope"); err != nil {
		t.Fatal(err)
	}
	original, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA = original
	tree, err := git(ctx, root, "rev-parse", original+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	split, err := git(ctx, root, "commit-tree", tree, "-p", base, "-m", "Prepared independent boundary")
	if err != nil {
		t.Fatal(err)
	}
	recovery := "refs/prfix/scope-drift-recovery"
	if _, err = git(ctx, root, "update-ref", recovery, original); err != nil {
		t.Fatal(err)
	}
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-split"}}
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var originalScope *ScopeReport
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "scope" {
			originalScope = &ScopeReport{AddedLines: 1, Threshold: 1000, EvacuateMode: "automatic", RelatedMode: "automatic", Enabled: true, Assessment: "Independent boundary at the original content.", Evidence: []string{s.Evidence}, Boundaries: []SplitBoundary{{Branch: "split/drift", Head: split, Base: base, Behavior: "split_related", Paths: []string{"code.txt"}, Reason: "Independent original work.", Evidence: []string{s.Evidence}, RecoveryRef: recovery}}}
			report.Scope = originalScope
		}
	}}
	_, _, runtime := executeFixture(t, root, provider, prepared, session)
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{Kind: "split_push", Head: original, Lease: original, Target: base, Payload: map[string]any{"branch": "split/drift", "head": split, "behavior": "split_related"}}
	if _, err = runtime.splitBoundary(ctx, p, intent); err != nil {
		t.Fatal("current accepted scope rejected", err)
	}
	o, err := p.Occurrence(p.Groups["tasks"][0][0])
	if err != nil {
		t.Fatal(err)
	}
	before, err := runtime.scopeAccounting(ctx, p, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "code.txt"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.scopeAccounting(ctx, p, o)
	if err != nil || before.AddedLines != 1 || before.AddedLines != after.AddedLines {
		t.Fatal("fixture did not preserve the size count", before, after, err)
	}
	if _, err = git(ctx, root, "add", "code.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err = git(ctx, root, "commit", "-m", "Replace source content"); err != nil {
		t.Fatal(err)
	}
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA = head
	intent.Head, intent.Lease = head, head // A newly prepared current intent, not an old lease.
	if err = runtime.validateScope(ctx, p, o, originalScope); err != nil {
		t.Fatal("counterexample changed the accounting/object checks", err)
	}
	if _, err = runtime.splitBoundary(ctx, p, intent); err == nil {
		t.Fatal("current intent consumed stale scope content")
	}
	if provider.writes != 0 {
		t.Fatal("scope check dispatched a remote effect", provider.writes)
	}
}

func TestRequiredHookResourcesStayFreshWithinSameCycle(t *testing.T) {
	root, provider, req := fixture(t)
	resource := filepath.Join(t.TempDir(), "hook-policy.txt")
	if err := os.WriteFile(resource, []byte("first policy"), 0600); err != nil {
		t.Fatal(err)
	}
	hook := Adapter{Identity: "custom:hook", Version: "v1", Kind: "hook", Cacheable: true, Resources: []string{resource}, Procedure: [][]Pass{{{ID: "hook", Role: "leaf", Assignment: "Check the hook-only policy.", Evidence: []string{"original"}}}}}
	req.Adapters = map[string]Adapter{hook.Identity: hook, "custom:review": reviewAdapter()}
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}}
	configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": []any{map[string]any{"skill": hook.Identity}}}}}})
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	initial, err := runtime.live(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if complete, blockers, err := runtime.coverage(ctx, p, initial); err != nil || !complete {
		t.Fatal("unchanged hook coverage incomplete", blockers, err)
	}
	if err = os.WriteFile(resource, []byte("second policy"), 0600); err != nil {
		t.Fatal(err)
	}
	live, err := runtime.live(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := runtime.reports(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	count, err := completedReviewCount(reports, live)
	if err != nil || initial.LocalTree != live.LocalTree || initial.Resources != live.Resources || count != 1 {
		t.Fatal("hook-only update changed unrelated review inputs")
	}
	complete, blockers, err := runtime.coverage(ctx, p, live)
	hookID := p.Groups["before_cycle"][0][0]
	if err != nil || complete || !strings.Contains(strings.Join(blockers, " "), hookID+": governing evidence stale") {
		t.Fatal("same-cycle required hook accepted stale resource", complete, blockers, err)
	}
}
