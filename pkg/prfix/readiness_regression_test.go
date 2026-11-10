package prfix

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestFlakeCannotExemptUnrelatedBuildFailure(t *testing.T) {
	root, _, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head := strings.Repeat("a", 40)
	responses := map[string]any{
		"repos/owner/repo/actions/runs/11": map[string]any{"head_sha": head, "run_attempt": 2, "path": ".github/workflows/test.yml@main"},
		"repos/owner/repo/actions/jobs/22": map[string]any{"run_id": 11, "run_attempt": 2, "name": "unit", "conclusion": "failure", "steps": []ciStep{{Number: 1, Name: "Run tests", Status: "completed", Conclusion: "failure"}}, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/33"},
		"repos/owner/repo/issues/99":       map[string]any{"state": "open", "html_url": "https://github.com/owner/repo/issues/99", "title": "Known flaky test.", "body": "`.github/workflows/test.yml` `unit` `TestIntermittent` `reset`"},
	}
	update := fakeGH(t, root, responses)
	c := CIClassification{CheckID: "33", RunID: "11", JobID: "22", Attempt: 2, Workflow: ".github/workflows/test.yml", Job: "unit", Kind: "flake", Test: "TestIntermittent", Signature: "reset", Issue: "https://github.com/owner/repo/issues/99"}
	for _, other := range []string{"", "# example/other\nother.go:8:2: undefined: absent\nFAIL example/other [build failed]\n", "ERROR collecting tests/test_other.py\nSyntaxError: invalid syntax\n", "WARNING: DATA RACE\n"} {
		responses["repos/owner/repo/actions/jobs/22/logs"] = "--- FAIL: TestIntermittent (0.0s)\nreset\nFAIL example/test 1.0s\n" + other
		update(responses)
		proof, err := (&GitHub{Root: root}).VerifyCI(context.Background(), id, head, []CIClassification{c}, nil)
		if other == "" {
			if err != nil || !proof.Exempt {
				t.Fatal(proof, err)
			}
		} else if err == nil || proof.Exempt {
			t.Fatal("flake hid unrelated failure", other, proof, err)
		}
	}
}

func TestFractionalRerunIsRejectedBeforeIntentOrDispatch(t *testing.T) {
	root, provider, req := fixture(t)
	req.Authorizations = map[string]Authorization{"iterate": {"ci", "Rerun diagnosed failed checks.", "chat:ci"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	id := p.Identity
	fakeGH(t, root, map[string]any{"repos/owner/repo/actions/runs/11": map[string]any{"head_sha": p.Snapshot.Head, "run_attempt": 2, "status": "completed", "conclusion": "failure"}})
	for _, attempt := range []float64{2.5, 0, -1, math.Inf(1), math.NaN(), 1 << 53} {
		intent := Intent{ID: "bad", Kind: "ci_rerun", PR: id, Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Payload: map[string]any{"run_id": "11", "attempt": attempt}, Sources: []string{p.Snapshot.Evidence}}
		if err = ledger.PrepareIntent(context.Background(), p, intent, p.Snapshot.Evidence); err == nil {
			t.Fatal("invalid attempt persisted", attempt)
		}
		if err = (&GitHub{Root: root}).VerifyRerun(context.Background(), intent); err == nil {
			t.Fatal("invalid attempt verified", attempt)
		}
		if _, err = (&GitHub{Root: root}).Execute(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "integral Actions attempt") {
			t.Fatal("invalid attempt reached transport", attempt, err)
		}
	}
	var effects int
	if err = ledger.db.QueryRow("SELECT count(*) FROM effects").Scan(&effects); err != nil || effects != 0 || provider.writes != 0 {
		t.Fatal(effects, provider.writes, err)
	}
	valid := Intent{Kind: "ci_rerun", PR: id, Head: p.Snapshot.Head, Payload: map[string]any{"run_id": "11", "attempt": float64(2)}}
	if err = (&GitHub{Root: root}).VerifyRerun(context.Background(), valid); err != nil {
		t.Fatal("integral attempt refused", err)
	}
}

func TestPartitionOptOutBlocksNewAudienceClaim(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	base, err := materializeSource(root)
	if err != nil {
		t.Fatal(err)
	}
	script := "import sys;sys.path.insert(0,sys.argv[1]);from shared import pr_partition_policy as p;p.configure(sys.argv[2],'disable',reason='Fixture opt-out',source='fixture:user')"
	if _, err = runCommand(context.Background(), root, nil, "python3", "-I", "-B", "-c", script, filepath.Join(base, "source/runtime"), req.PR); err != nil {
		t.Fatal(err)
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := &Runtime{Root: root, Provider: provider, Store: store}
	_, err = runtime.claim(context.Background(), p, workflows.ActionContext{}, ActionInput{Occurrence: "audience", Cycle: "1", Attempt: 1})
	if err == nil || !strings.Contains(err.Error(), "selected configuration changed") {
		t.Fatal("new audience accepted superseded opt-out", err)
	}
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var claims int
	if err = ledger.db.QueryRow("SELECT count(*) FROM claims").Scan(&claims); err != nil || claims != 0 || provider.writes != 0 {
		t.Fatal(claims, provider.writes, err)
	}
}

func TestMergeCannotOmitReviewedUnpublishedHead(t *testing.T) {
	root, provider, req := fixture(t)
	if err := os.WriteFile(filepath.Join(root, "code.txt"), []byte("reviewed local fix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), root, "commit", "-am", "Fix local behavior"); err != nil {
		t.Fatal(err)
	}
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "dk-workflows:dk-pr-description"}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	req.Authorizations = map[string]Authorization{"merge": {"merge", "Merge the reviewed PR when ready.", "chat:merge"}}
	provider.mergeReady = true
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "merge-old", Kind: "merge", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"sha": s.Head, "merge_method": "squash"}, Sources: []string{s.Evidence}}}
		}
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if provider.writes != 0 || !strings.Contains(run.Error, "reviewed local head and clean tree to be published") {
		t.Fatal("merge omitted reviewed fix", provider.writes, run.Error)
	}
	p, _ := ReadPlan(prepared.Plan)
	reports, err := runtime.reports(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	acceptedCompletion := false
	for _, rr := range reports {
		acceptedCompletion = acceptedCompletion || rr.Occurrence.Kind == "completion" && rr.Claim.State == "complete"
	}
	if !acceptedCompletion || session.calls["completion"] != 1 {
		t.Fatal("fixture did not reach accepted completion", session.calls)
	}
}

type rerunPendingProvider struct {
	*fakeProvider
	observations int
}

func (p *rerunPendingProvider) ObserveCI(ctx context.Context, id Identity, head string) (CIObservation, error) {
	p.observations++
	obs, err := p.fakeProvider.ObserveCI(ctx, id, head)
	if p.observations == 1 {
		obs.Checks = append([]CICheck(nil), obs.Checks...)
		obs.Checks[0].Status = "in_progress"
		obs.Checks[0].Conclusion = ""
	}
	return obs, err
}
func TestVerifiedRerunOpensOneRetainedAttemptWindow(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-ci", "options": map[string]any{"timeout_seconds": 2, "max_pending_extensions": 0}}}
	req.Authorizations = map[string]Authorization{"iterate": {"ci", "Rerun diagnosed failures.", "chat:ci"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	now := time.Now()
	old := now.Add(-time.Hour)
	if _, err = ledger.Deadline(context.Background(), p.LogicalRun, p.Snapshot.Head, old, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, err = ledger.Extend(context.Background(), p.LogicalRun, p.Snapshot.Head, time.Second, 1); err != nil {
		t.Fatal(err)
	}
	intent := Intent{ID: "rerun-window", Kind: "ci_rerun", Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Payload: map[string]any{"run_id": "11", "attempt": float64(2)}, Sources: []string{p.Snapshot.Evidence}}
	if err = ledger.PrepareIntent(context.Background(), p, intent, p.Snapshot.Evidence); err != nil {
		t.Fatal(err)
	}
	intent, err = ledger.Intent(context.Background(), p.LogicalRun, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.db.Exec("UPDATE effects SET state='dispatched'"); err != nil {
		t.Fatal(err)
	}
	provider.lose = true
	provider.ci.Checks[0].Attempt = 2
	if _, err = provider.Execute(context.Background(), intent); err == nil {
		t.Fatal("lost response fixture did not fire")
	}
	observer := &rerunPendingProvider{fakeProvider: provider}
	runtime := &Runtime{Root: root, Provider: observer, Now: func() time.Time { return now }, PollInterval: time.Millisecond}
	if _, err = runtime.publish(context.Background(), p, ActionInput{Intent: intent.ID}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := runtime.ci(ctx, p, ActionInput{Occurrence: p.Groups["tasks"][0][0]})
	if err != nil || out.Report == "" || observer.observations != 2 {
		t.Fatal("new pending attempt inherited expired window", out, err, observer.observations)
	}
	first, err := ledger.Deadline(context.Background(), p.LogicalRun, p.Snapshot.Head, now.Add(time.Hour), time.Hour)
	if err != nil || !first.Equal(now.Add(2*time.Second)) {
		t.Fatal("new deadline not bound to verification time", first, err)
	}
	now = now.Add(time.Second)
	if _, err = runtime.publish(context.Background(), p, ActionInput{Intent: intent.ID}); err != nil {
		t.Fatal(err)
	}
	retained, err := ledger.Deadline(context.Background(), p.LogicalRun, p.Snapshot.Head, now, time.Hour)
	var windows int
	if e := ledger.db.QueryRow("SELECT count(*) FROM ci_windows").Scan(&windows); e != nil {
		t.Fatal(e)
	}
	if err != nil || !retained.Equal(first) || windows != 1 || provider.writes != 1 {
		t.Fatal("recovery reset window or duplicated effect", retained, windows, provider.writes, err)
	}
	var oldExtensions int
	if err = ledger.db.QueryRow("SELECT extensions FROM deadlines WHERE run=? AND head=?", p.LogicalRun, p.Snapshot.Head).Scan(&oldExtensions); err != nil || oldExtensions != 1 {
		t.Fatal("prior window erased", oldExtensions, err)
	}
}

func TestUnsupportedCIDurationBlocksBeforeClaims(t *testing.T) {
	for _, key := range []string{"timeout_seconds", "poll_interval_seconds", "classification_timeout_seconds", "no_checks_grace_seconds", "pending_extension_seconds"} {
		t.Run(key, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-ci", "options": map[string]any{key: 10000000000}}}
			if _, err := Prepare(context.Background(), root, provider, req); err == nil || !strings.Contains(err.Error(), "supported integral duration range") {
				t.Fatal("overflow accepted", err)
			}
			if provider.writes != 0 {
				t.Fatal(provider.writes)
			}
			if _, err := os.Stat(filepath.Join(root, ".agent-workflow/pr-fix/ledger.sqlite")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("duration rejection created bookkeeping", err)
			}
		})
	}
	options := map[string]any{"timeout_seconds": float64(math.MaxInt64 / int64(time.Second))}
	if err := validateCIOptions(options); err != nil || ciSeconds(options, "timeout_seconds", 0) <= 0 {
		t.Fatal("representable boundary rejected", err)
	}
}
