package prfix

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

func TestImmutablePublicationConcurrentAndInterruptedWriters(t *testing.T) {
	dir := os.Getenv("PRFIX_IMMUTABLE_WRITER_FIXTURE")
	if dir == "" {
		dir = t.TempDir()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Filesystem operations cannot be cancelled in-process. CommandContext
		// kills and Wait joins the child before TempDir cleanup on timeout.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImmutablePublicationConcurrentAndInterruptedWriters$")
		cmd.Env = append(os.Environ(), "PRFIX_IMMUTABLE_WRITER_FIXTURE="+dir)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("immutable writer subprocess failed: %v: %s", err, b)
		}
		return
	}
	path := filepath.Join(dir, "final.json")
	// Simulate a writer killed before publication. Its incomplete temp file must
	// neither expose a final artifact nor prevent another writer from finishing.
	if err := os.WriteFile(filepath.Join(dir, ".prfix-artifact-abandoned"), []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("complete bytes\n"), 100000)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- writeImmutable(path, data) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("partial final artifact", err)
	}
	if err = writeImmutable(path, []byte("different")); err == nil {
		t.Fatal("overwrote immutable bytes")
	}
	link := filepath.Join(dir, "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = writeImmutable(link, data); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestManagedRunnerWorkspaceRecoveryUsesProductionRestore(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "child")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// worker_links are written by the recovery action, not by the provider fake.
	_, err = l.db.Exec("INSERT INTO worker_links VALUES(?,?,?,?,?,?,?,?)", "prfix_recovered", "engine", "claim", 1, "original", "retained-session", "original-owner", "leaf")
	if err != nil {
		t.Fatal(err)
	}
	runner := WrapRunner(root, &clirunner.Runner{Workspace: root, Config: clirunner.Config{Agents: map[string]clirunner.Agent{"leaf": {Provider: "claude-cli"}}}})
	scoped, err := runner.WithWorkerWorkspace("leaf", workspace)
	if err != nil {
		t.Fatal(err)
	}
	access, err := scoped.(interface {
		WorkerAccess(string) (workflows.WorkerAccess, error)
	}).WorkerAccess("leaf")
	if err != nil || access.Workspace != workspace {
		t.Fatal("lost workspace scope", access, err)
	}
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: "engine", ParentWorkerID: "prfix_recovered", AgentID: "leaf"}}
	// The real CLI compatibility backend refuses session resume before spawning a
	// process. This proves scoping retained the real wrapper's restoration path.
	_, err = scoped.RunTurn(context.Background(), req, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot resume provider sessions") {
		t.Fatal("production scoped dispatch lost restored session", err)
	}
	_, err = l.db.Exec("UPDATE worker_links SET role='different'")
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoped.RunTurn(context.Background(), req, nil)
	if err == nil || !strings.Contains(err.Error(), "role binding changed") {
		t.Fatal("recovery role ownership bypassed", err)
	}
}

func TestCIPollIntervalResolvesConfigurationAndTestOverride(t *testing.T) {
	opts := map[string]any{"poll_interval_seconds": float64(7)}
	if ciPollInterval(opts, 0) != 7*time.Second || ciPollInterval(opts, time.Millisecond) != time.Millisecond || ciPollInterval(nil, 0) != 30*time.Second {
		t.Fatal("CI poll interval resolution")
	}
}

func reviewAdapter() Adapter {
	return Adapter{Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "read", Role: "leaf", Assignment: "Review only the pinned diff.", Evidence: []string{"original"}}}}}
}

func TestIndependentReviewerLocalIDsRetainEveryOriginalObligation(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review", "instructions": "First"}, map[string]any{"skill": "custom:review", "instructions": "Second"}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-address"}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{omitChecklist: true, reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Review {
			report.Findings = []Finding{{ID: "F1", Text: o.Call.Invocation.Instructions, Sources: []string{s.Evidence}}}
		}
	}, reportCandidates: func(o Occurrence, _ Snapshot, candidates []Candidate, report *Report) {
		for _, c := range candidates {
			disposition := "confirmed"
			if o.Kind == "fix" {
				disposition = "not_actionable"
			}
			report.Findings = append(report.Findings, Finding{ID: c.Finding.ID, Text: c.Finding.Text, Sources: []string{c.Original}, Disposition: disposition, Reason: "Verified the complete original evidence."})
		}
	}}
	_, run, runtime := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true {
		t.Fatal(run.Outputs)
	}
	p, _ := ReadPlan(prepared.Plan)
	candidates, err := runtime.candidates(context.Background(), p)
	if err != nil || len(candidates) != 2 || candidates[0].Finding.ID == candidates[1].Finding.ID {
		t.Fatal("reviewer-local collision or duplicate checklist identity", candidates, err)
	}
	fixes, err := runtime.fixCandidates(context.Background(), p)
	if err != nil || len(fixes) != 2 {
		t.Fatal(fixes, err)
	}
}

func TestNestedReviewerConfigurationDriftBlocksBeforeDispatch(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-reviewers-run"}}
	saved := func(instruction string) any {
		return map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-reviewers-run": map[string]any{"options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review", "instructions": instruction}}}}}}
	}
	configFile(t, "", saved("First"))
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	configFile(t, "", saved("Changed selected child only"))
	session := &fakeSession{}
	_, run, _ := executeFixture(t, root, provider, prepared, session, true)
	if run.Status == workflows.RunStatusSucceeded || len(session.calls) > 0 {
		t.Fatal("stale child dispatched", run.Status, session.calls)
	}
}

func TestCycleHooksAdmittedBeforeEffectsAndFollowAllSelectedWork(t *testing.T) {
	root, provider, req := fixture(t)
	configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": []any{map[string]any{"instructions": "Before hook"}}, "after_cycle": []any{map[string]any{"instructions": "After hook"}}}}}})
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"instructions": "Tail work"}}
	req.Options["limits"] = map[string]any{"review_cycles": 1}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	// Validate the actual generated dependency path, including independent
	// verification before the tail and after hooks after that tail.
	raw, err := os.ReadFile(filepath.Join(root, filepath.Dir(prepared.Workflow), "pass.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var flow workflows.Workflow
	if err = yaml.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	stages := []string{}
	for _, g := range p.Groups["tasks"] {
		stages = append(stages, g...)
	}
	var tail, after string
	for id, j := range flow.Jobs {
		if j.With["occurrence"] == stages[len(stages)-1] {
			tail = id
		}
		if j.With["occurrence"] == p.Groups["after_cycle"][0][0] {
			after = id
		}
	}
	var review string
	for id, j := range flow.Jobs {
		if j.With["occurrence"] == stages[0] {
			review = id
		}
	}
	verification := flow.Jobs["verify_findings"]
	if review == "" || !contains(verification.Needs, review) || !strings.HasSuffix(verification.Uses, "/verification.yml") || !contains(flow.Jobs[tail].Needs, "verify_findings") {
		t.Fatal("review verification barrier missing or reordered before tail", verification, flow.Jobs[tail])
	}
	if tail == "" || after == "" || !contains(flow.Jobs[after].Needs, tail) {
		t.Fatal("after hook precedes selected tail", flow.Jobs[after])
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	err = l.Reserve(context.Background(), p.LogicalRun, "review", "all", "exhausted", "selected reviewer pass")
	l.Close()
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{}
	_, run, _ := executeFixture(t, root, provider, prepared, session, true)
	if run.Status == workflows.RunStatusSucceeded || len(session.calls) > 0 {
		t.Fatal("hook ran before exhausted cycle admission", run.Status, session.calls)
	}
}

func TestCommitPolicyProviderRequiresAndHandsOffOriginalInstructions(t *testing.T) {
	for _, completePolicy := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty blocks", true: "accepted handoff"}[completePolicy], func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-commit-split", "options": map[string]any{"policy": map[string]any{"skill": "custom:policy", "instructions": "Return policy only"}}}}
			req.Adapters = map[string]Adapter{"custom:policy": {Identity: "custom:policy", Version: "v1", Kind: "inspection", Cacheable: true, Procedure: [][]Pass{{{ID: "define", Role: "leaf", Assignment: "Define commit instructions only.", Evidence: []string{"original"}}}}}}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			const policy = "Keep each focused commit. Use the repository message format."
			received := false
			session := &fakeSession{assignmentHook: func(raw []byte) {
				var a struct {
					Occurrence Occurrence           `json:"occurrence"`
					Policy     *CommitPolicyHandoff `json:"commit_policy"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					t.Error(err)
					return
				}
				if a.Occurrence.Kind == "cleanup" {
					received = true
					if a.Policy == nil || a.Policy.Prompt == nil || *a.Policy.Prompt != policy || len(a.Policy.Providers) != 1 || a.Policy.Context == "" {
						t.Error("cleanup missing exact accepted handoff", string(raw))
					}
				}
			}, reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Kind == "commit_policy" && completePolicy {
					report.PolicyPrompt = policy
				}
				if o.Kind == "cleanup" {
					report.Cleanup = &Cleanup{Outcome: "no_op", ReviewedHead: s.LocalHead, FinalHead: s.LocalHead, FinalTree: s.LocalTree, Commits: map[string]string{}, BoundaryReasons: []string{"The complete series already follows the verified policy."}, Validation: []string{s.Evidence}}
				}
			}}
			if completePolicy {
				_, run, _ := executeFixture(t, root, provider, prepared, session)
				if run.Status != workflows.RunStatusSucceeded || !received {
					t.Fatal("verified policy did not reach cleanup", run.Status, received)
				}
			} else {
				_, run, _ := executeFixture(t, root, provider, prepared, session, true)
				if run.Status == workflows.RunStatusSucceeded || received {
					t.Fatal("empty provider silently fell back", run.Status, received)
				}
			}
		})
	}
}

func TestCycleHookReuseRequiresNoNewReservation(t *testing.T) {
	root, provider, req := fixture(t)
	configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": []any{map[string]any{"instructions": "Before hook"}}, "after_cycle": []any{map[string]any{"instructions": "After hook"}}}}}})
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}, map[string]any{"instructions": "Tail work"}}
	req.Adapters = map[string]Adapter{"custom:review": reviewAdapter()}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{}
	_, _, runtime := executeFixture(t, root, provider, prepared, session)
	p, _ := ReadPlan(prepared.Plan)
	out, err := runtime.reviewAdmission(context.Background(), p, ActionInput{Cycle: "reuse-only"})
	if err != nil || out.Invoke {
		t.Fatal("reused reviewers admitted hooks", out, err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var count int
	if err = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='review'").Scan(&count); err != nil || count != 1 {
		t.Fatal("reuse consumed another cycle", count, err)
	}
}

func TestCommitPolicyLiteralBuiltinAndInstructionOnlyPreparation(t *testing.T) {
	for _, policy := range []any{nil, map[string]any{"prompt": "Exact literal\n  preserve spacing"}, map[string]any{"instructions": "Define only grouping and message instructions."}} {
		root, provider, req := fixture(t)
		req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-commit-split", "options": map[string]any{"policy": policy}}}
		prepared, err := Prepare(context.Background(), root, provider, req)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := ReadPlan(prepared.Plan)
		providers := 0
		var cleanup Occurrence
		for _, o := range p.Occurrences {
			if o.Kind == "commit_policy" {
				providers++
				if o.Call.Invocation.Skill != "dk-workflows:dk-commit-split" {
					t.Fatal("instruction provider lost cleanup identity", o)
				}
			}
			if o.Kind == "cleanup" {
				cleanup = o
			}
		}
		value, _ := policy.(map[string]any)
		if _, ok := value["instructions"]; ok {
			if providers != 1 || len(cleanup.PolicyProviders) != 1 {
				t.Fatal("missing instruction provider")
			}
			continue
		}
		if providers != 0 {
			t.Fatal("literal policy invoked a provider")
		}
		handoff, err := (&Runtime{Root: root, Provider: provider}).commitPolicy(context.Background(), p, cleanup, p.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if policy == nil {
			if handoff.Prompt != nil {
				t.Fatal("builtin became an explicit prompt")
			}
		} else if handoff.Prompt == nil || *handoff.Prompt != value["prompt"] {
			t.Fatal("literal prompt changed", handoff)
		}
	}
}

func TestCleanupRetainsOnlyItsVerifiedHistoryChange(t *testing.T) {
	initial := Snapshot{Head: "old", LocalHead: "old", LocalTree: "tree", History: "old-series", Discussion: "discussion", Resources: "resources", Settings: "settings"}
	live := initial
	live.Head = "final"
	live.LocalHead = "final"
	live.History = "new-series"
	o := Occurrence{ID: "cleanup", History: true, HeadSensitive: true}
	cleanup := &Cleanup{Outcome: "published", FinalHead: "final", FinalTree: "tree"}
	if !cleanupContextCurrent(initial, live, o, cleanup) {
		t.Fatal("intentional cleanup invalidated its starting policy")
	}
	for _, field := range []string{"tree", "discussion", "resources", "target"} {
		changed := live
		switch field {
		case "tree":
			changed.LocalTree = "changed"
		case "discussion":
			changed.Discussion = "changed"
		case "resources":
			changed.Resources = "changed"
		case "target":
			changed.Target = "changed"
		}
		if cleanupContextCurrent(initial, changed, o, cleanup) {
			t.Fatal("cleanup concealed changed governing context", field)
		}
	}
}
