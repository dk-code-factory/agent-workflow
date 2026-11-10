package prfix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

type blockedPublisher struct {
	*fakeProvider
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (p *blockedPublisher) Execute(ctx context.Context, i Intent) (Receipt, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		}
	}
	return p.fakeProvider.Execute(ctx, i)
}

func TestConcurrentControllersDispatchOnePublication(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-description"}}
	req.Options["finalizers"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-publish"}}
	req.Authorizations = map[string]Authorization{"iterate": {"publish", "Fix and publish this PR.", "chat"}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "description" {
			report.Operations = []Intent{{ID: "parallel-reply", Kind: "reply", Head: s.Head, Lease: s.Head, Target: s.Target, Branch: s.HeadBranch, HeadRepository: s.HeadRepository, Payload: map[string]any{"body": "Verified answer."}, Sources: []string{s.Evidence}}}
		}
	}}
	_, _, runtime := executeFixture(t, root, provider, prepared, session)
	p, _ := ReadPlan(prepared.Plan)
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var intent, claim string
	if err = l.db.QueryRow("SELECT id FROM effects WHERE run=?", p.LogicalRun).Scan(&intent); err != nil {
		t.Fatal(err)
	}
	if err = l.db.QueryRow("SELECT id FROM claims WHERE run=? AND occurrence='audience' AND payload=? AND state='complete'", p.LogicalRun, intent).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the disposable pre-dispatch checkpoint with its real accepted
	// audience claim, then exercise two controllers against the same ledger.
	if _, err = l.db.Exec("UPDATE effects SET state='prepared',receipt='' WHERE run=? AND id=?", p.LogicalRun, intent); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.writes, provider.receipts = 0, nil
	provider.mu.Unlock()
	blocked := &blockedPublisher{fakeProvider: provider, entered: make(chan struct{}), release: make(chan struct{})}
	runtime.Provider = blocked
	contended := make(chan struct{}, 1)
	runtime.publicationContended = func() {
		select {
		case contended <- struct{}{}:
		default:
		}
	}
	other := *runtime
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan error, 2)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked.release) }) }
	launched, joined := 0, 0
	defer func() {
		release()
		cancel()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for joined < launched {
			select {
			case <-results:
				joined++
			case <-timer.C:
				t.Error("publisher cleanup did not join within five seconds")
				return
			}
		}
	}()
	launched++
	go func() { _, err := runtime.publish(ctx, p, ActionInput{Intent: intent, Claim: claim}); results <- err }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("first dispatch did not start")
	}
	launched++
	go func() { _, err := other.publish(ctx, p, ActionInput{Intent: intent, Claim: claim}); results <- err }()
	// Observe actual SQLite contention before allowing the first write to finish.
	// A delayed second goroutine cannot make this assertion pass by reading an
	// already completed effect after release.
	select {
	case err := <-results:
		joined++
		release()
		t.Fatal("second publisher bypassed outstanding dispatch", err)
	case <-contended:
	case <-ctx.Done():
		release()
		t.Fatal("second publisher never reached the contending lock")
	}
	release()
	for n := 0; n < 2; n++ {
		select {
		case err := <-results:
			joined++
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("publishers did not join")
		}
	}
	if blocked.calls.Load() != 1 || provider.writes != 1 {
		t.Fatal("publication dispatched twice", blocked.calls.Load(), provider.writes)
	}
}

func TestPublicationLockCancellationAndRelease(t *testing.T) {
	root := t.TempDir()
	runtime := &Runtime{Root: root}
	unlock, err := runtime.lockPublication(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := &Runtime{Root: root}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = other.lockPublication(ctx)
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("contending lock ignored cancellation", err)
	}
	reuseCtx, reuseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reuseCancel()
	unlock, err = other.lockPublication(reuseCtx)
	if err != nil {
		t.Fatal("released lock not reusable", err)
	}
	unlock()
}

func TestEveryReviewBlockAndFinalizerReviewGetsVerification(t *testing.T) {
	for _, finalizersOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "separated tasks", true: "finalizer reviewers"}[finalizersOnly], func(t *testing.T) {
			root, provider, req := fixture(t)
			a := reviewAdapter()
			b := a
			b.Identity = "custom:second"
			b.Cacheable = false
			req.Adapters = map[string]Adapter{a.Identity: a, b.Identity: b}
			selected := []any{map[string]any{"skill": a.Identity}, map[string]any{"instructions": "Intervening work"}, map[string]any{"skill": b.Identity}, map[string]any{"skill": "dk-workflows:dk-pr-feedback-address"}}
			if finalizersOnly {
				req.Options["finalizers"] = selected
				configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": []any{map[string]any{"instructions": "Before"}}, "after_cycle": []any{map[string]any{"instructions": "After"}}}}}})
			} else {
				req.Options["tasks"] = selected
			}
			prepared, err := Prepare(context.Background(), root, provider, req)
			if err != nil {
				t.Fatal(err)
			}
			p, _ := ReadPlan(prepared.Plan)
			var second, fixer string
			for _, o := range p.Occurrences {
				if o.Call.Invocation.Skill == b.Identity {
					second = o.ID
				}
				if o.Kind == "fix" {
					fixer = o.ID
				}
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.Dir(prepared.Workflow), "pass.yml"))
			if err != nil {
				t.Fatal(err)
			}
			var flow workflows.Workflow
			if err = yaml.Unmarshal(data, &flow); err != nil {
				t.Fatal(err)
			}
			var secondJob, fixJob string
			for id, j := range flow.Jobs {
				if j.With["occurrence"] == second {
					secondJob = id
				}
				if j.With["occurrence"] == fixer {
					fixJob = id
				}
			}
			if !contains(flow.Jobs["verify_findings_2"].Needs, secondJob) || !contains(flow.Jobs[fixJob].Needs, "verify_findings_2") {
				t.Fatal("later reviewer has no verification barrier")
			}
			session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
				if o.ID == second {
					report.Findings = []Finding{{ID: "B1", Text: "Second review candidate", Sources: []string{s.Evidence}}}
				}
			}, reportCandidates: func(o Occurrence, _ Snapshot, candidates []Candidate, report *Report) {
				for _, c := range candidates {
					disposition := "confirmed"
					if o.Kind == "fix" {
						disposition = "not_actionable"
					}
					report.Findings = append(report.Findings, Finding{ID: c.Finding.ID, Text: c.Finding.Text, Sources: []string{c.Original}, Disposition: disposition, Reason: "Verified the sourced candidate."})
				}
			}}
			_, run, _ := executeFixture(t, root, provider, prepared, session)
			if run.Outputs["selected_complete"] != true || session.calls[second] != 1 || session.calls["finding_verification"] != 1 {
				t.Fatal("later verification is stale or missing", run.Status, session.calls)
			}
		})
	}
}

func TestCacheableHooksBindTheirNewReviewCycle(t *testing.T) {
	root, provider, req := fixture(t)
	hook := Adapter{Identity: "custom:hook", Version: "v1", Kind: "hook", Cacheable: true, Procedure: [][]Pass{{{ID: "hook", Role: "leaf", Assignment: "Read the cycle context.", Evidence: []string{"original"}}}}}
	req.Adapters = map[string]Adapter{hook.Identity: hook, "custom:review": reviewAdapter()}
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}}
	configFile(t, "", map[string]any{"processes": map[string]any{"pr-fix": map[string]any{"hooks": map[string]any{"before_cycle": []any{map[string]any{"skill": hook.Identity}}}}}})
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	_, run, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
	p, _ := ReadPlan(prepared.Plan)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runtime.claim(ctx, p, workflows.ActionContext{RunID: run.ID, OperationKey: "next-cycle-hook"}, ActionInput{Occurrence: p.Groups["before_cycle"][0][0], Cycle: "next-cycle", Attempt: 1})
	if err != nil || !out.Invoke {
		t.Fatal("hook reused obsolete cycle", out, err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := getClaim(ctx, l.db, p.LogicalRun, out.Claim)
	if err != nil || c.Cycle != "next-cycle" {
		t.Fatal("hook kept old cycle binding", c, err)
	}
}

func TestFreshPassCyclesCannotReuseOneAllowance(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["limits"] = map[string]any{"review_cycles": 1}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	_, run, runtime := executeFixture(t, root, provider, prepared, &fakeSession{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	input := ActionInput{Plan: prepared.Plan}
	first, err := runtime.handle(ctx, "cycle", workflows.ActionContext{RunID: run.ID, OperationKey: "pass-1"}, input)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := runtime.handle(ctx, "cycle", workflows.ActionContext{RunID: run.ID, OperationKey: "pass-1"}, input)
	if err != nil || first.Cycle != resumed.Cycle {
		t.Fatal("resume changed cycle", err)
	}
	fresh, err := runtime.handle(ctx, "cycle", workflows.ActionContext{RunID: run.ID, OperationKey: "pass-2"}, input)
	if err != nil || first.Cycle == fresh.Cycle {
		t.Fatal("fresh same-input pass reused cycle", first, fresh, err)
	}
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.Reserve(ctx, prepared.LogicalRun, "review", "all", first.Cycle, "selected reviewer pass"); err != nil {
		t.Fatal(err)
	}
	if err = l.Reserve(ctx, prepared.LogicalRun, "review", "all", fresh.Cycle, "selected reviewer pass"); err == nil {
		t.Fatal("fresh noncacheable review bypassed allowance")
	}
}

func TestCandidateReadFailureRetainsItsDiagnostic(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	// This is a failed governing live read, not an empty finding set.
	provider.ev.Metadata.State = "closed"
	out, err := (&Runtime{Root: root, Provider: provider}).handle(context.Background(), "candidates", workflows.ActionContext{}, ActionInput{Plan: prepared.Plan})
	if err == nil || !strings.Contains(err.Error(), "open PR") || out.Report != "" {
		t.Fatal("candidate failure became applicability success", out, err)
	}
}

func TestHistoricalResolutionDoesNotHideIncomingReopening(t *testing.T) {
	root, provider, req := fixture(t)
	provider.ev.Threads = []map[string]any{{"id": "thread", "isResolved": true}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	runtime := &Runtime{Root: root, Provider: provider}
	i := Intent{ID: "old-resolution", Kind: "resolve_thread", Run: p.LogicalRun, PR: p.Identity, Payload: map[string]any{"thread_id": "thread"}}
	receipt := Receipt{ID: i.ID, Kind: i.Kind, PR: i.PR.URL, Head: p.Snapshot.Head, Result: map[string]any{"id": "thread", "isResolved": true}, Evidence: []string{"fixture"}}
	path, err := artifact(root, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.Exec("INSERT INTO effects VALUES(?,?,?,'completed',?)", p.LogicalRun, i.ID, string(jsonBytes(i)), path); err != nil {
		t.Fatal(err)
	}
	provider.ev.Threads = []map[string]any{{"id": "thread", "isResolved": false}}
	live, err := capture(context.Background(), root, provider, p)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := runtime.ownDiscussionDelta(context.Background(), p, p.Snapshot.Discussion, live)
	if err != nil || allowed {
		t.Fatal("human reopening normalized away", allowed, err)
	}
}

// Only the verifier blocks; all other reads use disposable local fixtures.
type deadlineVerifier struct {
	*fakeProvider
	observed     chan time.Duration
	liveObserved chan time.Duration
}

func (p *deadlineVerifier) Capture(ctx context.Context, id Identity) (PREvidence, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return PREvidence{}, errors.New("missing live classification deadline")
	}
	select {
	case p.liveObserved <- time.Until(deadline):
	default:
	}
	return p.fakeProvider.Capture(ctx, id)
}

func (p *deadlineVerifier) VerifyCI(ctx context.Context, _ Identity, _ string, _ []CIClassification, _ map[string]map[string]any) (CIProof, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return CIProof{}, errors.New("missing classification deadline")
	}
	remaining := time.Until(deadline)
	p.observed <- remaining
	if remaining > 1500*time.Millisecond {
		return CIProof{}, errors.New("configured classification deadline not applied")
	}
	<-ctx.Done()
	return CIProof{}, ctx.Err()
}
func TestCIClassificationHonorsItsConfiguredDeadline(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-ci", "options": map[string]any{"classification_timeout_seconds": 1}}}
	provider.ci.Checks[0].Conclusion = "failure"
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ReadPlan(prepared.Plan)
	session := &fakeSession{reportHook: func(o Occurrence, s Snapshot, report *Report) {
		if o.Kind == "ci" {
			report.CI = &CIReport{Head: s.Head, Checks: []string{"1"}, Classifications: []CIClassification{{CheckID: "1", Kind: "flake"}}, Status: "failure", Evidence: []string{s.Evidence}}
		}
	}}
	// The first classification has no verifier and stops after accepting the real
	// native CI claim. Supply a controlled verifier for the same retained claim.
	_, _, runtime := executeFixture(t, root, provider, prepared, session, true)
	l, err := OpenLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var claim string
	if err = l.db.QueryRow("SELECT id FROM claims WHERE occurrence=?", p.Groups["tasks"][0][0]).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	verifier := &deadlineVerifier{fakeProvider: provider, observed: make(chan time.Duration, 1), liveObserved: make(chan time.Duration, 1)}
	runtime.Provider = verifier
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = runtime.classifyCI(ctx, p, ActionInput{Claim: claim})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("classification escaped configured deadline", err)
	}
	select {
	case remaining := <-verifier.liveObserved:
		if remaining > time.Second {
			t.Fatal("live read received parent deadline instead of classification window", remaining)
		}
	default:
		t.Fatal("classification live read was not exercised")
	}
	select {
	case remaining := <-verifier.observed:
		if remaining > time.Second {
			t.Fatal("wrong classification deadline", remaining)
		}
	default:
		if ctx.Err() != nil {
			t.Fatal("only the parent deadline was observed")
		}
		// A bounded live read may consume the window before the verifier. In that
		// case the classification deadline must precede the still-active parent.
	}
}
