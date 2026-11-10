package prfix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every gh call goes to a local, deny-by-default fixture. No provider credentials
// or remote repository are used by these adapter tests.
func fakeGH(t *testing.T, root string, responses map[string]any) func(map[string]any) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "api.json")
	script := `#!/usr/bin/env python3
import json, os, sys
responses=json.load(open(os.environ['PRFIX_FAKE_API']))
args=sys.argv[1:]
if args[:2]==['run','view']:
 if 'failed-step-output' in responses:
  sys.stdout.write(responses['failed-step-output']);sys.exit(0)
 job_id=args[args.index('--job')+1]
 job_path=next(k for k in responses if k.endswith('/actions/jobs/'+job_id))
 job=responses[job_path]
 if args[2]!=str(job['run_id']) or args[args.index('--attempt')+1]!=str(job['run_attempt']):
  print('Unpinned fixture run/attempt',file=sys.stderr);sys.exit(2)
 failed=[s for s in job.get('steps',[]) if s['conclusion'] not in ('success','skipped','neutral')]
 if not failed:
  print('Fixture requires explicit failed-step metadata',file=sys.stderr);sys.exit(2)
 logs=responses[job_path+'/logs']
 sys.stdout.write(''.join(job['name']+'\t'+failed[0]['name']+'\t'+line+'\n' for line in logs.splitlines()))
 sys.exit(0)
path=next((x for x in args if x.startswith('repos/') or x=='graphql' or x=='user'),None)
if os.environ.get('PRFIX_FAKE_REQUESTS'):
 with open(os.environ['PRFIX_FAKE_REQUESTS'],'a') as calls:
  method=args[args.index('--method')+1] if '--method' in args else 'GET'
  calls.write(method+' '+str(path)+'\n')
if path not in responses:
 print('Unconfigured fake GitHub request: '+str(path),file=sys.stderr);sys.exit(2)
value=responses[path]
if isinstance(value,dict) and '__sequence' in value:
 sequence=value['__sequence']
 value=sequence[0]
 if len(sequence)>1:
  sequence.pop(0)
  with open(os.environ['PRFIX_FAKE_API'],'w') as data: json.dump(responses,data)
if isinstance(value,dict) and '__exit' in value:
 sys.stdout.write(value.get('__body',''));sys.stderr.write(value.get('__stderr',''));sys.exit(value['__exit'])
if isinstance(value,str): sys.stdout.write(value)
else: json.dump(value,sys.stdout)
if isinstance(value,str) and value.startswith("HTTP/") and value.split()[1]!="200": sys.exit(1)
`
	if e := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRFIX_FAKE_API", data)
	update := func(values map[string]any) {
		t.Helper()
		if e := os.WriteFile(data, jsonBytes(values), 0600); e != nil {
			t.Fatal(e)
		}
	}
	update(responses)
	return update
}
func TestGitHubExactAttemptFlakeAndExceptionVerification(t *testing.T) {
	root, _, req := fixture(t)
	id, e := ParseIdentity(req.PR)
	if e != nil {
		t.Fatal(e)
	}
	head := strings.Repeat("a", 40)
	response := map[string]any{
		"repos/owner/repo/actions/runs/11":      map[string]any{"head_sha": head, "run_attempt": 2, "path": ".github/workflows/test.yml@main"},
		"repos/owner/repo/actions/jobs/22":      map[string]any{"run_id": 11, "run_attempt": 2, "name": "unit", "conclusion": "failure", "steps": []ciStep{{Number: 1, Name: "Run tests", Status: "completed", Conclusion: "failure"}}, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/33"},
		"repos/owner/repo/actions/jobs/22/logs": "--- FAIL: TestIntermittent (0.0s)\nconnection reset by peer\nFAIL\n",
		"repos/owner/repo/issues/99":            map[string]any{"state": "open", "html_url": "https://github.com/owner/repo/issues/99", "title": "Confirmed flaky test.", "body": "`.github/workflows/test.yml` `unit` `TestIntermittent` `connection reset by peer`"},
	}
	update := fakeGH(t, root, response)
	g := &GitHub{Root: root}
	classification := CIClassification{CheckID: "33", RunID: "11", JobID: "22", Attempt: 2, Workflow: ".github/workflows/test.yml", Job: "unit", Kind: "flake", Test: "TestIntermittent", Signature: "connection reset by peer", Issue: "https://github.com/owner/repo/issues/99"}
	proof, e := g.VerifyCI(context.Background(), id, head, []CIClassification{classification}, nil)
	if e != nil || !proof.Exempt {
		t.Fatal(proof, e)
	}
	response["repos/owner/repo/issues/99"] = map[string]any{"state": "closed", "title": "TestIntermittent flaky"}
	update(response)
	if _, e = g.VerifyCI(context.Background(), id, head, []CIClassification{classification}, nil); e == nil {
		t.Fatal("closed flake issue exempted failure")
	}
	classification.Kind = "exception"
	classification.Rule = "known"
	classification.Issue = ""
	rules := map[string]map[string]any{"known": {"kind": "test", "workflow": classification.Workflow, "job": "unit", "test": classification.Test, "signature": classification.Signature}}
	proof, e = g.VerifyCI(context.Background(), id, head, []CIClassification{classification}, rules)
	if e != nil || !proof.Exempt {
		t.Fatal("configured exact exception", e)
	}
	response["repos/owner/repo/actions/jobs/22/logs"] = "--- FAIL: TestIntermittent (0.0s)\nconnection reset by peer\n--- FAIL: TestOther (0.0s)\nunknown failure\n"
	update(response)
	if _, e = g.VerifyCI(context.Background(), id, head, []CIClassification{classification}, rules); e == nil {
		t.Fatal("one exception hid another failure")
	}
	classification.Attempt = 1
	if _, e = g.VerifyCI(context.Background(), id, head, []CIClassification{classification}, rules); e == nil {
		t.Fatal("stale attempt accepted")
	}
}
func TestCIObservationRetainsExactFailureLogs(t *testing.T) {
	root, _, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head := strings.Repeat("b", 40)
	prefix := "repos/owner/repo/commits/" + head
	responses := map[string]any{
		prefix + "/check-runs?filter=latest&per_page=100": []any{map[string]any{"check_runs": []any{map[string]any{"id": 33, "name": "unit", "head_sha": head, "status": "completed", "conclusion": "failure", "details_url": "https://github.com/owner/repo/actions/runs/11/job/22"}}}},
		prefix + "/statuses?per_page=100":                 []any{[]any{}},
		"repos/owner/repo/actions/jobs/22":                map[string]any{"run_id": 11, "run_attempt": 2, "head_sha": head, "check_run_url": "https://api.github.com/repos/owner/repo/check-runs/33"},
		"repos/owner/repo/actions/jobs/22/logs":           "--- FAIL: TestRealFailure\nactual failure log\n",
	}
	fakeGH(t, root, responses)
	observation, e := (&GitHub{Root: root}).ObserveCI(context.Background(), id, head)
	if e != nil {
		t.Fatal(e)
	}
	if observation.Success() || len(observation.Checks) != 1 || observation.Checks[0].Attempt != 2 || observation.Logs["33"] == "" {
		t.Fatal(observation)
	}
	b, e := os.ReadFile(observation.Logs["33"])
	if e != nil || !strings.Contains(string(b), "actual failure log") {
		t.Fatal("native leaf cannot inspect original logs", e)
	}
	if (CIObservation{Head: head}).Success() {
		t.Fatal("empty CI counted as successful")
	}
}

func TestInlineReplyUsesPRScopedEndpointAndReconcilesExactThread(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	body := "The correction is verified."
	wrong := map[string]any{"id": float64(8), "body": body, "in_reply_to_id": float64(99), "user": map[string]any{"login": "author", "id": 7}}
	right := map[string]any{"id": float64(9), "body": body, "in_reply_to_id": float64(7), "user": map[string]any{"login": "author", "id": 7}}
	responses := map[string]any{
		"repos/owner/repo/pulls/1/comments/7/replies": right,
		"repos/owner/repo/pulls/1":                    provider.ev.Metadata,
		"user":                                        map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}},
		"repos/owner/repo/pulls/1/reviews?per_page=100":   []any{[]any{}},
		"repos/owner/repo/pulls/1/comments?per_page=100":  map[string]any{"__sequence": []any{[]any{[]any{wrong}}, []any{[]any{wrong, right}}}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	fakeGH(t, root, responses)
	receipt, e := (&GitHub{Root: root}).Execute(context.Background(), Intent{ID: "reply", Kind: "reply_inline", PR: id, ActorID: 7, Before: map[string]any{"comment_ids": []string{}}, Head: provider.ev.Metadata.Head.SHA, Payload: map[string]any{"body": body, "comment_id": "7"}})
	if e != nil {
		t.Fatal(e)
	}
	if githubID(receipt.Result, "in_reply_to_id") != "7" {
		t.Fatal("matched a reply in another thread", receipt)
	}
}

func TestMergeReconciliationRequiresExactActorMethodAndTargetParents(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	target := strings.Repeat("b", 40)
	head := strings.Repeat("a", 40)
	merged := strings.Repeat("c", 40)
	metadata := provider.ev.Metadata
	metadata.State = "closed"
	metadata.Merged = true
	metadata.Head.SHA = head
	metadata.Base.SHA = strings.Repeat("d", 40)
	metadata.MergeCommit = merged
	metadata.MergedBy.ID = 7
	responses := map[string]any{
		"repos/owner/repo/pulls/1": metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	for _, tc := range []struct {
		name, method string
		actor        int64
		parents      []string
		want         string
	}{
		{"lost squash response", "squash", 7, []string{target}, "completed"},
		{"lost merge response", "merge", 7, []string{target, head}, "completed"},
		{"another actor", "squash", 8, []string{target}, "unknown"},
		{"advanced target", "squash", 7, []string{metadata.Base.SHA}, "unknown"},
		{"wrong method", "merge", 7, []string{target}, "unknown"},
		{"wrong head parent", "merge", 7, []string{target, strings.Repeat("e", 40)}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata.MergedBy.ID = tc.actor
			responses["repos/owner/repo/pulls/1"] = metadata
			parents := []any{}
			for _, sha := range tc.parents {
				parents = append(parents, map[string]any{"sha": sha})
			}
			responses["repos/owner/repo/git/commits/"+merged] = map[string]any{"sha": merged, "parents": parents}
			update(responses)
			state, _, err := (&GitHub{Root: root}).Reconcile(context.Background(), Intent{ID: "merge", Kind: "merge", PR: id, Head: head, Target: target, ActorID: 7, Payload: map[string]any{"merge_method": tc.method}})
			if err != nil || state != tc.want {
				t.Fatal("incorrect merge reconciliation", state, err)
			}
		})
	}
}

func TestSplitPRReconciliationRequiresExactCreationIdentity(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head, split, target := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	body := "Prepared independent change."
	intent := Intent{ID: "split", Kind: "split_pr", PR: id, Head: head, CreatedHead: split, Target: target, ActorID: 7, Before: map[string]any{"pr_numbers": []string{"1"}}, HeadRepository: "github.com/owner/repo", Payload: map[string]any{"title": "Independent change", "body": body, "head": "owner:split/one", "base": "main", "draft": true}}
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	for _, name := range []string{"exact", "actor", "head", "head repository", "base", "base repository", "draft", "body", "title", "closed", "duplicate", "baseline", "absent"} {
		t.Run(name, func(t *testing.T) {
			pr := map[string]any{"number": 2, "state": "open", "draft": true, "title": "Independent change", "body": body, "user": map[string]any{"login": "author", "id": 7}, "head": map[string]any{"sha": split, "ref": "split/one", "repo": map[string]any{"full_name": "owner/repo"}}, "base": map[string]any{"sha": target, "ref": "main", "repo": map[string]any{"full_name": "owner/repo"}}}
			switch name {
			case "actor":
				pr["user"].(map[string]any)["id"] = 8
			case "head":
				pr["head"].(map[string]any)["sha"] = head
			case "head repository":
				pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "other/repo"}
			case "base":
				pr["base"].(map[string]any)["sha"] = head
			case "base repository":
				pr["base"].(map[string]any)["repo"] = map[string]any{"full_name": "owner/other"}
			case "draft":
				pr["draft"] = false
			case "body":
				pr["body"] = "Edited payload."
			case "title":
				pr["title"] = "Edited title"
			case "closed":
				pr["state"] = "closed"
			}
			prs := []any{pr}
			if name == "duplicate" {
				prs = append(prs, pr)
			}
			if name == "absent" {
				prs = nil
			}
			responses["repos/owner/repo/pulls?state=all&per_page=100"] = []any{prs}
			update(responses)
			candidate := intent
			if name == "baseline" {
				candidate.Before = map[string]any{"pr_numbers": []string{"1", "2"}}
			}
			state, _, err := (&GitHub{Root: root}).Reconcile(context.Background(), candidate)
			want := "unknown"
			if name == "exact" {
				want = "completed"
			}
			if err != nil || state != want {
				t.Fatal(state, err)
			}
		})
	}
}

func TestCIRerunReconciliationRequiresExactlyNextAttempt(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head := provider.ev.Metadata.Head.SHA
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/pulls/1/comments?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	for _, attempt := range []int{2, 3, 4} {
		responses["repos/owner/repo/actions/runs/11"] = map[string]any{"head_sha": head, "run_attempt": attempt}
		update(responses)
		state, _, err := (&GitHub{Root: root}).Reconcile(context.Background(), Intent{ID: "rerun", Kind: "ci_rerun", PR: id, Head: head, Payload: map[string]any{"run_id": "11", "attempt": float64(2)}})
		want := "unknown"
		if attempt == 3 {
			want = "completed"
		}
		if err != nil || state != want {
			t.Fatal(attempt, state, err)
		}
	}
}
