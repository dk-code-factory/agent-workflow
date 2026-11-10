package prfix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetadataDispatchRejectsNewlyObservedHead(t *testing.T) {
	for _, kind := range []string{"edit_pr", "request_review"} {
		t.Run(kind, func(t *testing.T) {
			root, provider, req := fixture(t)
			id, err := ParseIdentity(req.PR)
			if err != nil {
				t.Fatal(err)
			}
			i := Intent{ID: "metadata", Kind: kind, PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA, Lease: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"body": "The reviewed description."}, Before: map[string]any{"body": provider.ev.Metadata.Body}}
			if kind == "request_review" {
				i.Payload = map[string]any{"reviewers": []string{"reviewer"}, "team_reviewers": []string{}}
			}
			provider.ev.Metadata.Head.SHA = strings.Repeat("b", 40)
			responses := recoveryAdapterResponses(provider)
			fakeGH(t, root, responses)
			calls := filepath.Join(t.TempDir(), "requests")
			t.Setenv("PRFIX_FAKE_REQUESTS", calls)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := (&GitHub{Root: root}).Execute(ctx, i); err == nil || !strings.Contains(err.Error(), "changed before dispatch") {
				t.Fatal("metadata write ignored the newer head observation", err)
			}
			log, err := os.ReadFile(calls)
			if err != nil || strings.Contains(string(log), "PATCH ") || strings.Contains(string(log), "POST repos/") {
				t.Fatal("head drift reached a mutation", string(log), err)
			}
		})
	}
}

func TestRemovedReviewersCannotAuthorizeMerge(t *testing.T) {
	root, provider, req := fixture(t)
	provider.mergeReady = true
	req.Options["tasks"] = []any{"custom:review"}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	req.Authorizations = map[string]Authorization{"merge": {"merge", "Merge only with current selected review coverage.", "chat:merge"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	_, _, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
	initial, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := runtime.reports(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	for n := range initial.Occurrences {
		if initial.Occurrences[n].Review {
			initial.Occurrences[n].Adapter.Version += "-changed"
		}
	}
	if count, err := completedReviewCount(initial, historical, initial.Snapshot); err != nil || count != 0 {
		t.Fatal("changed reviewer definition still established current coverage", count, err)
	}
	req.Run = prepared.LogicalRun
	req.Options["tasks"] = []any{"dk-workflows:dk-pr-description"}
	req.Options["finalizers"] = []any{"dk-workflows:dk-pr-publish"}
	replacement, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadPlan(replacement.Plan)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := runtime.reports(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) == 0 {
		t.Fatal("fixture lost the historical reviewer")
	}
	count, err := completedReviewCount(p, reports, p.Snapshot)
	if err != nil || count != 0 {
		t.Fatal("removed reviewer still established selected coverage", count, err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "merge-after-removal", Kind: "merge", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"sha": s.Head, "merge_method": "squash"}, Sources: []string{s.Evidence}}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, replacement, session, true)
	if provider.writes != 0 || !strings.Contains(run.Error, "current completed review coverage") {
		t.Fatal("removed review coverage authorized a merge", provider.writes, run.Error)
	}
}

func TestReusedReviewHooksDoNotSkipNewFinalizer(t *testing.T) {
	for _, phase := range []string{"before_cycle", "after_cycle"} {
		t.Run(phase, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{"custom:review"}
			req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
			configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{phase: []any{map[string]any{"instructions": "Inspect the retained cycle boundary."}}}}}})
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _ = executeFixture(t, root, provider, prepared, &fakeSession{})
			req.Run = prepared.LogicalRun
			req.Options["finalizers"] = []any{map[string]any{"instructions": "Inspect the new independent finalizer."}}
			replacement, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ReadPlan(replacement.Plan)
			if err != nil {
				t.Fatal(err)
			}
			session := &fakeSession{}
			_, run, _ := executeFixture(t, root, provider, replacement, session)
			if run.Outputs["selected_complete"] != true || session.calls[p.Groups["finalizers"][0][0]] != 1 || session.calls[p.Groups[phase][0][0]] != 0 || session.calls[p.Groups["tasks"][0][0]] != 0 {
				t.Fatal("reused hooks skipped selected new work or reran reviews", run.Outputs, session.calls)
			}
			if phase == "after_cycle" {
				req.Options["finalizers"] = []any{map[string]any{"instructions": "Run the distinct required finalizer that fails."}}
				failed, err := Prepare(context.Background(), root, provider, req)
				if err != nil {
					t.Fatal(err)
				}
				failedPlan, err := ReadPlan(failed.Plan)
				if err != nil {
					t.Fatal(err)
				}
				failedOccurrence := failedPlan.Groups["finalizers"][0][0]
				failing := &fakeSession{nativeError: func(o Occurrence) error {
					if o.ID == failedOccurrence {
						return errors.New("required finalizer failed")
					}
					return nil
				}}
				_, _, _ = executeFixture(t, root, provider, failed, failing, true)
				if failing.calls[failedOccurrence] == 0 || failing.calls["completion"] != 0 {
					t.Fatal("skipped after hooks concealed a failed required predecessor", failing.calls)
				}
			}
		})
	}
}

type resumeCIProvider struct {
	*fakeProvider
	observations atomic.Int32
}

func (p *resumeCIProvider) ObserveCI(ctx context.Context, id Identity, head string) (CIObservation, error) {
	p.observations.Add(1)
	return p.fakeProvider.ObserveCI(ctx, id, head)
}

func TestResumedCompletionDoesNotReuseOldSameHeadCI(t *testing.T) {
	root, base, req := fixture(t)
	provider := &resumeCIProvider{fakeProvider: base}
	req.Options["tasks"] = []any{"custom:review"}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	req.Options["finalizers"] = []any{"dk-workflows:dk-ci"}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	var completionProduced, stopped atomic.Bool
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "ci" {
			var err error
			report.CI, err = ciReportFor(root, p, s)
			if err != nil {
				t.Error(err)
			}
		}
		if o.Kind == "completion" {
			completionProduced.Store(true)
		}
	}, checkpointHook: func(name string, _ ActionOutput) error {
		if name == "finish" && completionProduced.Load() && stopped.CompareAndSwap(false, true) {
			return errors.New("stop after managed completion and before live completion")
		}
		return nil
	}}
	ex, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if !stopped.Load() || provider.observations.Load() != 1 {
		t.Fatal("fixture did not retain exactly one completed CI observation", stopped.Load(), provider.observations.Load())
	}
	var live ActionOutput
	var observed bool
	runtime.BeforeCheckpoint = func(name string, out ActionOutput) error {
		if name == "completion" {
			live, observed = out, true
			return errors.New("stop after resumed live gate")
		}
		return nil
	}
	for _, status := range []string{"failure", "in_progress"} {
		base.mu.Lock()
		base.ci.Checks[0].Status, base.ci.Checks[0].Conclusion = "completed", status
		if status == "in_progress" {
			base.ci.Checks[0].Status, base.ci.Checks[0].Conclusion = status, ""
		}
		base.mu.Unlock()
		observed = false
		before := provider.observations.Load()
		if _, err := resumeFixture(t, ex, run.ID, false); err == nil || !observed || live.Complete || live.Ready || provider.observations.Load() != before+1 {
			t.Fatal("resume treated cached CI as current", status, observed, live, provider.observations.Load(), before, err)
		}
	}
	attempts, err := runtime.Store.ActionAttempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, attempt := range attempts {
		retained = retained || attempt.Contract.Name == "prfix_ci" && attempt.State == "completed"
	}
	if !retained {
		t.Fatal("test did not exercise a retained completed CI action")
	}
}

func TestInlineRecoveryVerifiesLaterFeedbackAndOriginalThread(t *testing.T) {
	root, base, req := fixture(t)
	id, err := ParseIdentity(req.PR)
	if err != nil {
		t.Fatal(err)
	}
	rootComment := map[string]any{"id": 9, "html_url": req.PR + "#discussion_r9", "body": "Original thread root"}
	feedback := map[string]any{"id": 10, "html_url": req.PR + "#discussion_r10", "body": "A later question", "in_reply_to_id": 9}
	base.ev.Inline = []map[string]any{rootComment, feedback}
	base.ev.Threads = []map[string]any{{"id": "original-thread", "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 9}}}}}
	i := Intent{ID: "historical", Kind: "reply_inline", Source: &FeedbackSource{"inline_comment", "10", req.PR + "#discussion_r10", "original-thread"}, PR: id, ActorID: 7, Head: base.ev.Metadata.Head.SHA, Payload: map[string]any{"comment_id": "9", "body": "The historical answer"}, Sources: []string{req.PR + "#discussion_r10"}}
	receipt := Receipt{ID: i.ID, Kind: i.Kind, PR: req.PR, Head: i.Head, Result: map[string]any{"id": 101}}
	runtime := &Runtime{Root: root, Provider: &replyRecoveryProvider{base}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := runtime.inspectReply(ctx, i, receipt); err != nil || out.Status != "unavailable" {
		t.Fatal("accessible later source was not distinguished from its routing parent", out, err)
	}
	for _, change := range []string{"missing source", "wrong parent", "wrong thread", "legacy provenance"} {
		t.Run(change, func(t *testing.T) {
			candidate := i
			base.ev.Inline = []map[string]any{rootComment, feedback}
			base.ev.Threads[0]["id"] = "original-thread"
			switch change {
			case "missing source":
				base.ev.Inline = []map[string]any{rootComment}
			case "wrong parent":
				moved := jsonObject(feedback)
				moved["in_reply_to_id"] = 8
				base.ev.Inline[1] = moved
			case "wrong thread":
				base.ev.Threads[0]["id"] = "another-thread"
			case "legacy provenance":
				candidate.Source = nil
			}
			if _, err := runtime.inspectReply(ctx, candidate, receipt); err == nil {
				t.Fatal("routing parent substituted for inaccessible original feedback", change)
			}
		})
	}
}
