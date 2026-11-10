package prfix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestParallelSingletonProceduresRejectBeforeClaims(t *testing.T) {
	for _, identity := range []string{"dk-workflows:dk-pr-publish", "dk-workflows:dk-ci"} {
		t.Run(identity, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description"}, map[string]any{"parallel": []any{map[string]any{"skill": identity}, map[string]any{"skill": identity}}}}
			_, err := Prepare(context.Background(), root, provider, req)
			if err == nil || !strings.Contains(err.Error(), "conflicting parallel") {
				t.Fatal("singleton conflict reached dispatch", err)
			}
			if _, err = os.Stat(filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")); !os.IsNotExist(err) {
				t.Fatal("conflict created claims ledger", err)
			}
		})
	}
}

func TestInactiveHooksDoNotConsumeWorkerAdmission(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(fmt.Sprint(review), func(t *testing.T) {
			root, provider, req := fixture(t)
			hooks := []any{}
			for n := 0; n < 65; n++ {
				hooks = append(hooks, map[string]any{"instructions": fmt.Sprintf("Hook %d", n)})
			}
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": hooks}}}})
			if review {
				req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}}
				req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if review {
				if err == nil || !strings.Contains(err.Error(), "engine limit") {
					t.Fatal("reachable work bypassed limit", err)
				}
				return
			}
			if err != nil {
				t.Fatal("inactive hooks blocked empty plan", err)
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.Dir(prepared.Workflow), "pass.yml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "review_admission") || strings.Contains(string(data), "stage_") {
				t.Fatal("inactive hooks were generated")
			}
		})
	}
}

func TestChecklistEnvelopeAdmissionCoversSameSessionBoundary(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	var original []byte
	_, _, _ = executeFixture(t, root, provider, prepared, &fakeSession{assignmentHook: func(b []byte) { original = append([]byte{}, b...) }})
	plan, _ := ReadPlan(prepared.Plan)
	o, _ := plan.Occurrence("completion")
	overhead := len(checklistPrompt(original, o, o.MaxChecks)) - len(original)
	target := (1 << 20) - overhead - 256
	req.Instructions += strings.Repeat("x", target-len(original))
	prepared, err = Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	lengths := []int{}
	session := &fakeSession{omitChecklist: true, assignmentHook: func(b []byte) { lengths = append(lengths, len(b)) }}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if len(lengths) != 2 || session.calls["completion"] != 2 {
		t.Fatal("near-limit same-session continuation was not exercised", lengths, run.Status, run.FailureClass, run.Error)
	}
	for _, n := range lengths {
		if n > (1<<20) || n < (1<<20)-4096 {
			t.Fatal("wrong prompt boundary", n)
		}
	}
	// Still below the worker's initial bound, but too large for its mandatory
	// checklist envelope. Reject before claiming any native work.
	req.Instructions += strings.Repeat("x", overhead+200)
	prepared, err = Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = ReadPlan(prepared.Plan)
	_, err = runtime.claim(context.Background(), plan, workflows.ActionContext{RunID: run.ID, OperationKey: "near-limit"}, ActionInput{Plan: prepared.Plan, Occurrence: "completion", Cycle: "boundary", Attempt: 1})
	if err == nil || !strings.Contains(err.Error(), "checklist envelope") {
		t.Fatal("oversized continuation admitted", err)
	}
}

func TestUnacceptedOptionalFixCannotDisposeOriginalFinding(t *testing.T) {
	for _, state := range []string{"failed", "incomplete", "accepted"} {
		t.Run(state, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-address", "required": false}}
			req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			session := &fakeSession{failCheckpoint: "completion", reportHook: func(o Occurrence, s Snapshot, r *Report) {
				if o.Review {
					r.Findings = []Finding{{ID: "F1", Text: "Retained original candidate", Sources: []string{s.Evidence}}}
				}
				if o.Kind == "fix" && state == "incomplete" {
					r.Complete = false
					r.Checklist = nil
				}
			}, reportCandidates: func(o Occurrence, _ Snapshot, candidates []Candidate, r *Report) {
				for _, c := range candidates {
					disposition := "confirmed"
					if o.Kind == "fix" {
						disposition = "fixed"
					}
					r.Findings = append(r.Findings, Finding{ID: c.Finding.ID, Text: c.Finding.Text, Sources: []string{c.Original}, Disposition: disposition, Reason: "Assessed the original candidate."})
				}
			}}
			if state == "failed" {
				session.failAfterReport = func(o Occurrence) error {
					if o.Kind == "fix" {
						return errors.New("ordinary native failure with retained JSON")
					}
					return nil
				}
			}
			_, _, runtime := executeFixture(t, root, provider, prepared, session, true)
			p, _ := ReadPlan(prepared.Plan)
			live, err := runtime.live(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			complete, blockers, err := runtime.coverage(context.Background(), p, live)
			if err != nil {
				t.Fatal(err)
			}
			if state == "accepted" {
				if !complete {
					t.Fatal("accepted fix lost disposition", blockers)
				}
				return
			}
			if complete || !strings.Contains(strings.Join(blockers, " "), "sourced disposition required") {
				t.Fatal("unaccepted output cleared finding", complete, blockers)
			}
			reports, err := runtime.reports(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			retained := false
			for _, rr := range reports {
				if rr.Occurrence.Kind == "fix" && rr.Claim.State == "optional" && len(rr.Report.Findings) > 0 {
					retained = true
				}
			}
			if !retained {
				t.Fatal("fixture did not retain optional native JSON")
			}
		})
	}
}

type slowCIProvider struct {
	*fakeProvider
	observed chan time.Duration
}

func (p *slowCIProvider) ObserveCI(ctx context.Context, _ Identity, _ string) (CIObservation, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return CIObservation{}, errors.New("CI read has no deadline")
	}
	remaining := time.Until(deadline)
	p.observed <- remaining
	if remaining > 1500*time.Millisecond {
		return CIObservation{}, errors.New("CI read received enclosing deadline")
	}
	<-ctx.Done()
	return CIObservation{}, ctx.Err()
}
func TestCITransportUsesRetainedObservationDeadline(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-ci", "options": map[string]any{"timeout_seconds": 1, "max_pending_extensions": 0}}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	slow := &slowCIProvider{fakeProvider: provider, observed: make(chan time.Duration, 1)}
	runtime := &Runtime{Root: root, Provider: slow}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = runtime.ci(ctx, p, ActionInput{Occurrence: p.Groups["tasks"][0][0]})
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatal("CI read escaped retained window", err, ctx.Err())
	}
	select {
	case remaining := <-slow.observed:
		if remaining > time.Second {
			t.Fatal("wrong observation deadline", remaining)
		}
	default:
		t.Fatal("CI transport was not exercised")
	}
}

func TestConditionalPreferenceDecisionsRetainMatchedPaths(t *testing.T) {
	root, provider, req := fixture(t)
	ctx := context.Background()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "a.go", "b.go"}, {"commit", "-m", "Add scoped fixture files"}} {
		if _, err := git(ctx, root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA = head
	catalog := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "dk-workflows", "v2", "preferences.json")
	if err = os.MkdirAll(filepath.Dir(catalog), 0700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{"id": "scoped", "owner": map[string]any{"type": "user"}, "prompt": "Preserve the scoped requirement.", "scope": map[string]any{"include": []any{map[string]any{"type": "file", "path": "a.go"}}, "condition": "When this file participates in the requested change."}}
	if err = os.WriteFile(catalog, jsonBytes(map[string]any{"version": 1, "revision": 1, "records": []any{record}}), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(ctx, root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	runtime := &Runtime{Root: root, Provider: provider}
	for _, tc := range []struct {
		name       string
		applicable bool
		paths      []string
		valid      bool
	}{
		{"matched", true, []string{"a.go"}, true}, {"inapplicable", false, []string{}, true}, {"outside match", true, []string{"b.go"}, false}, {"omitted match", true, []string{}, false}, {"inapplicable paths", false, []string{"a.go"}, false}, {"duplicate paths", true, []string{"a.go", "a.go"}, false}, {"expanded scope", true, []string{"a.go", "b.go"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := Report{PreferenceDecisions: []PreferenceDecision{{ID: "scoped", Applicable: tc.applicable, Paths: tc.paths, Reason: "Assessed the scoped condition.", Sources: []string{p.Snapshot.Evidence}}}}
			err := runtime.validatePreferenceDecisions(ctx, p, Occurrence{Review: true}, report)
			if (err == nil) != tc.valid {
				t.Fatal("wrong preference scope decision", err)
			}
		})
	}
	for _, occurrence := range []Occurrence{{Kind: "hook"}, {Kind: "cleanup"}, {Kind: "inspection", Adapter: Adapter{Writes: []string{"a.go"}}}} {
		if err := runtime.validatePreferenceDecisions(ctx, p, occurrence, Report{}); err == nil {
			t.Fatal("writable occurrence omitted conditional preference", occurrence.Kind)
		}
		report := Report{PreferenceDecisions: []PreferenceDecision{{ID: "scoped", Applicable: true, Paths: []string{"a.go"}, Reason: "Assessed the scoped condition.", Sources: []string{p.Snapshot.Evidence}}}}
		if err := runtime.validatePreferenceDecisions(ctx, p, occurrence, report); err != nil {
			t.Fatal("valid writable preference rejected", occurrence.Kind, err)
		}
	}
}

type extendingCIProvider struct {
	*fakeProvider
	now    *time.Time
	calls  int
	resume bool
}

func (p *extendingCIProvider) ObserveCI(ctx context.Context, _ Identity, head string) (CIObservation, error) {
	p.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 {
		return CIObservation{}, errors.New("extension did not admit a bounded live read")
	}
	obs := CIObservation{Head: head, Checks: []CICheck{{ID: "1", Status: "completed", Conclusion: "success"}}}
	if p.calls == 1 {
		if !p.resume {
			*p.now = p.now.Add(2 * time.Second)
		}
		obs.Checks[0].Status = "in_progress"
		obs.Checks[0].Conclusion = ""
	}
	return obs, nil
}
func TestCITransportExtensionSurvivesInterruptedObservation(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-ci", "options": map[string]any{"timeout_seconds": 1, "pending_extension_seconds": 5, "max_pending_extensions": 1}}}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := ReadPlan(prepared.Plan)
			now := time.Now()
			started := now
			ledger, err := OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			if resume {
				started = now.Add(-2 * time.Second)
				if _, err = ledger.Deadline(context.Background(), p.LogicalRun, p.Snapshot.Head, started, time.Second); err != nil {
					t.Fatal(err)
				}
				path, err := artifact(root, CIObservation{Head: p.Snapshot.Head, Checks: []CICheck{{ID: "1", Status: "in_progress"}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = ledger.db.Exec("INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, p.Snapshot.Evidence, "ci:"+p.Snapshot.Head, path); err != nil {
					t.Fatal(err)
				}
			}
			observer := &extendingCIProvider{fakeProvider: provider, now: &now, resume: resume}
			runtime := &Runtime{Root: root, Provider: observer, Now: func() time.Time { return now }, PollInterval: time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := runtime.ci(ctx, p, ActionInput{Occurrence: p.Groups["tasks"][0][0]})
			if err != nil || out.Report == "" {
				t.Fatal(out, err)
			}
			var extensions int
			var deadline string
			if err = ledger.db.QueryRow("SELECT extensions,deadline FROM deadlines WHERE run=? AND head=?", p.LogicalRun, p.Snapshot.Head).Scan(&extensions, &deadline); err != nil {
				t.Fatal(err)
			}
			parsed, err := time.Parse(time.RFC3339Nano, deadline)
			if err != nil || extensions != 1 || !parsed.Equal(started.Add(6*time.Second)) {
				t.Fatal("observation reset deadline or allowance", extensions, deadline, err)
			}
			want := 2
			if observer.calls != want {
				t.Fatal("wrong observation count", observer.calls)
			}
		})
	}
}

func TestCIReportFixtureReturnsMissingAndMalformedEvidence(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	report, err := ciReportFor(root, p, p.Snapshot)
	if err == nil || report != nil {
		t.Fatal("missing observation was not returned as an error", report, err)
	}
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	path := filepath.Join(t.TempDir(), "ci.json")
	if _, err = ledger.db.Exec("INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, p.Snapshot.Evidence, "ci:"+p.Snapshot.Head, path); err != nil {
		t.Fatal(err)
	}
	report, err = ciReportFor(root, p, p.Snapshot)
	if !os.IsNotExist(err) || report != nil {
		t.Fatal("missing artifact not returned", report, err)
	}
	if err = os.WriteFile(path, []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = ciReportFor(root, p, p.Snapshot)
	if err == nil || report != nil {
		t.Fatal("malformed artifact not returned", report, err)
	}
}

func TestUnacceptedInspectionCannotWeakenRetainedReplyObligation(t *testing.T) {
	root, provider, req := fixture(t)
	provider.ev.Comments = []map[string]any{{"id": float64(9), "body": "Please reply when verified.", "user": map[string]any{"login": "author"}}}
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-feedback-read"}, map[string]any{"skill": "custom:optional-inspection", "required": false}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-reply"}}
	req.Adapters = map[string]Adapter{"custom:optional-inspection": {Identity: "custom:optional-inspection", Version: "v1", Kind: "inspection", Procedure: [][]Pass{{{ID: "inspect", Role: "leaf", Assignment: "Inspect current discussion.", Evidence: []string{"original"}}}}}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{failCheckpoint: "completion", reportHook: func(o Occurrence, s Snapshot, r *Report) {
		if o.Kind == "inspection" {
			r.ReplyObligations = []ReplyObligation{{ID: "request-9", Text: "Please reply when verified.", Required: o.Call.Invocation.Skill != "custom:optional-inspection", Reason: "Assessed the author request.", Sources: []string{s.Evidence}}}
		}
	}, reportReplies: func(_ Occurrence, _ Snapshot, items []retainedObligation, r *Report) {
		for _, item := range items {
			r.Responses = append(r.Responses, ReplyResponse{ID: item.Obligation.ID, Outcome: "no_reply", Reason: "Optional discussion does not need an answer.", Sources: []string{item.Original}})
		}
	}, failAfterReport: func(o Occurrence) error {
		if o.Call.Invocation.Skill == "custom:optional-inspection" {
			return errors.New("native inspection failed after partial output")
		}
		return nil
	}}
	_, _, runtime := executeFixture(t, root, provider, prepared, session, true)
	p, _ := ReadPlan(prepared.Plan)
	items, err := runtime.replyObligations(context.Background(), p)
	if err != nil || len(items) != 1 || !items[0].Obligation.Required {
		t.Fatal("failed output weakened retained request", items, err)
	}
	live, err := runtime.live(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	complete, blockers, err := runtime.coverage(context.Background(), p, live)
	if err != nil || complete || !strings.Contains(strings.Join(blockers, " "), "explicit no-reply authority required") {
		t.Fatal("required reply was bypassed", complete, blockers, err)
	}
}
