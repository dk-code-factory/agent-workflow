package prfix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestAcknowledgedReplyRecoveryKeepsReturnedCommentIdentity(t *testing.T) {
	for _, kind := range []string{"reply", "reply_inline"} {
		t.Run(kind, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Authorizations = map[string]Authorization{"iterate": {"reply", "Reply to the feedback.", "chat:reply"}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prepared, err := Prepare(ctx, root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, err := ReadPlan(prepared.Plan)
			if err != nil {
				t.Fatal(err)
			}
			l, err := OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			i := Intent{ID: "acknowledged-reply", Kind: kind, ActorID: 7, Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Before: map[string]any{"comment_ids": []string{}}, Payload: map[string]any{"body": "The acknowledged answer."}, Sources: []string{p.Snapshot.Evidence}}
			writePath := "repos/owner/repo/issues/1/comments"
			listPath := writePath + "?per_page=100"
			if kind == "reply_inline" {
				i.Payload["comment_id"] = "9"
				writePath = "repos/owner/repo/pulls/1/comments/9/replies"
				listPath = "repos/owner/repo/pulls/1/comments?per_page=100"
			}
			if err = l.PrepareIntent(ctx, p, i, p.Snapshot.Evidence); err != nil {
				t.Fatal(err)
			}
			i, err = l.Intent(ctx, p.LogicalRun, i.ID)
			if err != nil {
				t.Fatal(err)
			}
			comment := func(id, actor int, body string) map[string]any {
				return map[string]any{"id": id, "body": body, "in_reply_to_id": 9, "user": map[string]any{"id": actor, "login": "author"}}
			}
			original := comment(101, 7, "The acknowledged answer.")
			duplicate := comment(102, 7, "The acknowledged answer.")
			responses := recoveryAdapterResponses(provider)
			responses[writePath] = original
			responses["repos/owner/repo/pulls/1"] = map[string]any{"__sequence": []any{provider.ev.Metadata, provider.ev.Metadata, "{failed observation"}}
			update := fakeGH(t, root, responses)
			calls := filepath.Join(t.TempDir(), "requests")
			t.Setenv("PRFIX_FAKE_REQUESTS", calls)
			g := &GitHub{Root: root}
			if _, err = l.db.Exec("UPDATE effects SET state='dispatched' WHERE run=? AND id=?", i.Run, i.ID); err != nil {
				t.Fatal(err)
			}
			if receipt, err := g.Execute(ctx, i); err == nil || receipt.ID != "" || !strings.Contains(err.Error(), "response retained") {
				t.Fatal("did not exercise acknowledged POST followed by observation failure", receipt, err)
			}
			var acknowledged DispatchResponse
			if err = readJSON(g.dispatchResponsePath(i), &acknowledged); err != nil || githubID(acknowledged.Response, "id") != "101" {
				t.Fatal("original response was not retained", acknowledged, err)
			}
			responses["repos/owner/repo/pulls/1"] = provider.ev.Metadata
			r := &Runtime{Root: root, Provider: g}
			for _, tc := range []struct {
				name     string
				comments []any
			}{
				{"missing original", []any{duplicate}},
				{"edited original", []any{comment(101, 7, "Edited answer."), duplicate}},
				{"wrong original actor", []any{comment(101, 8, "The acknowledged answer."), duplicate}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					responses[listPath] = []any{tc.comments}
					update(responses)
					if out, err := r.publish(ctx, p, ActionInput{Intent: i.ID}); err == nil || out.Settled {
						t.Fatal("substitute settled the acknowledged reply", out, err)
					}
					var state, receipt string
					if err := l.db.QueryRow("SELECT state,receipt FROM effects WHERE run=? AND id=?", i.Run, i.ID).Scan(&state, &receipt); err != nil || state != "dispatched" || receipt != "" {
						t.Fatal("uncertainty or original identity lost", state, receipt, err)
					}
				})
			}
			responses[listPath] = []any{[]any{original, duplicate}}
			update(responses)
			out, err := r.publish(ctx, p, ActionInput{Intent: i.ID})
			if err != nil || !out.Settled || out.Receipt == nil || githubID(out.Receipt.Result, "id") != "101" {
				t.Fatal("exact acknowledged comment did not settle with an identical sibling", out, err)
			}
			if err := readJSON(g.dispatchResponsePath(i), &acknowledged); err != nil || githubID(acknowledged.Response, "id") != "101" {
				t.Fatal("recovery replaced acknowledged response", acknowledged, err)
			}
			requests, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(requests), "POST "+writePath+"\n") != 1 {
				t.Fatal("recovery resent the reply", string(requests), err)
			}
		})
	}
}

func TestDuplicateReviewerFindingIDsRejectBeforeProposals(t *testing.T) {
	malformed := Report{Complete: true, Findings: []Finding{{ID: "F1", Text: "Issue A", Sources: []string{"evidence"}}, {ID: "F1", Text: "Issue B", Sources: []string{"evidence"}}}}
	if _, err := parseReport(workflows.WorkerMessage{Result: workflows.SessionResult{Outputs: map[string]any{"text": string(jsonBytes(malformed))}}}); err == nil || !strings.Contains(err.Error(), "duplicate finding ID") {
		t.Fatal("distinct findings sharing an ID were not rejected at report admission", err)
	}
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"instructions": "Work after accepted review."}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Review {
			report.Findings = []Finding{{ID: "F1", Text: "Issue A", Sources: []string{s.Evidence}}, {ID: "F1", Text: "Issue B", Sources: []string{s.Evidence}}}
			report.Operations = []Intent{{ID: "unaccepted-proposal", Kind: "reply", Head: s.Head, Payload: map[string]any{"body": "Must not be admitted."}, Sources: []string{s.Evidence}}}
		}
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session, true)
	if run.Status == workflows.RunStatusSucceeded || provider.writes != 0 {
		t.Fatal("duplicate findings were accepted", run.Status, provider.writes)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var effects, accepted, submissions int
	for query, count := range map[string]*int{"SELECT COUNT(*) FROM effects": &effects, "SELECT COUNT(*) FROM claims WHERE state='complete'": &accepted, "SELECT COUNT(*) FROM submissions": &submissions} {
		if err := l.db.QueryRow(query).Scan(count); err != nil {
			t.Fatal(err)
		}
	}
	if effects != 0 || accepted != 0 {
		t.Fatal("malformed report created accepted evidence or proposals", effects, accepted, submissions)
	}
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, err := runtime.candidates(context.Background(), p); err != nil || len(candidates) != 0 {
		t.Fatal("malformed findings became verified candidates", candidates, err)
	}
}
