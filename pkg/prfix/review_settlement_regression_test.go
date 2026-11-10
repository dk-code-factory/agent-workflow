package prfix

import (
	"context"
	"testing"
	"time"
)

func TestInactiveCIHooksDoNotRestoreOmittedCompletionDuties(t *testing.T) {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		t.Run(phase, func(t *testing.T) {
			root, provider, req := fixture(t)
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{phase: []any{"dk-workflows:dk-ci"}}}}})
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := ReadPlan(prepared.Plan)
			if err != nil || len(plan.Groups[phase]) != 1 {
				t.Fatal("CI hook was not retained in the prepared plan", plan.Groups, err)
			}
			session := &fakeSession{}
			ex, run, runtime := executeFixture(t, root, provider, prepared, session)
			if run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != false || len(session.calls) != 1 || session.calls["completion"] != 1 {
				t.Fatal("inactive CI hook restored omitted work", run.Outputs, session.calls)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := runtime.latestCI(ctx, plan, plan.Snapshot.Head); err == nil {
				t.Fatal("inactive CI hook produced an observation")
			}
			if _, err := resumeFixture(t, ex, run.ID, false); err != nil {
				t.Fatal("inactive CI hook blocked checkpoint reuse", err)
			}
		})
	}
}

func TestFreshReplyAfterReplacementDoesNotAliasSupersededEffect(t *testing.T) {
	root, provider, req := fixture(t)
	provider.discussionWrites = true
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat:publish"}}
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	proposal := func(id string) func(Occurrence, Snapshot, *Report) {
		return func(o Occurrence, s Snapshot, report *Report) {
			if o.Kind == "description" {
				report.Operations = []Intent{{ID: id, Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The current verified answer."}, Sources: []string{s.Evidence}}}
			}
		}
	}
	_, stopped, _ := executeFixture(t, root, provider, prepared, &fakeSession{failCheckpoint: "finish", reportHook: proposal("undispatched")}, true)
	if stopped.Status == "succeeded" || provider.writes != 0 {
		t.Fatal("fixture did not stop with an undispatched proposal", stopped.Status, provider.writes)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var state string
	if err := l.db.QueryRow("SELECT state FROM effects WHERE run=? AND id='undispatched'", prepared.LogicalRun).Scan(&state); err != nil || state != "prepared" {
		t.Fatal("missing original prepared proposal", state, err)
	}
	req.Run = prepared.LogicalRun
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description", "instructions": "Reassess the answer against the replacement plan."}}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	replacement, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.db.QueryRow("SELECT state FROM effects WHERE run=? AND id='undispatched'", req.Run).Scan(&state); err != nil || state != "superseded" {
		t.Fatal("replacement did not supersede the original effect", state, err)
	}
	_, run, _ := executeFixture(t, root, provider, replacement, &fakeSession{reportHook: proposal("fresh")})
	if run.Outputs["selected_complete"] != true || provider.writes != 1 {
		t.Fatal("fresh proposal was swallowed by superseded history", run.Outputs, provider.writes)
	}
	if err := l.db.QueryRow("SELECT state FROM effects WHERE run=? AND id='fresh'", req.Run).Scan(&state); err != nil || state != "completed" {
		t.Fatal("fresh reply did not retain its own completed effect", state, err)
	}
	var aliases int
	if err := l.db.QueryRow("SELECT count(*) FROM effect_aliases WHERE run=? AND id='fresh'", req.Run).Scan(&aliases); err != nil || aliases != 0 {
		t.Fatal("fresh reply was aliased to undispatched history", aliases, err)
	}
}
