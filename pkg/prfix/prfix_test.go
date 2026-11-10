package prfix

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type fakeProvider struct {
	stopWorkers      func()
	mu               sync.Mutex
	ev               PREvidence
	ci               CIObservation
	writes           int
	receipts         map[string]Receipt
	lose             bool
	mergeReady       bool
	discussionWrites bool
}

func (f *fakeProvider) replaceWorkerService(stop func()) {
	if f.stopWorkers != nil {
		f.stopWorkers()
	}
	f.stopWorkers = stop
}

func (f *fakeProvider) PullRequestNumbers(context.Context, Identity) ([]string, error) {
	return []string{"1"}, nil
}
func (f *fakeProvider) MergeReady(_ context.Context, _ Identity, head string) (MergeProof, error) {
	return MergeProof{Head: head, Satisfied: f.mergeReady, Evidence: []string{"fake repository requirements"}}, nil
}
func (f *fakeProvider) VerifyRerun(_ context.Context, i Intent) error {
	if i.Head != f.ev.Metadata.Head.SHA {
		return errors.New("wrong head")
	}
	return nil
}
func (f *fakeProvider) Capture(_ context.Context, _ Identity) (PREvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ev, nil
}
func (f *fakeProvider) ObserveCI(_ context.Context, _ Identity, head string) (CIObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.ci
	o.Head = head
	return o, nil
}
func (f *fakeProvider) Execute(_ context.Context, i Intent) (Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	receipt := Receipt{i.ID, i.Kind, i.PR.URL, i.Head, jsonObject(i.Payload), []string{"fake-observation"}}
	if f.receipts == nil {
		f.receipts = map[string]Receipt{}
	}
	if f.discussionWrites && (i.Kind == "reply" || i.Kind == "reply_inline") {
		comment := map[string]any{"id": float64(100 + f.writes), "body": stringValue(i.Payload, "body"), "user": map[string]any{"login": f.ev.Viewer, "id": i.ActorID}}
		if i.Kind == "reply" {
			f.ev.Comments = append(f.ev.Comments, comment)
		} else {
			comment["in_reply_to_id"] = i.Payload["comment_id"]
			f.ev.Inline = append(f.ev.Inline, comment)
		}
		receipt.Result = comment
	}
	f.receipts[i.ID] = receipt
	if i.Kind == "ci_rerun" {
		for n := range f.ci.Checks {
			f.ci.Checks[n].Conclusion = "success"
			f.ci.Checks[n].Attempt++
		}
	}
	if f.lose {
		return Receipt{}, errors.New("response lost after write")
	}
	return receipt, nil
}
func (f *fakeProvider) Reconcile(_ context.Context, i Intent) (string, Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	receipt, ok := f.receipts[i.ID]
	if ok {
		return "completed", receipt, nil
	}
	return "no_effect", Receipt{i.ID, i.Kind, i.PR.URL, i.Head, jsonObject(map[string]bool{"absent": true}), []string{"fake read-only absence"}}, nil
}
func fixture(t *testing.T) (string, *fakeProvider, PrepareRequest) {
	t.Helper()
	ctx := context.Background()
	// Supervision re-execs this test binary for each local command. Retain race
	// instrumentation and failure reporting without its per-child exit delay.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	for _, kind := range []string{"CONFIG", "DATA", "STATE"} {
		t.Setenv("XDG_"+kind+"_HOME", filepath.Join(outer, strings.ToLower(kind)))
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "fixture@example.invalid"}, {"config", "user.name", "Fixture"}, {"remote", "add", "origin", "https://github.com/owner/repo.git"}} {
		if _, e := git(ctx, root, args...); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".agent-workflow/\nworkflows/.pr-fix/\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "code.txt"), []byte("baseline\n"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "Initial fixture"}} {
		if _, e := git(ctx, root, args...); e != nil {
			t.Fatal(e)
		}
	}
	sha, e := git(ctx, root, "rev-parse", "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	ev := PREvidence{Viewer: "author", ViewerID: 7, Comments: []map[string]any{}, Reviews: []map[string]any{}, Inline: []map[string]any{}, Threads: []map[string]any{}}
	ev.Metadata.URL = "https://github.com/owner/repo/pull/1"
	ev.Metadata.Number = 1
	ev.Metadata.State = "open"
	ev.Metadata.User.Login = "author"
	ev.Metadata.Head.SHA = sha
	ev.Metadata.Head.Ref = "main"
	ev.Metadata.Head.Repo.FullName = "owner/repo"
	ev.Metadata.Base.SHA = sha
	ev.Metadata.Base.Ref = "main"
	provider := &fakeProvider{ev: ev, ci: CIObservation{Checks: []CICheck{{ID: "1", Name: "test", Status: "completed", Conclusion: "success"}}}}
	req := PrepareRequest{PR: ev.Metadata.URL, Role: "leaf", Instructions: "Fix this PR with deterministic checks.", Options: map[string]any{"tasks": []any{}, "finalizers": []any{}}, Config: clirunner.Config{Agents: map[string]clirunner.Agent{"leaf": {Provider: "claude-sdk"}}}, Capabilities: func(string) (workflows.ProviderCapabilities, error) {
		return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
	}}
	return root, provider, req
}
func TestPrepareEmptyPlanIsInspectableAndStillHasCompletion(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Groups["tasks"]) != 0 || len(p.Groups["finalizers"]) != 0 || len(p.Omitted) != 6 {
		t.Fatal("empty lists did not replace defaults", p.Groups, p.Omitted)
	}
	ex := &workflows.Executor{WorkspaceDir: root, ApplicationActions: &workflows.ActionRegistry{}}
	runtime := &Runtime{Root: root, Provider: provider}
	if e = runtime.Register(ex.ApplicationActions); e != nil {
		t.Fatal(e)
	}
	if _, e = ex.Load(prepared.Workflow); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(strings.Replace(prepared.Workflow, "main.yml", "pass.yml", 1))))
	if e != nil || !strings.Contains(string(b), "completion.yml") {
		t.Fatalf("missing live completion %s %v", b, e)
	}
	again, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	if prepared.LogicalRun != again.LogicalRun || prepared.Workflow != again.Workflow || prepared.Plan != again.Plan {
		t.Fatal("unchanged prepare did not retain logical identity", prepared, again)
	}
}
func TestDefaultPlanBlocksMissingReviewersBeforeClaims(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options = nil
	_, e := Prepare(context.Background(), root, provider, req)
	if e == nil || !strings.Contains(e.Error(), "missing reviewer") {
		t.Fatalf("expected reviewer preflight blocker, got %v", e)
	}
	if _, e = os.Stat(filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")); !os.IsNotExist(e) {
		t.Fatal("preflight consumed logical state", e)
	}
}
func TestUnavailableOptionalAndUnsupportedControlsBlock(t *testing.T) {
	for _, tc := range []struct {
		call    map[string]any
		adapter bool
		want    string
	}{{map[string]any{"skill": "custom:review", "required": false}, false, "requires an available"}, {map[string]any{"skill": "custom:review", "agent": map[string]any{"reasoning_effort": "high"}}, true, "control"}} {
		t.Run(tc.want, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{tc.call}
			if tc.adapter {
				req.Adapters = map[string]Adapter{"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "correctness", Role: "leaf", Assignment: "Review only correctness.", Evidence: []string{"source-coverage"}}}}}}
			}
			_, e := Prepare(context.Background(), root, provider, req)
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, e)
			}
		})
	}
}
func TestRetainedBudgetsContinuationAndDeadlines(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["limits"] = map[string]any{"review_cycles": 1}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	run := prepared.LogicalRun
	if e = l.Reserve(ctx, run, "review", "all", "cycle1", "review"); e != nil {
		t.Fatal(e)
	}
	if e = l.Reserve(ctx, run, "review", "all", "cycle1", "review"); e != nil {
		t.Fatal(e)
	}
	if e = l.Reserve(ctx, run, "review", "all", "cycle2", "review"); e == nil {
		t.Fatal("budget exceeded")
	}
	a := Authorization{"user-message-2", "Continue reviewing this PR.", "chat:2"}
	n, e := l.Continue(ctx, run, a, 2)
	if e != nil || n != 3 {
		t.Fatal(n, e)
	}
	n, e = l.Continue(ctx, run, a, 2)
	if e != nil || n != 3 {
		t.Fatal("duplicate grant", n, e)
	}
	a.Instruction = "different"
	if _, e = l.Continue(ctx, run, a, 2); e == nil {
		t.Fatal("conflicting replay accepted")
	}
	start := time.Now()
	first, e := l.Deadline(ctx, run, "head", start, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	same, e := l.Deadline(ctx, run, "head", start.Add(time.Hour), time.Hour)
	if e != nil || !same.Equal(first) {
		t.Fatal("deadline reset", same, first, e)
	}
	var success atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := l.Reserve(ctx, run, "ci_rerun", "head", "operation", "transient"); e == nil {
				success.Add(1)
			}
		}()
	}
	joined := make(chan struct{})
	go func() { wg.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("reservation writers did not join within their deadline")
	}
	if success.Load() != 12 {
		t.Fatal("idempotent concurrent reservations failed")
	}
	var count int
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='ci_rerun'").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
}
func TestContentFingerprintPreservesHistoryOnlyCleanup(t *testing.T) {
	s := Snapshot{Identity: Identity{URL: "pr"}, Head: "old", LocalHead: "old", Target: "target", HeadTree: "tree", LocalTree: "tree", History: "old-history", Discussion: "discussion"}
	next := s
	next.Head = "new"
	next.LocalHead = "new"
	next.History = "new-history"
	if s.Fingerprint(false, false) != next.Fingerprint(false, false) {
		t.Fatal("tree-preserving cleanup invalidated content review")
	}
	if s.Fingerprint(true, false) == next.Fingerprint(true, false) || s.Fingerprint(false, true) == next.Fingerprint(false, true) {
		t.Fatal("history/head-sensitive evidence incorrectly reused")
	}
	next = s
	next.Discussion = "new discussion"
	if s.Fingerprint(false, false) == next.Fingerprint(false, false) {
		t.Fatal("unchanged head concealed stale discussion")
	}
}

// Compile-time guarantees keep fake-provider tests separate from live services.
var _ Provider = (*fakeProvider)(nil)
var _ = json.Valid

type fakeSession struct {
	root              string
	mu                sync.Mutex
	calls             map[string]int
	omitChecklist     bool
	sessions          map[string]string
	hook              func(Occurrence)
	assignmentHook    func([]byte)
	reportHook        func(Occurrence, Snapshot, *Report)
	reportCandidates  func(Occurrence, Snapshot, []Candidate, *Report)
	reportReplies     func(Occurrence, Snapshot, []retainedObligation, *Report)
	failCheckpoint    string
	nativeError       func(Occurrence) error
	failAfterReport   func(Occurrence) error
	checkpointHook    func(string, ActionOutput) error
	factoryError      error
	factoryErrorAfter int32
	failureTimeout    time.Duration
}

func (f *fakeSession) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true, Interrupt: true}, nil
}
func (f *fakeSession) RunTurn(ctx context.Context, req workflows.SessionRequest, _ func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if err := RestoreSession(ctx, f.root, &req); err != nil {
		return workflows.SessionResult{}, err
	}
	var assignment struct {
		Occurrence         Occurrence           `json:"occurrence"`
		PublicationPayload *Intent              `json:"publication_payload"`
		Candidates         []Candidate          `json:"candidates"`
		ReplyObligations   []retainedObligation `json:"reply_obligations"`
		Snapshot           Snapshot             `json:"snapshot"`
		Checklist          []ChecklistItem      `json:"checklist"`
		Assignment         json.RawMessage      `json:"assignment"`
	}
	if e := json.Unmarshal([]byte(req.Prompt), &assignment); e != nil {
		return workflows.SessionResult{}, e
	}
	followup := len(assignment.Assignment) > 0 && assignment.Assignment[0] == '{'
	if followup {
		if e := json.Unmarshal(assignment.Assignment, &assignment); e != nil {
			return workflows.SessionResult{}, e
		}
	}
	if f.assignmentHook != nil {
		f.assignmentHook([]byte(req.Prompt))
	}
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
		f.sessions = map[string]string{}
	}
	f.calls[assignment.Occurrence.ID]++
	n := f.calls[assignment.Occurrence.ID]
	session := req.SessionID
	if session == "" {
		session = "fixture-" + newID()
	}
	if prior := f.sessions[assignment.Occurrence.ID]; followup && prior != session {
		f.mu.Unlock()
		return workflows.SessionResult{}, errors.New("checklist changed sessions")
	}
	f.sessions[assignment.Occurrence.ID] = session
	f.mu.Unlock()
	if f.hook != nil {
		f.hook(assignment.Occurrence)
	}
	if f.nativeError != nil {
		if e := f.nativeError(assignment.Occurrence); e != nil {
			return workflows.SessionResult{Status: workflows.RunStatusFailed, SessionID: session}, e
		}
	}
	report := Report{Complete: true, Findings: []Finding{}, Evidence: []string{assignment.Snapshot.Evidence}, Blockers: []string{}}
	if !f.omitChecklist || n > 1 {
		for _, item := range assignment.Checklist {
			report.Checklist = append(report.Checklist, Check{ID: item.ID, Status: "completed", Evidence: report.Evidence})
		}
	}
	if assignment.PublicationPayload != nil {
		report.Audience = &AudienceReport{Fingerprint: assignment.PublicationPayload.Fingerprint(), Audience: "PR participants", Suitable: true, Evidence: report.Evidence}
	}
	if f.reportHook != nil {
		f.reportHook(assignment.Occurrence, assignment.Snapshot, &report)
	}
	if f.reportReplies != nil {
		f.reportReplies(assignment.Occurrence, assignment.Snapshot, assignment.ReplyObligations, &report)
	}
	if f.reportCandidates != nil {
		f.reportCandidates(assignment.Occurrence, assignment.Snapshot, assignment.Candidates, &report)
	}
	result := workflows.SessionResult{Status: workflows.RunStatusSucceeded, SessionID: session, Outputs: map[string]any{"text": string(jsonBytes(report))}}
	if f.failAfterReport != nil {
		if err := f.failAfterReport(assignment.Occurrence); err != nil {
			result.Status = workflows.RunStatusFailed
			return result, err
		}
	}
	return result, nil
}
func executeFixture(t *testing.T, root string, provider interface {
	Provider
	replaceWorkerService(func())
}, prepared Prepared, session *fakeSession, expectFailure ...bool) (*workflows.Executor, *workflows.Run, *Runtime) {
	t.Helper()
	// Replacement executions share the worker store. Join the prior scheduler
	// before installing another factory, so it cannot consume the new session's
	// messages with an earlier fake runner. Keep the current service for resume.
	provider.replaceWorkerService(nil)
	// Race-instrumented SQLite snapshots make the full default plan substantially
	// slower than individual recovery fixtures; keep a deadline below make test's.
	timeout := 10 * time.Minute
	if session.failureTimeout > 0 {
		timeout = session.failureTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	store, e := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = store.Close() })
	session.root = root
	slots := make(chan struct{}, 1)
	capacity := &workflows.Capacity{Slots: slots}
	var factoryCalls atomic.Int32
	workers := &workflows.WorkerManager{Store: store, Slots: slots, Capacity: capacity, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		if session.factoryError != nil && factoryCalls.Add(1) > session.factoryErrorAfter {
			return nil, nil, nil, session.factoryError
		}
		return session, []string{}, nil, nil
	}}
	if e = workers.Init(ctx); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { defer close(done); workers.Serve(ctx) }()
	stopWorkers := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("managed worker service did not stop within five seconds")
		}
	}
	provider.replaceWorkerService(stopWorkers)
	t.Cleanup(stopWorkers)
	runtime := &Runtime{Root: root, Store: store, Workers: workers, Provider: provider, PollInterval: time.Millisecond}
	var failed atomic.Bool
	runtime.BeforeCheckpoint = func(name string, _ ActionOutput) error {
		if session.checkpointHook != nil {
			if e := session.checkpointHook(name, ActionOutput{}); e != nil {
				return e
			}
		}
		if name == session.failCheckpoint && failed.CompareAndSwap(false, true) {
			return errors.New("injected lost application checkpoint")
		}
		return nil
	}
	registry := &workflows.ActionRegistry{}
	if e = runtime.Register(registry); e != nil {
		t.Fatal(e)
	}
	ex := &workflows.Executor{WorkspaceDir: root, Store: store, Slots: slots, Capacity: capacity, Roles: prepared.Roles, ApplicationActions: registry, MaxParallel: 1, BeforeFinalize: workers.StopRun}
	run, e := ex.Prepare(ctx, workflows.RunRequest{Ref: prepared.Workflow, Inputs: prepared.Inputs})
	if e != nil {
		t.Fatal(e)
	}
	ledger, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	e = ledger.Execution(ctx, prepared.LogicalRun, prepared.RequestID, prepared.Plan, run.ID)
	ledger.Close()
	if e != nil {
		t.Fatal(e)
	}
	run, e = ex.Resume(ctx, run.ID, nil, false)
	if e != nil && len(expectFailure) == 0 {
		t.Fatalf("workflow failed: %v", e)
	}
	if len(expectFailure) > 0 && e == nil {
		t.Fatal("fault injection did not fail execution")
	}
	return ex, run, runtime
}

// Each recovery call has its own failure deadline even if the original worker
// service expires; queued native messages must not wait for the package timeout.
func resumeFixture(t *testing.T, ex *workflows.Executor, id string, recover bool) (*workflows.Run, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return ex.Resume(ctx, id, nil, recover)
}

func TestEmptyPlanExecutesManagedLiveGateWithSameSessionFollowup(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	session := &fakeSession{omitChecklist: true}
	ex, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Status != workflows.RunStatusSucceeded || run.Outputs["selected_complete"] != true || run.Outputs["pr_ready"] != false {
		t.Fatal("empty plan completion/readiness conflated", run.Status, run.Outputs)
	}
	if session.calls["completion"] != 2 {
		t.Fatal("managed completion did not recheck", session.calls)
	}
	if _, e = resumeFixture(t, ex, run.ID, false); e != nil {
		t.Fatal("checkpoint replay", e)
	}
	if session.calls["completion"] != 2 {
		t.Fatal("completion worker was repeated on resume", session.calls)
	}
}
func TestRepeatedParallelReviewPassesHaveDistinctClaims(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-reviewers-run", "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:review"}, map[string]any{"skill": "custom:review"}}}}}
	req.Adapters = map[string]Adapter{"custom:review": {Identity: "custom:review", Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "standards", Role: "leaf", Assignment: "Review documented standards.", Evidence: []string{"standards"}}, {ID: "spec", Role: "leaf", Assignment: "Review originating spec.", Evidence: []string{"spec"}}}}}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	reviews := 0
	for _, o := range p.Occurrences {
		if o.Review {
			reviews++
		}
	}
	if reviews != 4 {
		t.Fatal("missing required pass expansion", reviews)
	}
	session := &fakeSession{}
	_, run, _ := executeFixture(t, root, provider, prepared, session)
	if run.Outputs["selected_complete"] != true {
		t.Fatal(run.Outputs)
	}
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var cycles, claims int
	if e = l.db.QueryRow("SELECT count(*) FROM reservations WHERE kind='review'").Scan(&cycles); e != nil {
		t.Fatal(e)
	}
	if e = l.db.QueryRow("SELECT count(*) FROM claims WHERE occurrence<>'completion'").Scan(&claims); e != nil {
		t.Fatal(e)
	}
	if cycles != 1 || claims != 4 {
		t.Fatal("parallel siblings did not share one cycle or collided", cycles, claims)
	}
}

func TestLostLifecycleCheckpointsReconcileWithoutDuplicateWorkers(t *testing.T) {
	for _, operation := range []string{"claim", "worker_spawn", "worker_send", "finish", "close"} {
		t.Run(operation, func(t *testing.T) {
			root, provider, req := fixture(t)
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{failCheckpoint: operation}
			ex, run, _ := executeFixture(t, root, provider, prepared, session, true)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			run, e = ex.Resume(ctx, run.ID, nil, true)
			if e != nil {
				t.Fatal("reconciliation blocked", e)
			}
			if run.Status != workflows.RunStatusSucceeded || session.calls["completion"] != 1 {
				t.Fatal("duplicate native effect", run.Status, session.calls)
			}
		})
	}
}
func TestUncertainPublicationAndReceiptCheckpointNeverDuplicateWrite(t *testing.T) {
	for _, fault := range []string{"provider_response", "publish", "receipt"} {
		t.Run(fault, func(t *testing.T) {
			root, provider, req := fixture(t)
			req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description"}}
			req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
			req.Authorizations = map[string]Authorization{"iterate": {RequestID: "user-1", Instruction: "Fix this PR and reply to the feedback.", Source: "chat:1"}}
			prepared, e := Prepare(context.Background(), root, provider, req)
			if e != nil {
				t.Fatal(e)
			}
			p, e := ReadPlan(prepared.Plan)
			if e != nil {
				t.Fatal(e)
			}
			session := &fakeSession{failCheckpoint: fault, reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.Kind == "description" {
					report.Operations = []Intent{{ID: "reply-once", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "The requested correction is complete."}, Sources: []string{s.Evidence}}}
				}
			}}
			provider.lose = fault == "provider_response"
			ex, run, _ := executeFixture(t, root, provider, prepared, session, true)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			run, e = ex.Resume(ctx, run.ID, nil, true)
			if e != nil {
				t.Fatal("publication reconciliation failed", e)
			}
			if run.Status != workflows.RunStatusSucceeded || provider.writes != 1 {
				t.Fatal("duplicate or incomplete publication", run.Status, provider.writes)
			}
			l, e := OpenLedger(root)
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			var state string
			if e = l.db.QueryRow("SELECT state FROM effects WHERE run=? AND id='reply-once'", p.LogicalRun).Scan(&state); e != nil || state != "completed" {
				t.Fatal("application receipt not reconciled", state, e)
			}
			if _, e = ex.Resume(ctx, run.ID, nil, false); e != nil {
				t.Fatal(e)
			}
			if provider.writes != 1 {
				t.Fatal("checkpoint replay duplicated write")
			}
		})
	}
}
