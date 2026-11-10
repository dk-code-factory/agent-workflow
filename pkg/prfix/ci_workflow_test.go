package prfix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func ciReportFor(root string, p Plan, s Snapshot) (*CIReport, error) {
	path, e := (&Runtime{Root: root}).latestCI(context.Background(), p, s.Head)
	if e != nil {
		return nil, e
	}
	var obs CIObservation
	if e = readJSON(path, &obs); e != nil {
		return nil, e
	}
	ids := []string{}
	for _, check := range obs.Checks {
		ids = append(ids, check.ID)
	}
	return &CIReport{Head: s.Head, Checks: ids, Evidence: []string{path}}, nil
}
func TestCIRepairCycleZeroReturnsThroughSelectedCleanup(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-ci"}, map[string]any{"skill": "dk-workflows:dk-commit-split"}}
	provider.ci.Checks[0].Conclusion = "failure"
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	order := []string{}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		order = append(order, o.Kind)
		switch o.Kind {
		case "ci":
			var err error
			report.CI, err = ciReportFor(root, p, s)
			if err != nil {
				t.Error(err)
				report.Checklist = nil
				report.Complete = false
				report.Blockers = append(report.Blockers, "CI fixture evidence unavailable")
				return
			}
			report.CI.Repair = provider.ci.Checks[0].Conclusion == "failure"
			if report.CI.Repair {
				report.CI.RepairReason = "The pinned unit check requires the bounded fixture correction."
			}
		case "ci_repair":
			if e := os.WriteFile(filepath.Join(root, "code.txt"), []byte("fixed\n"), 0600); e != nil {
				t.Error(e)
			}
			report.CommitMessage = "Fix the evidenced CI failure"
			report.Changes = []FileChange{{Path: "code.txt", Before: rawHash([]byte("baseline\n")), After: rawHash([]byte("fixed\n"))}}
		case "cleanup":
			series, e := git(context.Background(), root, "rev-list", s.Target+".."+s.LocalHead)
			if e != nil {
				t.Error(e)
			}
			mapping := map[string]string{}
			for _, sha := range strings.Fields(series) {
				mapping[sha] = sha
			}
			report.Cleanup = &Cleanup{Outcome: "no_op", ReviewedHead: s.LocalHead, FinalHead: s.LocalHead, FinalTree: s.LocalTree, Commits: mapping, BoundaryReasons: []string{"The repair is already one focused commit."}, Validation: []string{s.Evidence}}
		}
	}}
	// Simulate a trusted external test fixture observing the committed head. This
	// is not a native publication, GitHub call, or bypass of a production gate.
	session.checkpointHook = func(name string, _ ActionOutput) error {
		if name == "commit" {
			head, e := git(context.Background(), root, "rev-parse", "HEAD")
			if e != nil {
				return e
			}
			if head != provider.ev.Metadata.Head.SHA {
				provider.ev.Metadata.Head.SHA = head
				provider.ci.Checks[0].Conclusion = "success"
			}
		}
		return nil
	}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true {
		t.Fatal(run.Outputs)
	}
	joined := strings.Join(order, ",")
	if !strings.Contains(joined, "ci,ci_repair,cleanup,completion,ci,cleanup") {
		t.Fatal("repair skipped selected cleanup before observing resulting head", joined)
	}
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var repairs int
	var scope string
	if e = l.db.QueryRow("SELECT count(*),scope FROM reservations WHERE kind='ci_repair'").Scan(&repairs, &scope); e != nil || repairs != 1 || scope != "0" {
		t.Fatal("cycle-zero budget lost", repairs, scope, e)
	}
}
func TestLostCIRerunResponseReconcilesWithoutSecondRerun(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-ci"}, map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"user-ci", "Repair and rerun this PR's failed CI when eligible.", "chat:ci"}}
	provider.ci.Checks[0].Conclusion = "failure"
	provider.ci.Checks[0].Attempt = 2
	provider.lose = true
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind != "ci" {
			return
		}
		var err error
		report.CI, err = ciReportFor(root, p, s)
		if err != nil {
			t.Error(err)
			report.Checklist = nil
			report.Complete = false
			report.Blockers = append(report.Blockers, "CI fixture evidence unavailable")
			return
		}
		if provider.ci.Checks[0].Conclusion == "failure" {
			report.CI.RerunReason = "The exact observed attempt has a transient fixture failure."
			report.Operations = []Intent{{ID: "rerun-one", Kind: "ci_rerun", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"run_id": "11", "attempt": float64(2)}, Sources: report.CI.Evidence}}
		}
	}}
	ex, run, _ := executeFixture(t, root, provider, prepared, session, true)
	run, e = resumeFixture(t, ex, run.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if run.Status != workflows.RunStatusSucceeded || provider.writes != 1 {
		t.Fatal("CI rerun duplicated", run.Status, provider.writes)
	}
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var reruns int
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='ci_rerun'").Scan(&reruns); e != nil || reruns != 1 {
		t.Fatal("rerun budget reset", reruns, e)
	}
}
