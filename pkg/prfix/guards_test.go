package prfix

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeUsesFinalDiffAndExactEffectiveModes(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "dk-workflows:dk-pr-split", "options": map[string]any{"added_lines_threshold": 7, "evacuate_unrelated": "manual", "split_related": "disabled"}}}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	o, _ := p.Occurrence(p.Groups["tasks"][0][0])
	runtime := &Runtime{Root: root, Provider: provider}
	a, e := runtime.scopeAccounting(context.Background(), p, o)
	if e != nil {
		t.Fatal(e)
	}
	if a.Threshold != 7 || a.EvacuateMode != "manual" || a.RelatedMode != "disabled" || a.AddedLines != 0 {
		t.Fatal(a)
	}
	report := &ScopeReport{AddedLines: a.AddedLines, Threshold: a.Threshold, EvacuateMode: a.EvacuateMode, RelatedMode: a.RelatedMode, Enabled: a.Enabled, Assessment: "Keep this bounded change together.", Evidence: []string{p.Snapshot.Evidence}}
	if e = runtime.validateScope(context.Background(), p, o, report); e != nil {
		t.Fatal(e)
	}
	report.RelatedMode = "automatic"
	if e = runtime.validateScope(context.Background(), p, o, report); e == nil {
		t.Fatal("scope assessment ignored disabled related split")
	}
	if e = os.WriteFile(filepath.Join(root, "code.txt"), []byte("one\ntwo\nthree\n"), 0600); e != nil {
		t.Fatal(e)
	}
	a, e = runtime.scopeAccounting(context.Background(), p, o)
	if e != nil || a.AddedLines != 3 {
		t.Fatal("final diff accounting", a, e)
	}
}
func TestCleanupRequiresWholeSeriesTreeAndVerifiedPublication(t *testing.T) {
	root, provider, req := fixture(t)
	ctx := context.Background()
	if _, e := git(ctx, root, "commit", "--allow-empty", "-m", "A separate fixture commit"); e != nil {
		t.Fatal(e)
	}
	sha, e := git(ctx, root, "rev-parse", "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	provider.ev.Metadata.Head.SHA = sha
	prepared, e := Prepare(ctx, root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	s := p.Snapshot
	runtime := &Runtime{Root: root, Provider: provider}
	cleanup := &Cleanup{Outcome: "no_op", ReviewedHead: s.LocalHead, FinalHead: s.LocalHead, FinalTree: s.LocalTree, Commits: map[string]string{}, BoundaryReasons: []string{"One independent commit."}, Validation: []string{s.Evidence}}
	if e = runtime.validateCleanup(ctx, p, s, s, cleanup); e == nil {
		t.Fatal("cleanup omitted a commit")
	}
	cleanup.Commits[sha] = sha
	if e = runtime.validateCleanup(ctx, p, s, s, cleanup); e != nil {
		t.Fatal(e)
	}
	cleanup.Outcome = "published"
	cleanup.Receipt = s.Evidence
	if e = runtime.validateCleanup(ctx, p, s, s, cleanup); e == nil {
		t.Fatal("arbitrary evidence substituted for a push receipt")
	}
	cleanup.Outcome = "prepared"
	changed := s
	changed.LocalTree = "changed"
	if e = runtime.validateCleanup(ctx, p, s, changed, cleanup); e == nil {
		t.Fatal("cleanup changed the final tree")
	}
}
func TestCIRerunVerifiesHeadAndAttemptBeforeDispatch(t *testing.T) {
	root, _, req := fixture(t)
	id, _ := ParseIdentity(req.PR)
	head := strings.Repeat("c", 40)
	path := "repos/owner/repo/actions/runs/11"
	responses := map[string]any{path: map[string]any{"head_sha": head, "run_attempt": 2, "status": "completed", "conclusion": "failure"}}
	update := fakeGH(t, root, responses)
	g := &GitHub{Root: root}
	intent := Intent{PR: id, Head: head, Payload: map[string]any{"run_id": "11", "attempt": float64(2)}}
	if e := g.VerifyRerun(context.Background(), intent); e != nil {
		t.Fatal(e)
	}
	intent.Head = strings.Repeat("d", 40)
	if e := g.VerifyRerun(context.Background(), intent); e == nil {
		t.Fatal("rerun admitted for another head")
	}
	intent.Head = head
	responses[path] = map[string]any{"head_sha": head, "run_attempt": 3, "status": "completed", "conclusion": "failure"}
	update(responses)
	if e := g.VerifyRerun(context.Background(), intent); e == nil {
		t.Fatal("rerun admitted stale attempt")
	}
}
func TestPublicationDiscussionDeltaAllowsOnlyReconciledOwnWrites(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	l, e := OpenLedger(root)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	runtime := &Runtime{Root: root, Provider: provider}
	intent := Intent{ID: "own", Run: p.LogicalRun, PR: p.Identity, Kind: "reply", Head: p.Snapshot.Head, Payload: map[string]any{"body": "Completed reply."}}
	receipt := Receipt{ID: intent.ID, Kind: intent.Kind, PR: p.Identity.URL, Head: intent.Head, Result: map[string]any{"id": float64(9)}, Evidence: []string{"local fixture"}}
	path, e := artifact(root, receipt)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.db.Exec("INSERT INTO effects VALUES(?,?,?,'completed',?)", p.LogicalRun, intent.ID, string(jsonBytes(intent)), path); e != nil {
		t.Fatal(e)
	}
	provider.ev.Comments = append(provider.ev.Comments, map[string]any{"id": float64(9), "body": "Completed reply."})
	live, e := capture(context.Background(), root, provider, p)
	if e != nil {
		t.Fatal(e)
	}
	allowed, e := runtime.ownDiscussionDelta(context.Background(), p, p.Snapshot.Discussion, live)
	if e != nil || !allowed {
		t.Fatal("exact own reply invalidated remaining intents", allowed, e)
	}
	provider.ev.Comments = append(provider.ev.Comments, map[string]any{"id": float64(10), "body": "New incoming instruction."})
	live, e = capture(context.Background(), root, provider, p)
	if e != nil {
		t.Fatal(e)
	}
	allowed, e = runtime.ownDiscussionDelta(context.Background(), p, p.Snapshot.Discussion, live)
	if e != nil || allowed {
		t.Fatal("incoming instruction ignored", allowed, e)
	}
}
