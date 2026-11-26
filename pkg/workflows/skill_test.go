package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const skillWorkflow = `on: {workflow_call: {inputs: {selected: {type: skill, required: true}}}}
jobs:
  work:
    steps:
      - id: selected
        uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: Work.
          skill: {input: selected, arguments: exact, resources: [reference.txt], evidence: {invocation: required}}
`

func suppliedSkillFixture(t *testing.T) (string, map[string]any) {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{"SKILL.md": "---\nname: probe\ndescription: Fixture skill.\n---\nEXACT_ADMITTED_SKILL\n", "reference.txt": "EXACT_RESOURCE\n", "unselected.txt": "UNSELECTED_RESOURCE\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	return root, map[string]any{"selected": map[string]any{"directory": root}}
}

type suppliedSession struct {
	*nativeTaskFixture
	reportOnly   bool
	reject       bool
	continuation bool
}

func (f *suppliedSession) AdmitSkillTask(plan *SkillInvocation) (*SkillInvocation, error) {
	if f.reject {
		return nil, fmt.Errorf("unsupported required capability")
	}
	copy := *plan
	copy.Profile = "fixture/skill-v1"
	copy.Model = "fixture-model"
	copy.ContinuationRequiresEvidence = f.continuation
	return &copy, nil
}
func (f *suppliedSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	session := req.SessionID
	if session == "" {
		session = "session-" + req.ParentWorkerID
	}
	if req.Skill != nil && !f.reportOnly {
		for _, kind := range []string{"invocation", "result"} {
			observation := SkillObservation{Key: kind + ":original-call", Kind: kind, SessionID: session, CallID: "original-call", Name: req.Skill.Binding.Name, Arguments: req.Skill.Arguments, Raw: json.RawMessage(`{"provider_original":"exact output"}`)}
			raw, _ := json.Marshal(observation)
			var data map[string]any
			_ = json.Unmarshal(raw, &data)
			emit(ProviderEvent{Type: "skill_evidence", SessionID: session, Data: data})
		}
	}
	return f.nativeTaskFixture.RunTurn(ctx, req, emit)
}
func skillExecutorFixture(t *testing.T, source string, reportOnly bool) (*Executor, *SQLiteStore, *suppliedSession) {
	t.Helper()
	e, store, base := agentTaskFixture(t, source, func(req SessionRequest) (map[string]any, error) {
		if req.Skill != nil {
			if req.SkillRoot == "" {
				return nil, fmt.Errorf("missing verified assets")
			}
			data, err := os.ReadFile(filepath.Join(req.SkillRoot, filepath.FromSlash(req.Skill.Binding.Entry)))
			if err != nil || !strings.Contains(string(data), "EXACT_ADMITTED_SKILL") {
				return nil, fmt.Errorf("retained skill differs: %v", err)
			}
			if _, err := os.Stat(filepath.Join(req.SkillRoot, "skills/probe/unselected.txt")); !os.IsNotExist(err) {
				return nil, fmt.Errorf("unselected resource exposed")
			}
		}
		return map[string]any{"text": "report says complete"}, nil
	})
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(store.Dir, "assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	f := &suppliedSession{nativeTaskFixture: base, reportOnly: reportOnly}
	e.Agents = f
	e.Workers.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return f, nil, nil, nil
	}
	return e, store, f
}

func TestSkillCaptureRejectsMetadataAndSymlinks(t *testing.T) {
	root, _ := suppliedSkillFixture(t)
	captured, err := CaptureSkillDirectory(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	var empty bool
	for _, entry := range captured.Entries {
		if entry.Path == "skills/probe/empty" {
			empty = entry.Mode.IsDir() && entry.Mode.Perm() == 0750
		}
	}
	if !empty {
		t.Fatal("empty directory/source mode lost")
	}
	original := captured.SourceDigest
	if err = os.WriteFile(filepath.Join(root, "reference.txt"), []byte("changed"), 0640); err != nil {
		t.Fatal(err)
	}
	changed, err := CaptureSkillDirectory(t.Context(), root)
	if err != nil || original == changed.SourceDigest {
		t.Fatal("changed bytes reused digest", err)
	}
	if err = os.Symlink("reference.txt", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err = CaptureSkillDirectory(t.Context(), root); err == nil {
		t.Fatal("source symlink accepted")
	}
	for _, metadata := range []string{"name: probe\nname: again\ndescription: fixture", "name: probe\ndescription: fixture\nhooks: {}", "name: probe\ndescription: fixture\ncontext: fork"} {
		if _, err = ParseSkillMetadata([]byte("---\n" + metadata + "\n---\nbody")); err == nil {
			t.Fatal("authority-changing metadata accepted", metadata)
		}
	}
}

func TestSkillUnsupportedControlsRejectBeforeAdmission(t *testing.T) {
	for _, variant := range []string{"children", "late-options", "missing-input", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			_, inputs := suppliedSkillFixture(t)
			source := skillWorkflow
			if variant == "children" {
				source = strings.Replace(source, "arguments: exact", "children: [{name: child}], arguments: exact", 1)
			}
			if variant == "late-options" {
				source = strings.Replace(source, "arguments: exact", "arguments: '${{ steps.foreign.outputs.value }}'", 1)
			}
			if variant == "missing-input" {
				source = strings.Replace(source, "arguments: exact", "arguments: '${{ inputs.unknown }}'", 1)
			}
			e, store, f := skillExecutorFixture(t, source, false)
			f.reject = variant == "runtime"
			run, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
			if err == nil || run != nil {
				t.Fatal("unsupported contract admitted", run, err)
			}
			var count int
			if err = store.db.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil || count != 0 {
				t.Fatal("rejection retained a run", count, err)
			}
			if len(f.requests) != 0 {
				t.Fatal("unsupported contract reached provider")
			}
		})
	}
}

func TestSkillRetainedCapabilityRejectsBeforeResumeClaim(t *testing.T) {
	_, inputs := suppliedSkillFixture(t)
	e, store, f := skillExecutorFixture(t, skillWorkflow, false)
	run, err := e.Prepare(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err != nil {
		t.Fatal(err)
	}
	f.reject = true
	if _, err = e.Resume(t.Context(), run.ID, nil, false); err == nil {
		t.Fatal("unsupported retained capability resumed")
	}
	retained, _, err := store.Load(t.Context(), run.ID)
	if err != nil || retained.Status != RunStatusQueued || retained.Generation != run.Generation || len(f.requests) != 0 {
		t.Fatal("unsupported resume claimed work", retained, err)
	}
}

func TestSkillObservedEvidenceAndSiblingProjection(t *testing.T) {
	for _, reportOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(reportOnly), func(t *testing.T) {
			_, inputs := suppliedSkillFixture(t)
			source := skillWorkflow + "      - id: sibling\n        uses: agent/run\n        with: {agent: reviewer.yaml, prompt: Independent.}\n"
			e, store, f := skillExecutorFixture(t, source, reportOnly)
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
			if reportOnly {
				if err == nil {
					t.Fatal("report-only success accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			workers, err := e.Workers.List(t.Context(), run.ID)
			if err != nil || len(workers) != 2 {
				t.Fatal(workers, err)
			}
			selected := 0
			for _, worker := range workers {
				if worker.Skill != nil {
					selected++
					if err = worker.SkillEvidence.Complete(worker.Skill); err != nil {
						t.Fatal(err)
					}
				} else if worker.SkillEvidence != nil {
					t.Fatal("sibling inherited evidence")
				}
			}
			if selected != 1 || len(f.requests) != 2 || f.requests[1].Skill != nil || f.requests[1].SkillRoot != "" {
				t.Fatal("skill escaped selected occurrence", f.requests)
			}
			_, snap, err := store.Load(t.Context(), run.ID)
			if err != nil || snap.SkillInputs["selected"] == "" {
				t.Fatal("missing admitted digest", err)
			}
		})
	}
}

func TestSkillLostCompletionRestoresOriginalEvidenceAndBytes(t *testing.T) {
	root, inputs := suppliedSkillFixture(t)
	e, store, f := skillExecutorFixture(t, skillWorkflow, false)
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(store.Dir, "assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	if _, err := store.db.Exec(`CREATE TRIGGER lose_skill_completion BEFORE UPDATE OF status ON attempts WHEN NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'lost skill completion'); END`); err != nil {
		t.Fatal(err)
	}
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil {
		t.Fatal("fault not exercised", run, err)
	}
	if err = os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec("DROP TRIGGER lose_skill_completion"); err != nil {
		t.Fatal(err)
	}
	resumed, err := e.Resume(t.Context(), run.ID, nil, true)
	if err != nil || resumed.Status != RunStatusSucceeded {
		t.Fatal(resumed, err)
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil || len(workers) != 1 || workers[0].Remaining != 31 || len(f.requests) != 1 {
		t.Fatal("recovery replayed/reset original task", workers, len(f.requests), err)
	}
	if err = workers[0].SkillEvidence.Complete(workers[0].Skill); err != nil {
		t.Fatal(err)
	}
}

func TestSkillEvidenceRejectsConflictsAndUnmatchedResults(t *testing.T) {
	plan := &SkillInvocation{ID: "plan", Binding: SkillBinding{Name: "probe"}, Arguments: "exact"}
	ledger := &SkillEvidence{}
	result := SkillObservation{Key: "result:one", Kind: "result", SessionID: "session", CallID: "one", Name: "probe", Arguments: "exact", Raw: json.RawMessage(`{"original":true}`)}
	if err := ledger.Observe(plan, result); err != nil {
		t.Fatal(err)
	}
	if ledger.Complete(plan) == nil {
		t.Fatal("unmatched result completed")
	}
	changed := result
	changed.Raw = json.RawMessage(`{"original":false}`)
	if ledger.Observe(plan, changed) == nil {
		t.Fatal("conflicting original result accepted")
	}
	changed = result
	changed.Key = "result:other"
	changed.SessionID = "foreign"
	if ledger.Observe(plan, changed) == nil {
		t.Fatal("foreign session evidence accepted")
	}
}

func TestSkillCapturedBytesCannotKeepStaleInputIdentity(t *testing.T) {
	root, _ := suppliedSkillFixture(t)
	source, err := CaptureSkillDirectory(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]*SkillSource{"selected": source}
	original := SkillInputIdentities(sources)["selected"]
	for i := range source.Entries {
		if source.Entries[i].Path == source.Entry {
			source.Entries[i].Data = append(source.Entries[i].Data, []byte("changed resolver bytes")...)
		}
	}
	if original == SkillInputIdentities(sources)["selected"] {
		t.Fatal("mutated captured bytes retained idempotency identity")
	}
	if validateSkillSource(source) == nil {
		t.Fatal("stale captured content identity accepted")
	}
}

func TestSkillMissingOriginalReceiptRejectsRecoveryBeforeClaim(t *testing.T) {
	_, inputs := suppliedSkillFixture(t)
	e, store, f := skillExecutorFixture(t, skillWorkflow, true)
	f.continuation = true
	run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/main.yml", Inputs: inputs})
	if err == nil || run == nil {
		t.Fatal("missing receipt did not leave incomplete effect", run, err)
	}
	before, _, err := store.Load(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	workers, err := e.Workers.List(t.Context(), run.ID)
	if err != nil || len(workers) != 1 || workers[0].SessionID == "" {
		t.Fatal(workers, err)
	}
	remaining := workers[0].Remaining
	f.reportOnly = false
	if _, err = e.Resume(t.Context(), run.ID, nil, true); err == nil || !strings.Contains(err.Error(), "original observed invocation") {
		t.Fatal("missing original receipt recovered", err)
	}
	after, _, err := store.Load(t.Context(), run.ID)
	if err != nil || after.Generation != before.Generation || after.Status != before.Status || len(f.requests) != 1 {
		t.Fatal("uncertain original invocation was reclaimed/replayed", after, len(f.requests), err)
	}
	workers, err = e.Workers.List(t.Context(), run.ID)
	if err != nil || workers[0].Remaining != remaining {
		t.Fatal("continuation reset original budget", workers, err)
	}
}

func TestSkillContinuationDoesNotBorrowAnotherInvocationReceipt(t *testing.T) {
	plan := &SkillInvocation{ID: "original", Binding: SkillBinding{Name: "probe"}, ContinuationRequiresEvidence: true}
	ledger := &SkillEvidence{}
	for _, kind := range []string{"invocation", "result"} {
		if err := ledger.Observe(plan, SkillObservation{Key: kind + ":call", Kind: kind, SessionID: "session", CallID: "call", Name: "probe", Raw: json.RawMessage(`{"original":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Continuation(plan, "session"); err != nil {
		t.Fatal(err)
	}
	copy := *plan
	copy.ID = "new-occurrence"
	if ledger.Continuation(&copy, "session") == nil || ledger.Continuation(plan, "foreign-session") == nil {
		t.Fatal("foreign invocation borrowed original receipt")
	}
	ledger.Observations[1].Arguments = "changed"
	if ledger.Continuation(plan, "session") == nil {
		t.Fatal("tampered original receipt accepted")
	}
	worker := Worker{Skill: plan, TaskTurns: 32, Remaining: 31}
	if worker.validateSkillContinuation() == nil {
		t.Fatal("lost session acknowledgement permitted replacement invocation")
	}
}
