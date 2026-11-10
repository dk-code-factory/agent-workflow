package prfix

import (
	"context"
	"testing"
	"time"
)

func TestInactiveReviewerHooksDoNotDeferSelectedCI(t *testing.T) {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		t.Run(phase, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["finalizers"] = []any{"dk-workflows:dk-ci"}
			req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{phase: []any{"custom:review"}}}}})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := ReadPlan(prepared.Plan)
			if err != nil || len(plan.Groups[phase]) != 1 {
				t.Fatal("review hook was not retained in the prepared plan", plan.Groups, err)
			}
			runtime := &Runtime{Root: root, Provider: provider}
			current, missing, err := runtime.reviewCurrent(ctx, plan, plan.Snapshot)
			if err != nil || !current || len(missing) != 0 {
				t.Fatal("inactive review hook blocked selected CI", current, missing, err)
			}
			session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Kind != "ci" {
					return
				}
				path, err := runtime.latestCI(ctx, plan, s.Head)
				if err != nil {
					t.Error(err)
					return
				}
				report.CI = &CIReport{Head: s.Head, Status: "success", Checks: []string{"1"}, Evidence: []string{path}}
			}}
			_, run, observed := executeFixture(t, root, provider, prepared, session)
			if run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != false || len(session.calls) != 2 || session.calls["completion"] != 1 || session.calls[plan.Groups["finalizers"][0][0]] != 1 {
				t.Fatal("inactive reviewer ran or selected CI did not complete", run.Outputs, session.calls)
			}
			if _, err := observed.latestCI(ctx, plan, plan.Snapshot.Head); err != nil {
				t.Fatal("selected CI was not observed", err)
			}
		})
	}
}

func TestReplacementWithoutReviewersKeepsHistoricalHooksInactive(t *testing.T) {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		t.Run(phase, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{"custom:review"}
			req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			_, initial, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
			if initial.Outputs["selected_complete"] != true {
				t.Fatal("initial review did not settle", initial.Outputs)
			}
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{phase: []any{"dk-workflows:dk-ci"}}}}})
			req.Run = prepared.LogicalRun
			req.Options["tasks"] = []any{}
			replacement, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := ReadPlan(replacement.Plan)
			if err != nil || len(plan.Groups[phase]) != 1 {
				t.Fatal("replacement hook was not retained", plan.Groups, err)
			}
			selected, hooks, cycle, err := runtime.selectedOccurrences(ctx, plan)
			if err != nil || cycle == "" || len(hooks) != 1 {
				t.Fatal("fixture lost historical review reservation or current hook", cycle, hooks, err)
			}
			for id := range hooks {
				if selected[id] {
					t.Fatal("historical review activated a hook omitted from replacement YAML", id)
				}
			}
			session := &fakeSession{}
			ex, run, runtime := executeFixture(t, root, provider, replacement, session)
			if run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != false || len(session.calls) != 1 || session.calls["completion"] != 1 {
				t.Fatal("historical hook restored omitted work", run.Outputs, session.calls)
			}
			if _, err := runtime.latestCI(ctx, plan, plan.Snapshot.Head); err == nil {
				t.Fatal("omitted replacement CI hook produced an observation")
			}
			if _, err := resumeFixture(t, ex, run.ID, false); err != nil {
				t.Fatal("historical hook blocked replacement checkpoint reuse", err)
			}
		})
	}
}

func TestAfterCycleReviewerDoesNotBlockEarlierFinalizerCI(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["finalizers"] = []any{"custom:review", "dk-workflows:dk-ci"}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"after_cycle": []any{"custom:review"}}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{Root: root, Provider: provider}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "ci" {
			path, err := runtime.latestCI(ctx, plan, s.Head)
			if err != nil {
				t.Error(err)
				return
			}
			report.CI = &CIReport{Head: s.Head, Status: "success", Checks: []string{"1"}, Evidence: []string{path}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	for _, id := range []string{plan.Groups["finalizers"][0][0], plan.Groups["finalizers"][1][0], plan.Groups["after_cycle"][0][0], "completion"} {
		if session.calls[id] != 1 {
			t.Fatal("selected finalizer or after-cycle reviewer did not execute", id, session.calls)
		}
	}
	if run.Outputs["selected_complete"] != true {
		t.Fatal("after-cycle reviewer prevented selected completion", run.Outputs)
	}
}

func TestReplacementHookChangesAdmitNewReviewCycle(t *testing.T) {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		t.Run(phase, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{"custom:review"}
			req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{}}}})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			_, _, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{phase: []any{map[string]any{"instructions": "New hook for future cycles."}}}}}})
			req.Run = prepared.LogicalRun
			replacement, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := ReadPlan(replacement.Plan)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := ReadPlan(prepared.Plan)
			if err != nil {
				t.Fatal(err)
			}
			live, err := runtime.live(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			oldReview, _ := previous.Occurrence(previous.Groups["tasks"][0][0])
			newReview, _ := plan.Occurrence(plan.Groups["tasks"][0][0])
			if occurrenceFingerprint(previous.Snapshot, oldReview) == occurrenceFingerprint(live, newReview) {
				t.Fatal("changed governing hook configuration did not invalidate review")
			}
			session := &fakeSession{}
			ex, run, _ := executeFixture(t, root, provider, replacement, session)
			if run.Outputs["selected_complete"] != true || session.calls["completion"] != 1 || session.calls[newReview.ID] != 1 || session.calls[plan.Groups[phase][0][0]] != 1 {
				t.Fatal("replacement review or newly admitted hook did not complete", run.Outputs, session.calls)
			}
			ledger, err := runtime.ledger()
			if err != nil {
				t.Fatal(err)
			}
			var cycles int
			err = ledger.db.QueryRowContext(ctx, "SELECT count(*) FROM reservations WHERE run=? AND kind='review'", plan.LogicalRun).Scan(&cycles)
			ledger.Close()
			if err != nil || cycles != 2 {
				t.Fatal("replacement failed to retain both review cycles", cycles, err)
			}
			if _, err := resumeFixture(t, ex, run.ID, false); err != nil {
				t.Fatal("replacement cache reuse failed", err)
			}
		})
	}
}
