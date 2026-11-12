package prfix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestOptionalAndRequiredIncompleteReports(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional", true: "required"}[required], func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect only this bounded task.", "required": required}}
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{reportHook: func(o Occurrence, _ Snapshot, report *Report) {
				if o.Kind == "hook" {
					report.Complete = false
					report.Blockers = []string{"fixture blocker"}
					for n := range report.Checklist {
						report.Checklist[n] = Check{ID: report.Checklist[n].ID, Status: "unfinished", Reason: "fixture blocker"}
					}
				}
			}}
			if required {
				_, run, _ := executeFixture(t, root, provider, prepared, session, true)
				if run.Status == workflows.RunStatusSucceeded || session.calls["completion"] != 0 {
					t.Fatal(run.Status, session.calls)
				}
			} else {
				_, run, _ := executeFixture(t, root, provider, prepared, session)
				if run.Status != workflows.RunStatusSucceeded || session.calls["completion"] != 1 {
					t.Fatal(run.Status, session.calls)
				}
			}
		})
	}
}
func TestUsageRetryStaysInSameCycleAndPreservesNativeFailure(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect only this bounded task."}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	var failed atomic.Bool
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Kind == "hook" && failed.CompareAndSwap(false, true) {
			return errors.New("provider usage limit")
		}
		return nil
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if run.Status != workflows.RunStatusSucceeded {
		t.Fatal("eligible retry failed in the same execution", run.Status)
	}
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var retries, cycles int
	var distinct int
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='retry'").Scan(&retries); e != nil {
		t.Fatal(e)
	}
	if e = l.db.QueryRow("SELECT count(DISTINCT cycle) FROM claims WHERE occurrence<>'completion'").Scan(&distinct); e != nil {
		t.Fatal(e)
	}
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='review'").Scan(&cycles); e != nil {
		t.Fatal(e)
	}
	if retries != 1 || distinct != 1 || cycles != 0 {
		t.Fatal("retry changed cycles or reset budgets", retries, distinct, cycles)
	}
	for id, calls := range session.calls {
		if id != "completion" && calls != 2 {
			t.Fatal("successful retry repeated", session.calls)
		}
	}
	assertAcknowledgedNativeFailure(t, runtime, run.ID)
}
func TestFocusedCommitPreservesUnrelatedFiles(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-address"}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	var changed atomic.Bool
	session := &fakeSession{reportHook: func(o Occurrence, _ Snapshot, report *Report) {
		if o.Kind == "fix" && changed.CompareAndSwap(false, true) {
			if e := os.WriteFile(filepath.Join(root, "code.txt"), []byte("corrected\n"), 0600); e != nil {
				t.Error(e)
			}
			if e := os.WriteFile(filepath.Join(root, "user-notes.txt"), []byte("keep private notes\n"), 0600); e != nil {
				t.Error(e)
			}
			report.CommitMessage = "Fix the evidenced behavior"
			report.Changes = []FileChange{{Path: "code.txt", Before: rawHash([]byte("baseline\n")), After: rawHash([]byte("corrected\n"))}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != false {
		t.Fatal(run.Outputs)
	}
	committed, e := git(context.Background(), root, "show", "--format=", "--name-only", "HEAD")
	if e != nil || committed != "code.txt" {
		t.Fatal("unrelated data entered commit", committed, e)
	}
	notes, e := os.ReadFile(filepath.Join(root, "user-notes.txt"))
	if e != nil || string(notes) != "keep private notes\n" {
		t.Fatal("unrelated file changed", e)
	}
}
func TestReplacementRetainsBudgetsSiblingsAndReconcilesLostPublication(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description"}}
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"user-1", "Fix and publish this PR.", "chat:1"}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{failCheckpoint: "publish", reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "single-reply", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "Prepared correction completed."}, Sources: []string{s.Evidence}}}
		}
	}}
	_, _, _ = executeFixture(t, root, provider, prepared, session, true)
	old, _ := ReadPlan(prepared.Plan)
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if e = l.Reserve(context.Background(), prepared.LogicalRun, "review", "all", "old-cycle", "selected reviewer pass"); e != nil {
		t.Fatal(e)
	}
	provider.ev.Comments = append(provider.ev.Comments, map[string]any{"id": 77, "body": "New author instruction", "user": map[string]any{"login": "author"}})
	resume := ResumeRequest(old, req.Config)
	resume.Capabilities = req.Capabilities
	next, e := Prepare(context.Background(), root, provider, resume)
	if e != nil {
		t.Fatal(e)
	}
	if next.LogicalRun != prepared.LogicalRun || next.Workflow == prepared.Workflow || provider.writes != 1 {
		t.Fatal("replacement identity/effect changed", next, provider.writes)
	}
	var state string
	var cycles int
	if e = l.db.QueryRow("SELECT state FROM effects WHERE id='single-reply'").Scan(&state); e != nil || state != "completed" {
		t.Fatal("lost write not reconciled", state, e)
	}
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='review'").Scan(&cycles); e != nil || cycles != 1 {
		t.Fatal("replacement reset budget", cycles, e)
	}
}
func TestNoAuthorityCannotPreparePublication(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	e = l.PrepareIntent(context.Background(), p, Intent{Kind: "reply"}, "source")
	if e == nil || !strings.Contains(e.Error(), "authorization") {
		t.Fatal("configuration granted publication", e)
	}
	req.Authorizations = map[string]Authorization{"iterate": {"request", "Iterate this PR.", "chat"}}
	p.Authorizations = req.Authorizations
	e = l.PrepareIntent(context.Background(), p, Intent{Kind: "merge"}, "source")
	if e == nil || !strings.Contains(e.Error(), "authorization") {
		t.Fatal("iterate granted merge", e)
	}
}

func TestDefaultPlanExecutesAllDutiesWithFakeCIAndCleanup(t *testing.T) {
	testFinalizerDuties(t, false)
}
func TestEquivalentCustomCIFinalizerCanEstablishReadiness(t *testing.T) {
	testFinalizerDuties(t, true)
}
func testFinalizerDuties(t *testing.T, customCI bool) {
	t.Helper()
	root, provider, req := fixture(t)
	req.Options = nil
	configFile(t, "", map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-reviewers-run": map[string]any{"options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review"}}}}}})
	req.Adapters = map[string]Adapter{"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "required", Role: "codex", Assignment: "Review the pinned request and diff.", Evidence: []string{"original"}}}}}}
	if customCI {
		a := BuiltinAdapters("leaf")["dk-workflows:dk-ci"]
		a.Identity = "custom:ci"
		req.Adapters[a.Identity] = a
		req.Options = map[string]any{"finalizers": []any{"dk-workflows:dk-commit-split", "dk-workflows:dk-pr-decisions", "dk-workflows:dk-pr-description", "dk-workflows:dk-pr-feedback-reply", "custom:ci", "dk-workflows:dk-pr-publish"}}
	}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	runtime := &Runtime{Root: root, Provider: provider}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		switch o.Kind {
		case "scope":
			a, e := runtime.scopeAccounting(context.Background(), p, o)
			if e != nil {
				t.Error(e)
				return
			}
			report.Scope = &ScopeReport{AddedLines: a.AddedLines, Threshold: a.Threshold, EvacuateMode: a.EvacuateMode, RelatedMode: a.RelatedMode, Enabled: a.Enabled, Assessment: "All work belongs to the pinned request.", Evidence: []string{s.Evidence}}
		case "cleanup":
			series, e := git(context.Background(), root, "rev-list", s.Target+".."+s.LocalHead)
			if e != nil {
				t.Error(e)
				return
			}
			mapping := map[string]string{}
			for _, sha := range strings.Fields(series) {
				mapping[sha] = sha
			}
			report.Cleanup = &Cleanup{Outcome: "no_op", ReviewedHead: s.LocalHead, FinalHead: s.LocalHead, FinalTree: s.LocalTree, Commits: mapping, BoundaryReasons: []string{"The complete existing series already matches policy."}, Validation: []string{s.Evidence}}
		case "ci":
			path, e := runtime.latestCI(context.Background(), p, s.Head)
			if e != nil {
				t.Error(e)
				return
			}
			var obs CIObservation
			if e = readJSON(path, &obs); e != nil {
				t.Error(e)
				return
			}
			ids := []string{}
			for _, check := range obs.Checks {
				ids = append(ids, check.ID)
			}
			report.CI = &CIReport{Head: s.Head, Status: "success", Checks: ids, Evidence: []string{path}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != true || provider.writes != 0 {
		t.Fatal("complete default plan did not converge", run.Outputs, provider.writes)
	}
	for _, o := range p.Occurrences {
		if o.ID == "audience" || o.ID == "ci_repair" || o.ID == "finding_verification" || o.ID == "reply_recovery" {
			continue
		}
		if session.calls[o.ID] != 1 {
			t.Fatal("selected default duty missing or repeated", o.ID, session.calls)
		}
	}
}

func TestOriginalFindingSurvivesEmptyChecklistOutputAndGetsSourcedDisposition(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-address"}}
	req.Adapters = map[string]Adapter{"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "correctness", Role: "codex", Assignment: "Review only the pinned diff.", Evidence: []string{"original"}}}}}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	var first atomic.Bool
	session := &fakeSession{omitChecklist: true, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Review && first.CompareAndSwap(false, true) {
			report.Findings = []Finding{{ID: "original-1", Text: "Check the original fixture assumption.", Sources: []string{s.Evidence}}}
		}
	}, reportCandidates: func(o Occurrence, _ Snapshot, candidates []Candidate, report *Report) {
		for _, candidate := range candidates {
			if o.Kind == "verification" {
				report.Findings = append(report.Findings, Finding{ID: candidate.Finding.ID, Text: candidate.Finding.Text, Sources: []string{candidate.Original}, Disposition: "confirmed", Reason: "Independently checked the original candidate."})
			}
			if o.Kind == "fix" {
				report.Findings = append(report.Findings, Finding{ID: candidate.Finding.ID, Text: candidate.Finding.Text, Sources: []string{candidate.Original}, Disposition: "not_actionable", Reason: "The fixture assumption is explicitly satisfied by the pinned source."})
			}
		}
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true {
		t.Fatal(run.Outputs)
	}
	candidates, e := runtime.candidates(context.Background(), p)
	if e != nil || len(candidates) != 1 || !strings.HasPrefix(candidates[0].Finding.ID, "finding_") {
		t.Fatal("original finding erased by empty follow-up", candidates, e)
	}
	reports, e := runtime.reports(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, rr := range reports {
		if rr.Reference == candidates[0].Original && rr.Occurrence.Review && len(rr.Report.Findings) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("finding source does not name the original native output")
	}
}
func TestLaterConfiguredWorkFinishesBeforeAffectedReviewRepeats(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-address"}, map[string]any{"skill": "custom:tail"}}
	req.Adapters = map[string]Adapter{
		"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "review", Role: "codex", Assignment: "Review this exact diff.", Evidence: []string{"original"}}}}},
		"custom:tail":   {Identity: "custom:tail", Version: "v1", Kind: "inspection", Cacheable: true, Procedure: [][]Pass{{{ID: "tail", Role: "codex", Assignment: "Inspect only this final configured duty.", Evidence: []string{"original"}}}}},
	}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	var edited atomic.Bool
	order := []string{}
	session := &fakeSession{reportHook: func(o Occurrence, _ Snapshot, report *Report) {
		order = append(order, o.Kind)
		if o.Kind == "fix" && edited.CompareAndSwap(false, true) {
			if e := os.WriteFile(filepath.Join(root, "code.txt"), []byte("corrected\n"), 0600); e != nil {
				t.Error(e)
			}
			report.CommitMessage = "Fix the bounded fixture behavior"
			report.Changes = []FileChange{{Path: "code.txt", Before: rawHash([]byte("baseline\n")), After: rawHash([]byte("corrected\n"))}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true || !strings.Contains(strings.Join(order, ","), "review,fix,inspection,completion,review,fix,completion") {
		t.Fatal("remaining work or unchanged sibling was repeated in the wrong order", order, run.Outputs)
	}
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var cycles int
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='review'").Scan(&cycles); e != nil || cycles != 2 {
		t.Fatal("affected review cycle not retained", cycles, e)
	}
}
