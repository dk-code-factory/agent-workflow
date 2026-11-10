package prfix

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootInvocationInstructionsReachManagedLeaves(t *testing.T) {
	root, provider, req := fixture(t)
	exact := "  Do not modify generated/.\nPreserve this spacing.  \n"
	configFile(t, "", map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-fix": map[string]any{"instructions": "User default."}}})
	id, _ := ParseIdentity(req.PR)
	configFile(t, id.Repository, map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-fix": map[string]any{"instructions": exact}}})
	req.Options["tasks"] = []any{map[string]any{"instructions": "Inspect only."}}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	session := &fakeSession{assignmentHook: func(raw []byte) {
		var assignment struct {
			Owning     string     `json:"owning_invocation_instructions"`
			User       string     `json:"user_instruction"`
			Occurrence Occurrence `json:"occurrence"`
		}
		if err := json.Unmarshal(raw, &assignment); err != nil {
			t.Error(err)
			return
		}
		if assignment.Owning != exact || assignment.User != req.Instructions {
			t.Error("governing instruction text lost", assignment.Owning, assignment.User)
		}
		seen[assignment.Occurrence.Kind] = true
	}}
	executeFixture(t, root, provider, prepared, session)
	if !seen["hook"] || !seen["completion"] {
		t.Fatal("missing managed instruction coverage", seen)
	}
}

func TestRootNativeControlsBlockBeforeClaims(t *testing.T) {
	root, provider, req := fixture(t)
	configFile(t, "", map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-fix": map[string]any{"agent": map[string]any{"reasoning_effort": "high"}}}})
	if _, err := Prepare(context.Background(), root, provider, req); err == nil || !strings.Contains(err.Error(), "deterministic PR-fix") {
		t.Fatal("root native controls silently ignored", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent-workflow/pr-fix/ledger.sqlite")); !os.IsNotExist(err) {
		t.Fatal("unsupported controls admitted claims", err)
	}
}

func TestWritableReviewerBlocksBeforeClaims(t *testing.T) {
	root, provider, req := fixture(t)
	adapter := reviewAdapter()
	adapter.Writes = []string{"code.txt"}
	req.Adapters = map[string]Adapter{"custom:review": adapter}
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}}
	if _, err := Prepare(context.Background(), root, provider, req); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatal("writable reviewer admitted", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent-workflow/pr-fix/ledger.sqlite")); !os.IsNotExist(err) {
		t.Fatal("conflicting reviewer admitted claims", err)
	}
}

func TestRepositoryInstructionsArePinnedOutsideContentDiff(t *testing.T) {
	root, provider, req := fixture(t)
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested/code.go"), []byte("package fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "nested/code.go"}, {"commit", "-m", "Add nested scope"}} {
		if _, err := git(context.Background(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := git(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	provider.ev.Metadata.Head.SHA = head
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte("AGENTS.md\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AGENTS.md", "nested/AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("Original instructions."), 0600); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := Prepare(context.Background(), root, provider, req)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ReadPlan(prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Snapshot.RepositoryInstructions) != 2 {
		t.Fatal("scoped instructions omitted", p.Snapshot.RepositoryInstructions)
	}
	if err := os.WriteFile(filepath.Join(root, "nested/AGENTS.md"), []byte("Changed governing instructions."), 0600); err != nil {
		t.Fatal(err)
	}
	now, err := (&Runtime{Root: root, Provider: provider}).live(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if now.LocalTree != p.Snapshot.LocalTree || now.Diff != p.Snapshot.Diff || now.Resources == p.Snapshot.Resources {
		t.Fatal("ignored instruction drift not separately captured", now.LocalTree, p.Snapshot.LocalTree, now.Resources)
	}
}

func TestResourceGrowthCannotBypassByteBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	open := func(path string) (*os.File, error) {
		if err := os.WriteFile(path, []byte("eight!!!"), 0600); err != nil {
			return nil, err
		}
		return os.Open(path)
	}
	if _, err := resourceDigestWithOpen(path, 4, open); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatal("growth after stat bypassed the actual read bound", err)
	}
}
