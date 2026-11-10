package prfix

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGitHubIntegerIDsSurviveCreationAndStoredEvidence(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	const actor int64 = 40304587
	const parent = "3123456789"
	const created = "9007199254740993"
	body := "The exact correction is verified."
	old := map[string]any{"id": json.Number("4123456789"), "in_reply_to_id": json.Number(parent), "body": body, "user": map[string]any{"login": "author", "id": actor}}
	fresh := map[string]any{"id": json.Number(created), "in_reply_to_id": json.Number(parent), "body": body, "user": map[string]any{"login": "author", "id": actor}}
	responses := map[string]any{
		"repos/owner/repo/pulls/1/comments/" + parent + "/replies": fresh,
		"repos/owner/repo/pulls/1":                                 provider.ev.Metadata,
		"user":                                                     map[string]any{"login": "author", "id": actor},
		"repos/owner/repo/issues/1/comments?per_page=100":          []any{[]any{}},
		"repos/owner/repo/pulls/1/reviews?per_page=100":            []any{[]any{}},
		"repos/owner/repo/pulls/1/comments?per_page=100":           []any{[]any{old, fresh}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	fakeGH(t, root, responses)
	g := &GitHub{Root: root}
	intent := Intent{ID: "creation", Kind: "reply_inline", PR: id, ActorID: actor, Head: provider.ev.Metadata.Head.SHA, Before: map[string]any{"comment_ids": []string{"4123456789"}}, Payload: map[string]any{"body": body, "comment_id": parent}}
	receipt, err := g.Execute(context.Background(), intent)
	if err != nil || githubID(receipt.Result, "id") != created {
		t.Fatal("realistic ID creation was not reconciled exactly", receipt, err)
	}
	path, err := artifact(root, receipt)
	if err != nil {
		t.Fatal(err)
	}
	var saved Receipt
	if err := readJSON(path, &saved); err != nil || githubID(saved.Result, "id") != created {
		t.Fatal("receipt integer changed at rest", saved, err)
	}
	ev, err := g.Capture(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	path, err = artifact(root, map[string]any{"pr": ev})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := readPREvidence(path)
	if err != nil || len(restored.Inline) != 2 || githubID(restored.Inline[1], "id") != created {
		t.Fatal("evidence integer changed at rest", restored, err)
	}
}

func TestGitHubIDRejectsUncertainNumericRepresentations(t *testing.T) {
	for _, value := range []any{float64(1 << 53), 1.5, json.Number("1e6"), json.Number("9223372036854775808"), "-1", nil} {
		if got := githubID(map[string]any{"id": value}, "id"); got != "" {
			t.Fatal("uncertain identity admitted", value, got)
		}
	}
	if got := githubID(map[string]any{"id": float64(3123456789)}, "id"); got != "3123456789" {
		t.Fatal("legacy exact ID changed", got)
	}
}
