package prfix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPinnedSourceProvenance(t *testing.T) {
	var manifest struct {
		Version string            `json:"version"`
		Files   map[string]string `json:"files"`
	}
	raw, e := source.ReadFile("source/PROVENANCE.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &manifest); e != nil {
		t.Fatal(e)
	}
	if manifest.Version != Version || len(manifest.Files) < 100 {
		t.Fatal("incomplete pinned source", manifest.Version, len(manifest.Files))
	}
	for path, want := range manifest.Files {
		b, e := source.ReadFile("source/" + path)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatalf("source contract drift: %s", path)
		}
	}
}
func configFile(t *testing.T, repository string, record any) string {
	t.Helper()
	name := "user"
	if repository != "" {
		name = digest(repository)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "dk-workflows", "v2", name+".json")
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, jsonBytes(record), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func TestSettingsScopesReplaceListsAndPreserveExactInstructions(t *testing.T) {
	root, provider, req := fixture(t)
	id, e := ParseIdentity(req.PR)
	if e != nil {
		t.Fatal(e)
	}
	record := func(label string) map[string]any {
		return map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-fix": map[string]any{"options": map[string]any{"tasks": []any{map[string]any{"instructions": label}}, "finalizers": []any{}, "completion_instructions": label}}}}
	}
	paths := []string{configFile(t, "", record("user\nexact  ")), configFile(t, "github.com/upstream/repo", record("upstream")), configFile(t, id.Repository, record("repository"))}
	originals := map[string]string{}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		originals[path] = string(b)
	}
	req.Upstream = "github.com/upstream/repo"
	req.Options = nil
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	if p.Selection.Config.Options["completion_instructions"] != "repository" || len(p.Selection.Tasks) != 1 || p.Selection.Tasks[0][0].Invocation.Instructions != "repository" {
		t.Fatal("repository precedence", p.Selection)
	}
	exact := "  invocation\nkeep trailing spaces  \n"
	selection, e := resolveSelection(context.Background(), root, id, req.Upstream, map[string]any{"tasks": []any{}, "completion_instructions": exact}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(selection.Tasks) != 0 || selection.Config.Options["completion_instructions"] != exact {
		t.Fatal("invocation replacement or exact text lost", selection)
	}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil || string(b) != originals[path] {
			t.Fatal("read rewrote original settings", path, e)
		}
	}
	if e = os.Remove(paths[2]); e != nil {
		t.Fatal(e)
	}
	selection, e = resolveSelection(context.Background(), root, id, req.Upstream, nil, nil)
	if e != nil || selection.Config.Options["completion_instructions"] != "upstream" {
		t.Fatal("upstream precedence", e, selection.Config.Options)
	}
	if e = os.Remove(paths[1]); e != nil {
		t.Fatal(e)
	}
	selection, e = resolveSelection(context.Background(), root, id, "", nil, nil)
	if e != nil || selection.Config.Options["completion_instructions"] != "user\nexact  " {
		t.Fatal("user precedence", e, selection.Config.Options)
	}
}
func TestLegacyLimitsHooksAndNumericPersonalAccounts(t *testing.T) {
	root, provider, req := fixture(t)
	configFile(t, "", map[string]any{"accounts": map[string]any{"personal": []any{map[string]any{"host": "github.com", "id": 42}}}, "processes": map[string]any{"pr-fix": map[string]any{"limits": map[string]any{"review_cycles": 2}, "finalizers": []any{}, "hooks": map[string]any{"before_cycle": []any{map[string]any{"instructions": "Record hook evidence."}}}}}})
	provider.ev.Metadata.User.Login = "other-person"
	provider.ev.Metadata.User.ID = 42
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	if p.Selection.Limits().ReviewCycles != 2 || len(p.Selection.Accounts) != 1 || len(p.Groups["before_cycle"]) != 1 {
		t.Fatal(p.Selection, p.Groups)
	}
}
func TestDefaultOrderAndNestedReviewerAdapters(t *testing.T) {
	root, provider, req := fixture(t)
	reviewer := func(id string) Adapter {
		return Adapter{Identity: id, Version: "v1", Kind: "review", Cacheable: true, Procedure: [][]Pass{{{ID: "correctness", Role: "leaf", Assignment: "Review the specified diff only.", Evidence: []string{"original"}}}}}
	}
	req.Options = map[string]any{}
	configFile(t, "", map[string]any{"skill_defaults": map[string]any{"dk-workflows:dk-pr-reviewers-run": map[string]any{"options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:outer", "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:inner"}}}}}}}}})
	req.Adapters = map[string]Adapter{"custom:outer": reviewer("custom:outer"), "custom:inner": reviewer("custom:inner")}
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, e := ReadPlan(prepared.Plan)
	if e != nil {
		t.Fatal(e)
	}
	kinds := []string{}
	for _, g := range p.Groups["tasks"] {
		for _, id := range g {
			o, _ := p.Occurrence(id)
			kinds = append(kinds, o.Kind)
		}
	}
	want := []string{"inspection", "decisions", "fix", "publication", "scope", "description", "review", "review", "fix", "publication"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatal("default task order changed", kinds)
	}
	kinds = nil
	for _, g := range p.Groups["finalizers"] {
		for _, id := range g {
			o, _ := p.Occurrence(id)
			kinds = append(kinds, o.Kind)
		}
	}
	if !reflect.DeepEqual(kinds, []string{"cleanup", "decisions", "description", "reply", "ci", "publication"}) {
		t.Fatal("default finalizers changed", kinds)
	}
	if len(prepared.Warnings) == 0 {
		t.Fatal("potential engine capacity overrun was not reported")
	}
	req.Adapters["custom:inner"] = reviewer("custom:inner")
	req.Options = map[string]any{"tasks": []any{map[string]any{"skill": "custom:outer", "options": map[string]any{"reviewers": []any{map[string]any{"skill": "custom:outer"}}}}}, "finalizers": []any{}}
	_, e = Prepare(context.Background(), root, provider, req)
	if e == nil || !strings.Contains(strings.ToLower(e.Error()), "recurs") {
		t.Fatal("recursive adapter dispatched", e)
	}
}
func TestResourcesPinDirectoryContentsAndRejectCycles(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "entry"), []byte("one"), 0600); e != nil {
		t.Fatal(e)
	}
	first, e := resourceDigest(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "entry"), []byte("two"), 0600); e != nil {
		t.Fatal(e)
	}
	next, e := resourceDigest(root)
	if e != nil || first == next {
		t.Fatal("resource content omitted", e)
	}
	if e = os.Symlink(root, filepath.Join(root, "loop")); e != nil {
		t.Fatal(e)
	}
	if _, e = resourceDigest(root); e == nil {
		t.Fatal("cyclic resource accepted")
	}
}

func TestAdapterIdentityRoleAndOwnershipBlockBeforeDispatch(t *testing.T) {
	root, provider, req := fixture(t)
	req.Options["tasks"] = []any{map[string]any{"skill": "custom:review"}}
	adapter := Adapter{Identity: "custom:review", Version: "v1", Kind: "review", Procedure: [][]Pass{{{ID: "read", Role: "missing", Assignment: "Review only.", Evidence: []string{"original"}}}}}
	req.Adapters = map[string]Adapter{"custom:review": adapter}
	if _, e := Prepare(context.Background(), root, provider, req); e == nil || !strings.Contains(e.Error(), "unavailable") {
		t.Fatal("unknown native role admitted", e)
	}
	adapter.Procedure[0][0].Role = "leaf"
	adapter.Identity = "custom:other"
	req.Adapters["custom:review"] = adapter
	if _, e := Prepare(context.Background(), root, provider, req); e == nil || !strings.Contains(e.Error(), "declared identity") {
		t.Fatal("mismatched adapter identity admitted", e)
	}
	adapter.Identity = "custom:review"
	adapter.Writes = []string{"../other"}
	req.Adapters["custom:review"] = adapter
	if _, e := Prepare(context.Background(), root, provider, req); e == nil || !strings.Contains(e.Error(), "ownership") {
		t.Fatal("unsupported ownership admitted", e)
	}
	if _, e := os.Stat(filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")); !os.IsNotExist(e) {
		t.Fatal("invalid selection consumed logical state", e)
	}
}

func TestUntrackedBinaryContentChangesInvalidatePins(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	path := filepath.Join(root, "binary.bin")
	if e = os.WriteFile(path, []byte{0x80}, 0600); e != nil {
		t.Fatal(e)
	}
	first, e := capture(context.Background(), root, provider, p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte{0x81}, 0600); e != nil {
		t.Fatal(e)
	}
	next, e := capture(context.Background(), root, provider, p)
	if e != nil {
		t.Fatal(e)
	}
	if first.Fingerprint(false, false) == next.Fingerprint(false, false) {
		t.Fatal("distinct non-UTF-8 file contents shared a pin")
	}
	if validText(string([]byte{0xff})) {
		t.Fatal("an invalid instruction could be silently normalized")
	}
}

func TestLiveObservationReuseIsLimitedToOneAction(t *testing.T) {
	root, provider, req := fixture(t)
	prepared, e := Prepare(context.Background(), root, provider, req)
	if e != nil {
		t.Fatal(e)
	}
	p, _ := ReadPlan(prepared.Plan)
	r := &Runtime{Root: root, Provider: provider}
	ctx := context.WithValue(context.Background(), liveObservationKey{}, &liveObservation{})
	first, e := r.live(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	provider.ev.Comments = append(provider.ev.Comments, map[string]any{"id": 11, "body": "A new incoming instruction"})
	same, e := r.live(ctx, p)
	if e != nil || same.Discussion != first.Discussion {
		t.Fatal("one action mixed independent observations", e)
	}
	next, e := r.live(context.WithValue(context.Background(), liveObservationKey{}, &liveObservation{}), p)
	if e != nil || next.Discussion == first.Discussion {
		t.Fatal("evidence was cached across action boundaries", e)
	}
}
