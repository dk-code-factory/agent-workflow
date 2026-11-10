package prfix

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetadataAdapterUsesPreparedRepositoryIdentity(t *testing.T) {
	for _, kind := range []string{"edit_pr", "request_review"} {
		for _, mode := range []string{"write", "satisfied", "wrong repository"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				root, provider, req := fixture(t)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				prepared, err := Prepare(ctx, root, provider, req)
				if err != nil {
					t.Fatal(err)
				}
				plan, err := ReadPlan(prepared.Plan)
				if err != nil {
					t.Fatal(err)
				}
				s := plan.Snapshot
				i := Intent{ID: "metadata", Kind: kind, PR: plan.Identity, ActorID: 7, Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Before: map[string]any{"body": provider.ev.Metadata.Body}, Payload: map[string]any{"body": "The reviewed description."}}
				if s.HeadRepository != "github.com/owner/repo" {
					t.Fatal("fixture did not retain the application repository identity", s.HeadRepository)
				}
				before, after := provider.ev.Metadata, provider.ev.Metadata
				after.Body = stringValue(i.Payload, "body")
				responses := recoveryAdapterResponses(provider)
				mutation := "PATCH repos/owner/repo/pulls/1\n"
				if kind == "edit_pr" {
					responses[prPath(i.PR)] = map[string]any{"__sequence": []any{before, before, after, after, after}}
					if mode == "satisfied" {
						responses[prPath(i.PR)] = after
					}
				} else {
					i.Payload = map[string]any{"reviewers": []string{"reviewer"}, "team_reviewers": []string{}}
					requested := map[string]any{"users": []any{map[string]any{"login": "reviewer"}}, "teams": []any{}}
					responses[prPath(i.PR)+"/requested_reviewers"] = map[string]any{"__sequence": []any{map[string]any{"users": []any{}, "teams": []any{}}, map[string]any{"accepted": true}, requested}}
					if mode == "satisfied" {
						responses[prPath(i.PR)+"/requested_reviewers"] = requested
					}
					mutation = "POST repos/owner/repo/pulls/1/requested_reviewers\n"
				}
				if mode == "wrong repository" {
					i.HeadRepository = "github.com/another/repo"
				}
				fakeGH(t, root, responses)
				calls := filepath.Join(t.TempDir(), "requests")
				t.Setenv("PRFIX_FAKE_REQUESTS", calls)
				receipt, err := (&GitHub{Root: root}).Execute(ctx, i)
				if mode == "wrong repository" {
					if err == nil || !strings.Contains(err.Error(), "changed before dispatch") {
						t.Fatal("different repository reached metadata dispatch", receipt, err)
					}
				} else if err != nil || receipt.ID != i.ID || len(receipt.Evidence) == 0 {
					t.Fatal("prepared metadata identity did not reach verified completion", receipt, err)
				}
				log, readErr := os.ReadFile(calls)
				writes := strings.Count(string(log), mutation)
				want := 0
				if mode == "write" {
					want = 1
				}
				if readErr != nil || writes != want {
					t.Fatal("metadata no-op or write count differs", string(log), want, readErr)
				}
			})
		}
	}
}

func TestInlineSourceUsesCapturedRootAndPaginatedRESTFeedback(t *testing.T) {
	root, provider, req := fixture(t)
	id, err := ParseIdentity(req.PR)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rootComment := map[string]any{"id": 9, "html_url": req.PR + "#discussion_r9", "body": "Root question"}
	feedback := map[string]any{"id": 10, "html_url": req.PR + "#discussion_r10", "body": "Later question", "in_reply_to_id": 9}
	answer := map[string]any{"id": 101, "html_url": req.PR + "#discussion_r101", "body": "The verified answer.", "in_reply_to_id": 9, "user": map[string]any{"id": 7, "login": "author"}}
	thread := map[string]any{"id": "original-thread", "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 9}}}}
	responses := recoveryAdapterResponses(provider)
	responses["graphql"] = map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 1, "nodes": []any{thread}, "pageInfo": map[string]any{"hasNextPage": false}}}}}}
	responses[prPath(id)+"/comments?per_page=100"] = []any{[]any{rootComment}, []any{feedback}}
	responses[prPath(id)+"/comments/9/replies"] = answer
	responses["repos/owner/repo/pulls/comments/101"] = "HTTP/2 200 OK\n\n" + string(jsonBytes(answer))
	update := fakeGH(t, root, responses)
	g := &GitHub{Root: root}
	i := Intent{ID: "later-feedback", Kind: "reply_inline", PR: id, ActorID: 7, Head: provider.ev.Metadata.Head.SHA, Source: &FeedbackSource{Kind: "inline_comment", ID: "10", URL: req.PR + "#discussion_r10", ThreadID: "original-thread"}, Before: map[string]any{"comment_ids": []string{"9", "10"}}, Payload: map[string]any{"comment_id": "9", "body": answer["body"]}}
	ev, err := g.Capture(ctx, id)
	if err != nil || len(ev.Inline) != 2 || len(ev.Threads) != 1 {
		t.Fatal("complete REST feedback was not captured", ev, err)
	}
	if err := verifyFeedbackSource(i, ev); err != nil {
		t.Fatal("production root-only thread evidence rejected later feedback", err)
	}
	responses[prPath(id)+"/comments?per_page=100"] = map[string]any{"__sequence": []any{[]any{[]any{rootComment}, []any{feedback}}, []any{[]any{rootComment}, []any{feedback, answer}}}}
	update(responses)
	receipt, err := g.Execute(ctx, i)
	if err != nil || githubID(receipt.Result, "id") != "101" {
		t.Fatal("later feedback did not dispatch through its original root", receipt, err)
	}
	if observation, err := g.InspectReply(ctx, i, receipt); err != nil || observation.Status != "visible" {
		t.Fatal("historical later-feedback reply could not be inspected", observation, err)
	}
	for _, change := range []string{"missing feedback", "moved feedback", "wrong thread"} {
		t.Run(change, func(t *testing.T) {
			responses[prPath(id)+"/comments?per_page=100"] = []any{[]any{rootComment}, []any{feedback, answer}}
			thread["id"] = "original-thread"
			switch change {
			case "missing feedback":
				responses[prPath(id)+"/comments?per_page=100"] = []any{[]any{rootComment}, []any{answer}}
			case "moved feedback":
				moved := jsonObject(feedback)
				moved["in_reply_to_id"] = 8
				responses[prPath(id)+"/comments?per_page=100"] = []any{[]any{rootComment}, []any{moved, answer}}
			case "wrong thread":
				thread["id"] = "different-thread"
			}
			update(responses)
			if _, err := g.InspectReply(ctx, i, receipt); err == nil {
				t.Fatal("changed original source accepted", change)
			}
		})
	}
}

func TestCompletedCustomPlanCannotResumeStaleSelectedCI(t *testing.T) {
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
	session := &fakeSession{failCheckpoint: "close", reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "ci" {
			var err error
			report.CI, err = ciReportFor(root, p, s)
			if err != nil {
				t.Error(err)
			}
		}
	}}
	ex, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	attempts, err := runtime.Store.ActionAttempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	for _, attempt := range attempts {
		if attempt.Contract.Name != "prfix_completion" || attempt.State != "completed" {
			continue
		}
		var out ActionOutput
		if err := json.Unmarshal(attempt.Result.Output, &out); err != nil {
			t.Fatal(err)
		}
		completed = out.Complete && !out.Ready
	}
	if !completed || !strings.Contains(run.Error, "lost application checkpoint") {
		t.Fatal("fixture did not stop after completed custom-plan live gate", completed, run.Error)
	}
	runtime.BeforeCheckpoint = nil
	for _, state := range []string{"failure", "in_progress"} {
		base.mu.Lock()
		base.ci.Checks[0].Status, base.ci.Checks[0].Conclusion = "completed", state
		if state == "in_progress" {
			base.ci.Checks[0].Status, base.ci.Checks[0].Conclusion = state, ""
		}
		base.mu.Unlock()
		_, err := resumeFixture(t, ex, run.ID, false)
		if err == nil || !strings.Contains(err.Error(), "current-execution CI") {
			t.Fatal("completed selected CI survived resume as cached success", state, err)
		}
	}
}
