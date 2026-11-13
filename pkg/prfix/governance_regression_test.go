package prfix

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestMutatingClaimsRecaptureTheirResourcesAndPreferences(t *testing.T) {
	for _, tc := range []struct{ change, kind string }{{"unchanged", "fix"}, {"own resource", "fix"}, {"implementation preference", "fix"}, {"unrelated resource", "fix"}, {"unchanged", "hook"}, {"implementation preference", "hook"}} {
		change := tc.change
		t.Run(tc.kind+"/"+change, func(t *testing.T) {
			root, provider, req := fixture(t)
			provider.ev.Comments = []map[string]any{{"id": json.Number("3123456789"), "body": "Earlier discussion."}}
			if err := os.WriteFile(filepath.Join(root, "code.txt"), []byte("initial PR change\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := git(context.Background(), root, "commit", "-am", "Prepare PR scope"); err != nil {
				t.Fatal(err)
			}
			head, err := git(context.Background(), root, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			provider.ev.Metadata.Head.SHA = head
			own := filepath.Join(t.TempDir(), "own.md")
			other := filepath.Join(t.TempDir(), "other.md")
			for _, path := range []string{own, other} {
				if err = os.WriteFile(path, []byte("Original resource"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			catalog := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "dk-workflows/v2/preferences.json")
			record := map[string]any{"id": "implementation", "owner": map[string]any{"type": "user"}, "prompt": "Original implementation instruction.", "applies_to": []string{"implementation"}, "scope": map[string]any{"include": []any{map[string]any{"type": "file", "path": "code.txt"}}}}
			save := func() error {
				if e := os.MkdirAll(filepath.Dir(catalog), 0700); e != nil {
					return e
				}
				return os.WriteFile(catalog, jsonBytes(map[string]any{"version": 1, "revision": 1, "records": []any{record}}), 0600)
			}
			if err = save(); err != nil {
				t.Fatal(err)
			}
			req.Options["tasks"] = []any{map[string]any{"skill": "custom:fix"}, map[string]any{"skill": "custom:other"}}
			req.Authorizations = map[string]Authorization{"iterate": {"fix", "Fix and publish this PR.", "chat:fix"}}
			req.Adapters = map[string]Adapter{
				"custom:fix":   {Identity: "custom:fix", Version: "v1", Kind: tc.kind, Writes: []string{"code.txt"}, Resources: []string{own}, Procedure: [][]Pass{{{ID: "fix", Role: "codex", Assignment: "Fix only the assigned scope.", Evidence: []string{"original"}}}}},
				"custom:other": {Identity: "custom:other", Version: "v1", Kind: "inspection", Resources: []string{other}, Procedure: [][]Pass{{{ID: "read", Role: "codex", Assignment: "Read this independent resource.", Evidence: []string{"original"}}}}},
			}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			session := &fakeSession{assignmentHook: func(raw []byte) {
				var assignment struct {
					Occurrence  Occurrence     `json:"occurrence"`
					Preferences map[string]any `json:"preferences"`
				}
				if err := json.Unmarshal(raw, &assignment); err != nil {
					t.Error(err)
					return
				}
				if assignment.Occurrence.Adapter.Identity == "custom:fix" {
					records, _ := assignment.Preferences["records"].([]any)
					if len(records) != 1 {
						t.Error("writable leaf lost applicable implementation preference", assignment.Preferences)
					}
				}
			}, failCheckpoint: "finish", reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Adapter.Identity != "custom:fix" {
					return
				}
				switch change {
				case "own resource":
					err = os.WriteFile(own, []byte("Changed resource"), 0600)
				case "unrelated resource":
					err = os.WriteFile(other, []byte("Changed resource"), 0600)
				case "implementation preference":
					record["prompt"] = "Renewed implementation instruction."
					err = save()
				}
				if err != nil {
					t.Error(err)
				}
				report.Operations = []Intent{{ID: "retained-proposal", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "Verified substantive answer."}, Sources: []string{s.Evidence}}}
			}}
			_, _, _ = executeFixture(t, root, provider, prepared, session, true)
			ledger, err := OpenLedger(root)
			if err != nil {
				t.Fatal(err)
			}
			defer ledger.Close()
			var state string
			var effects int
			if err = ledger.db.QueryRow("SELECT state FROM claims ORDER BY rowid LIMIT 1").Scan(&state); err != nil {
				t.Fatal(err)
			}
			if err = ledger.db.QueryRow("SELECT count(*) FROM effects").Scan(&effects); err != nil {
				t.Fatal(err)
			}
			stale := change == "own resource" || change == "implementation preference"
			if stale && (state != "stale" || effects != 0) || !stale && (state != "complete" || effects != 1) {
				t.Fatal("incorrect mutation acceptance", change, state, effects)
			}
		})
	}
}

func TestNestedReviewerGroupPromptsReachEveryLeaf(t *testing.T) {
	root, provider, req := fixture(t)
	outer := "  Review authorization boundaries.\nPreserve exact spacing  \n"
	middle := "Check ownership and cancellation.\n"
	child := "Verify the final resource checks.  "
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-reviewers-run", "instructions": outer, "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:group", "instructions": middle, "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review", "instructions": child}}}}}}}}
	group := reviewAdapter()
	group.Identity = "custom:group"
	group.Kind = "review_group"
	resource := filepath.Join(t.TempDir(), "group.md")
	if err := os.WriteFile(resource, []byte("Governing group guidance"), 0600); err != nil {
		t.Fatal(err)
	}
	group.Resources = []string{resource}
	req.Adapters = map[string]Adapter{"custom:group": group, "custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	session := &fakeSession{assignmentHook: func(raw []byte) {
		var assignment struct {
			Occurrence Occurrence `json:"occurrence"`
			Inherited  []string   `json:"inherited_invocation_instructions"`
			Additional string     `json:"additional_instructions"`
			Snapshot   Snapshot   `json:"snapshot"`
		}
		if err := json.Unmarshal(raw, &assignment); err != nil {
			t.Error(err)
			return
		}
		if assignment.Occurrence.Review {
			seen = true
			if !reflect.DeepEqual(assignment.Occurrence.InheritedResources, []string{resource}) || assignment.Snapshot.ResourceDigests[resource] == "" {
				t.Error("group governing resource not captured", assignment.Occurrence.InheritedResources)
			}
			if !reflect.DeepEqual(assignment.Inherited, []string{outer, middle}) || assignment.Additional != child {
				t.Error("selected specialization lost", assignment.Inherited, assignment.Additional)
			}
		}
	}}
	_, _, _ = executeFixture(t, root, provider, prepared, session)
	if !seen {
		t.Fatal("review leaf never executed")
	}
}

func TestReportCollectionLimitsPrecedeTypedAllocation(t *testing.T) {
	report := func(text string) (Report, error) {
		return parseReport(workflows.WorkerMessage{Result: workflows.SessionResult{Outputs: map[string]any{"text": text}}})
	}
	for _, name := range []string{"operations", "findings", "evidence"} {
		// Invalid elements distinguish the shape guard from typed decoding even
		// when the decoder would otherwise return early on its first type error.
		_, err := report(`{"` + name + `":[0]}`)
		var typeError *json.UnmarshalTypeError
		if !errors.As(err, &typeError) {
			t.Fatal("fixture does not distinguish typed decoding", name, err)
		}
		text := `{"complete":true,"` + name + `":[` + strings.Repeat(`0,`, 100000) + `0]}`
		if len(text) >= 16<<20 {
			t.Fatal("fixture no longer below serialized cap")
		}
		if _, err := report(text); err == nil || !strings.Contains(err.Error(), "collection limit") {
			t.Fatal("oversized collection typed before rejection", name, err)
		}
	}
	// Error precedence alone cannot prove allocation order: a decoder could
	// retain its error and return the shape error afterward. Measure a valid
	// collection built before the sample. Half its minimum typed backing array
	// is a generous budget for the shape guard, which stops at the limit.
	const count = 100001
	oversized := `{"operations":[` + strings.Repeat(`{},`, count-1) + `{}]}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, allocationErr := report(oversized)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	budget := uint64(reflect.TypeOf(Intent{}).Size()) * count / 2
	if allocationErr == nil || !strings.Contains(allocationErr.Error(), "collection limit") {
		t.Fatal("oversized valid collection accepted", allocationErr)
	}
	if allocated >= budget {
		t.Fatalf("oversized collection allocated %d bytes before rejection; budget %d", allocated, budget)
	}
	boundary := `{"operations":[` + strings.Repeat(`{},`, maxReportCollection-1) + `{}]}`
	got, err := report(boundary)
	if err != nil || len(got.Operations) != maxReportCollection {
		t.Fatal("bounded report rejected", len(got.Operations), err)
	}
	deep := `{"operations":[{"payload":{"nested":` + strings.Repeat(`[`, maxReportDepth+1) + `0` + strings.Repeat(`]`, maxReportDepth+1) + `}}]}`
	if _, err = report(deep); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatal("unbounded nesting accepted", err)
	}
	if _, err = report(`{"complete":true,"findings":[],"operations":[]}`); err != nil {
		t.Fatal("valid empty report rejected", err)
	}
}

func TestTestExemptionAccountsForEveryLogLine(t *testing.T) {
	classes := []CIClassification{{Test: "TestIntermittent", Signature: "connection reset by peer"}}
	covered := map[string]bool{"TestIntermittent": true}
	base := "--- FAIL: TestIntermittent (0.0s)\nconnection reset by peer\nFAIL\n"
	for _, extra := range []string{"src/main.cc:1:1: error: absent was not declared in this scope\nninja: build stopped: subcommand failed\n", "unrecognized compiler output\n", "unrecognized benign output\n"} {
		if err := verifyTestLogInventory(base+extra, classes, covered); err == nil {
			t.Fatal("additional job output exempted", extra)
		}
	}
	if err := verifyTestLogInventory("2026-11-09T12:00:00.123Z --- FAIL: TestIntermittent (0.0s)\n2026-11-09T12:00:00.124Z     flaky_test.go:7: connection reset by peer\n2026-11-09T12:00:00.125Z FAIL example/pkg 0.1s\n", classes, covered); err != nil {
		t.Fatal("supported exact test output rejected", err)
	}
}

func TestCreationPayloadsKeepPrivateIdentityOutOfPublicBodies(t *testing.T) {
	root, provider, req := fixture(t)
	req.Authorizations = map[string]Authorization{"iterate": {"private-run-id", "Fix and publish the PR.", "chat:publish"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	ledger, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	source := Intent{ID: "private-split-source", Kind: "split_push", Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Sources: []string{p.Snapshot.Evidence}, Payload: map[string]any{"branch": "split/fixture", "head": p.Snapshot.Head, "behavior": "split_related"}}
	if err = ledger.PrepareIntent(context.Background(), p, source, p.Snapshot.Evidence); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"reply", "reply_inline", "split_pr"} {
		body := "The verified correction preserves ownership."
		intent := Intent{ID: "private-" + kind, Kind: kind, Head: p.Snapshot.Head, Lease: p.Snapshot.Head, Target: p.Snapshot.Target, Sources: []string{p.Snapshot.Evidence}, Payload: map[string]any{"body": body}}
		if kind == "split_pr" {
			intent.Payload["push_operation"] = source.ID
		}
		if err = ledger.PrepareIntent(context.Background(), p, intent, p.Snapshot.Evidence); err != nil {
			t.Fatal(err)
		}
		saved, err := ledger.Intent(context.Background(), p.LogicalRun, intent.ID)
		if err != nil || stringValue(saved.Payload, "body") != body {
			t.Fatal("publication body modified with private state", saved.Payload, err)
		}
	}
}

func TestReplyRecoveryRequiresUniquePostBaselineCreation(t *testing.T) {
	root, provider, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	body := "The verified correction preserves ownership."
	comment := func(n int) map[string]any {
		return map[string]any{"id": n, "body": body, "user": map[string]any{"login": "author", "id": 7}, "in_reply_to_id": 7}
	}
	responses := map[string]any{
		"repos/owner/repo/pulls/1": provider.ev.Metadata, "user": map[string]any{"login": "author", "id": 7},
		"repos/owner/repo/pulls/1/reviews?per_page=100": []any{[]any{}}, "repos/owner/repo/issues/1/comments?per_page=100": []any{[]any{}},
		"graphql": map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{"totalCount": 0, "nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}}}}}},
	}
	update := fakeGH(t, root, responses)
	intent := Intent{ID: "private-recovery", Kind: "reply_inline", PR: id, ActorID: 7, Before: map[string]any{"comment_ids": []string{"8"}}, Payload: map[string]any{"body": body, "comment_id": "7"}}
	for _, tc := range []struct {
		name     string
		comments []any
		want     string
	}{
		{"older answer", []any{comment(8)}, "unknown"}, {"lost response", []any{comment(8), comment(9)}, "completed"}, {"ambiguous", []any{comment(8), comment(9), comment(10)}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses["repos/owner/repo/pulls/1/comments?per_page=100"] = []any{tc.comments}
			update(responses)
			state, receipt, err := (&GitHub{Root: root}).Reconcile(context.Background(), intent)
			if err != nil || state != tc.want {
				t.Fatal(state, receipt, err)
			}
			if state == "completed" && githubID(receipt.Result, "id") != "9" {
				t.Fatal("older comment substituted", receipt)
			}
		})
	}
}

func TestOptionalReviewerGroupRetainsChildFailureAsOmission(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-reviewers-run", "required": false, "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review", "required": true}}}}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Review {
			return errors.New("provider usage limit")
		}
		return nil
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Status != workflows.RunStatusSucceeded || session.calls["completion"] != 1 {
		t.Fatal("optional group became required", run.Status, session.calls)
	}
	path, _ := run.Outputs["private_report"].(string)
	var report struct {
		Optional []string `json:"optional_incomplete"`
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &report)
	}
	if err != nil || len(report.Optional) != 1 {
		t.Fatal("optional failure evidence lost", report, err)
	}
}

func TestReviewerGroupCannotSilentlyIgnoreCoordinatorControls(t *testing.T) {
	root, provider, req := fixture(t)
	group := reviewAdapter()
	group.Identity = "custom:group"
	group.Kind = "review_group"
	group.Controls = []string{"model"}
	req.Adapters = map[string]Adapter{"custom:group": group, "custom:review": reviewAdapter()}
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:group", "agent": map[string]any{"model": "chosen"}, "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review"}}}}}
	if _, err := Prepare(context.Background(), root, provider, req); err == nil || !strings.Contains(err.Error(), "no native coordinator") {
		t.Fatal("coordinator model silently ignored", err)
	}
	if provider.writes != 0 {
		t.Fatal(provider.writes)
	}
}

func TestFailedStepInventoryRequiresEveryExactStep(t *testing.T) {
	steps := []ciStep{{Number: 1, Name: "Setup", Status: "completed", Conclusion: "success"}, {Number: 2, Name: "Tests", Status: "completed", Conclusion: "failure"}, {Number: 3, Name: "Build", Status: "completed", Conclusion: "failure"}}
	for _, tc := range []struct {
		name, log string
		steps     []ciStep
		valid     bool
	}{
		{"all", "unit\tTests\t--- FAIL: TestIntermittent\nunit\tBuild\tsrc/main.cc:1:1: error: absent\n", steps, true},
		{"missing", "unit\tTests\t--- FAIL: TestIntermittent\n", steps, false},
		{"unknown", "unit\tUNKNOWN STEP\t--- FAIL: TestIntermittent\n", steps, false},
		{"wrong job", "other\tTests\t--- FAIL: TestIntermittent\n", steps, false},
		{"no metadata", "unit\tTests\t--- FAIL: TestIntermittent\n", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs, err := failedStepLogs(tc.log, "unit", tc.steps)
			if (err == nil) != tc.valid {
				t.Fatal(logs, err)
			}
			if tc.valid {
				for _, log := range logs {
					if strings.Contains(log, "main.cc") && verifyTestLogInventory(log, []CIClassification{{Test: "TestIntermittent"}}, map[string]bool{"TestIntermittent": true}) == nil {
						t.Fatal("independent failed build exempted")
					}
				}
			}
		})
	}
	classes := []CIClassification{{Test: "TestIntermittent", Signature: "connection reset"}}
	log := "##[group]Run make test\nmake test\nshell: /usr/bin/bash -e {0}\n##[endgroup]\ngo test -race ./...\nok example/other (cached)\n--- FAIL: TestIntermittent (0.0s)\n    flaky_test.go:4: connection reset\nFAIL example/pkg 0.1s\nFAIL\nmake: *** [Makefile:4: test] Error 1\nError: Process completed with exit code 2.\n"
	if err := verifyTestLogInventory(log, classes, map[string]bool{"TestIntermittent": true}); err != nil {
		t.Fatal("supported Actions/Go envelope rejected", err)
	}
}

func TestFailedOptionalReviewsCannotEstablishMergeCoverage(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review", "required": false}, map[string]any{"skill": "dk-workflows:dk-pr-description"}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	req.Authorizations = map[string]Authorization{"merge": {"merge", "Merge this PR only when reviewed ready.", "chat:merge"}}
	provider.mergeReady = true
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{nativeError: func(o Occurrence) error {
		if o.Review {
			return errors.New("provider usage limit")
		}
		return nil
	}, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "merge-no-coverage", Kind: "merge", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"sha": s.Head, "merge_method": "squash"}, Sources: []string{s.Evidence}}}
		}
	}}
	_, run, _ := executeFixture(t, root, provider, prepared, session, true)
	if provider.writes != 0 || !strings.Contains(run.Error, "observed current completed review coverage") {
		t.Fatal("failed optional reviews authorized merge", provider.writes, run.Error)
	}
}
