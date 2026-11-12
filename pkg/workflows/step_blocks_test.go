package workflows

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const blockWorkflow = `jobs:
  review:
    outputs: {text: '${{ steps.combine.outputs.text }}'}
    steps:
      - id: seed
        uses: agent/run
        with:
          agent: a.toml
          prompt: seed
      - id: reviews
        parallel:
          - id: left
            uses: agent/run
            with:
              agent: a.toml
              prompt: '${{ steps.seed.outputs.text }} left'
          - id: right_branch
            sequence:
              - id: right
                uses: agent/run
                with:
                  agent: a.toml
                  prompt: '${{ steps.seed.outputs.text }} right'
              - id: refine
                uses: agent/run
                with:
                  agent: a.toml
                  prompt: '${{ steps.right.outputs.text }} refined'
      - id: combine
        uses: agent/run
        with:
          agent: a.toml
          prompt: '${{ steps.left.outputs.text }} + ${{ steps.refine.outputs.text }}'
`

func TestStepBlocksAdmissionAndScopes(t *testing.T) {
	for _, test := range []struct{ name, old, replacement, diagnostic string }{
		{"valid", "", "", ""},
		{"duplicate across branches", "id: right\n", "id: left\n", "duplicate"},
		{"sibling legacy result", "steps.seed.outputs.text }} right", "steps.left.outputs.text }} right", "unavailable"},
		{"sibling concise result", "steps.seed.outputs.text }} right", "steps.left.text }} right", "unavailable"},
		{"ancestor block unavailable", "steps.seed.outputs.text }} right", "steps.reviews.status }} right", "unavailable"},
		{"forward in sequence", "steps.seed.outputs.text }} right", "steps.refine.text }} right", "unavailable"},
		{"mixed fields", "parallel:\n", "uses: agent/a\n        parallel:\n", "cannot combine"},
		{"block continuation", "parallel:\n", "continue-on-error: false\n        parallel:\n", "cannot combine"},
		{"invalid nested command", "uses: agent/run\n                with:\n                  agent: a.toml\n                  prompt: '${{ steps.right.outputs.text }} refined'", "run: echo hi", "explicit shell"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := blockWorkflow
			if test.old != "" {
				source = strings.Replace(source, test.old, test.replacement, 1)
			}
			w, err := Parse([]byte(source))
			if err == nil {
				err = Validate(w)
			}
			if test.diagnostic == "" && err != nil || test.diagnostic != "" && (err == nil || !strings.Contains(err.Error(), test.diagnostic)) {
				t.Fatalf("got %v, want %q", err, test.diagnostic)
			}
		})
	}
	for _, block := range []string{"parallel: []", "parallel: null", "sequence: []", "parallel: [{sequence: []}]", "parallel: [{agent: a, prompt: x}]\n        sequence: [{agent: a, prompt: x}]"} {
		_, err := Parse([]byte("jobs:\n  x:\n    steps:\n      - " + block + "\n"))
		if err == nil {
			t.Errorf("accepted invalid block %s", block)
		}
	}
	steps := []Step{{ID: "outer", Parallel: []Step{{Sequence: []Step{{Uses: "agent/run", With: map[string]any{"agent": "a.toml"}}}}}}, {Uses: "agent/run", With: map[string]any{"agent": "a.toml"}}}
	got := []string{}
	for _, step := range FlattenSteps(steps) {
		got = append(got, step.ID)
	}
	want := []string{"outer", "step_1_parallel_1", "step_1_parallel_1_sequence_1", "step_2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs: %v", got)
	}
	steps[0].Parallel[0].Sequence[0].ID = "step_2"
	steps[0].Parallel[0].Sequence[0].With = map[string]any{"agent": "a.toml", "prompt": "x"}
	steps[1].With = map[string]any{"agent": "a.toml", "prompt": "x"}
	w := &Workflow{Jobs: map[string]Job{"x": {Steps: steps}}}
	if err := Validate(w); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("nested implicit ID collision accepted: %v", err)
	}
}

func TestStepBlocksParallelJoinAndCapacity(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			writeWorkflow(t, root, "main.yml", blockWorkflow)
			started := make(chan string, 2)
			release := make(chan struct{})
			var active, maxActive atomic.Int32
			runner := agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
				}
				if capacity == 2 && (req.Prompt == "seed left" || req.Prompt == "seed right") {
					started <- req.Prompt
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return map[string]any{"text": req.Prompt}, nil
			})
			e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, Agents: runner, Slots: make(chan struct{}, capacity)})
			type completed struct {
				run *Run
				err error
			}
			done := make(chan completed, 1)
			go func() {
				run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
				done <- completed{run, err}
			}()
			if capacity == 2 {
				for range 2 {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("parallel leaves did not overlap")
					}
				}
				close(release)
			}
			var got completed
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("nested execution held a parent capacity slot")
			}
			if got.err != nil || got.run.Status != RunStatusSucceeded {
				t.Fatalf("run: %+v %v", got.run, got.err)
			}
			if got.run.Jobs["review"].Outputs["text"] != "seed left + seed right refined" {
				t.Fatal(got.run.Jobs["review"])
			}
			if maxActive.Load() != int32(capacity) || len(e.Slots) != 0 {
				t.Fatalf("capacity: peak=%d slots=%d", maxActive.Load(), len(e.Slots))
			}
			branches := got.run.Jobs["review"].Steps["reviews"].Outputs["branches"].([]any)
			if branches[0].(map[string]any)["id"] != "left" || branches[1].(map[string]any)["id"] != "right_branch" {
				t.Fatalf("nondeterministic branch order: %v", branches)
			}
		})
	}
}

func TestStepBlocksFailureCancelsAndJoinsSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", `jobs:
  x:
    steps:
      - parallel:
          - id: fail
            uses: agent/run
            with:
              agent: a.toml
              prompt: fail
          - id: sibling
            sequence:
              - uses: agent/run
                with:
                  agent: a.toml
                  prompt: sibling
              - uses: agent/run
                with:
                  agent: a.toml
                  prompt: forbidden
      - uses: agent/run
        with:
          agent: a.toml
          prompt: forbidden
`)
	started := make(chan struct{})
	var stopped, forbidden atomic.Bool
	e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, MaxParallel: 2, Agents: agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		switch req.Prompt {
		case "fail":
			select {
			case <-started:
				return nil, errors.New("branch failed")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "sibling":
			close(started)
			<-ctx.Done()
			stopped.Store(true)
			return map[string]any{"text": "partial"}, ctx.Err()
		default:
			forbidden.Store(true)
			return nil, nil
		}
	})})
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "branch failed") || !stopped.Load() || forbidden.Load() {
		t.Fatalf("join: %+v %v stopped=%v forbidden=%v", run, err, stopped.Load(), forbidden.Load())
	}
	if run.Status != RunStatusFailed || run.Jobs["x"].Steps["step_1_parallel_2_sequence_1"].Outputs["text"] != "partial" {
		t.Fatalf("sibling evidence lost: %+v", run)
	}
}

type blockCompletionFault struct {
	*SQLiteStore
	failID string
	failed atomic.Bool
}

func (s *blockCompletionFault) Complete(ctx context.Context, a Attempt, result StepExecution) error {
	if a.StepID == s.failID && s.failed.CompareAndSwap(false, true) {
		return errors.New("lost checkpoint write")
	}
	return s.SQLiteStore.Complete(ctx, a, result)
}

func TestStepBlocksRestartReusesChildrenAndGuardsUncertainEffects(t *testing.T) {
	for _, lost := range []string{"reviews", "refine"} {
		t.Run(lost, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			calls := map[string]int{}
			e, store := durableExecutor(t, blockWorkflow, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
				mu.Lock()
				calls[req.Prompt]++
				mu.Unlock()
				return map[string]any{"text": req.Prompt}, nil
			}))
			e.Store = &blockCompletionFault{SQLiteStore: store, failID: lost}
			run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run.Status == RunStatusSucceeded {
				t.Fatalf("fault ignored: %+v %v", run, err)
			}
			e.Store = store
			if lost == "refine" {
				if _, err := nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, false); err == nil {
					t.Fatal("uncertain child replayed without acknowledgement")
				}
			}
			run, err = nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, lost == "refine")
			if err != nil || run.Status != RunStatusSucceeded {
				t.Fatalf("resume: %+v %v", run, err)
			}
			for prompt, count := range calls {
				want := 1
				if lost == "refine" && prompt == "seed right refined" {
					want = 1
				}
				if count != want {
					t.Fatalf("effect %q ran %d times, want %d", prompt, count, want)
				}
			}
			if calls["seed left + seed right refined"] != 1 {
				t.Fatalf("missing sequential continuation: %v", calls)
			}
			attempts, err := store.Attempts(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			joins := 0
			for _, attempt := range attempts {
				if attempt.Phase == "orchestration" && attempt.Status == RunStatusSucceeded {
					joins++
				}
			}
			if joins < 2 {
				t.Fatalf("missing durable joins: %+v", attempts)
			}
		})
	}
}

func TestStepBlocksInputWaitJoinsAndResumesCompletedSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	e, store := durableExecutor(t, `jobs:
  x:
    steps:
      - parallel:
          - id: question
            uses: input/wait
            with: {prompt: Choose, evidence-id: policy}
          - id: sibling
            uses: agent/run
            with:
              agent: a.toml
              prompt: sibling
      - id: after
        uses: agent/run
        with:
          agent: a.toml
          prompt: '${{ steps.question.outputs.answer }} ${{ steps.sibling.outputs.text }}'
`, agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"text": req.Prompt}, nil
	}))
	e.Slots = make(chan struct{}, 1)
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusWaiting || calls.Load() != 1 || run.Jobs["x"].Steps["sibling"].Status != RunStatusSucceeded {
		t.Fatalf("wait lost sibling: %+v %v", run, err)
	}
	answerGate(t, store, inputGate(t, store, run.ID))
	run, err = nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, false)
	if err != nil || run.Status != RunStatusSucceeded || calls.Load() != 2 || run.Jobs["x"].Steps["after"].Outputs["text"] != "approved sibling" {
		t.Fatalf("resume: %+v %v calls=%d", run, err, calls.Load())
	}
}

func TestStepBlocksSnapshotDetachesNestedState(t *testing.T) {
	ec := expressionContext{Inputs: map[string]any{"nested": map[string]any{"value": "initial"}}, Steps: map[string]StepExecution{"before": {Outputs: map[string]any{"nested": map[string]any{"value": "initial"}}}}}
	a, err := branchContext(ec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := branchContext(ec)
	if err != nil {
		t.Fatal(err)
	}
	a.Inputs["nested"].(map[string]any)["value"] = "changed"
	a.Steps["before"].Outputs["nested"].(map[string]any)["value"] = "changed"
	a.Steps["new"] = StepExecution{}
	if b.Inputs["nested"].(map[string]any)["value"] != "initial" || b.Steps["before"].Outputs["nested"].(map[string]any)["value"] != "initial" || len(b.Steps) != 1 {
		t.Fatal("parallel branches share mutable state")
	}
}

func TestStepBlocksRejectUnserializableSnapshotBeforeStartingBranches(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			writeWorkflow(t, root, "main.yml", `on:
  workflow_call:
    inputs:
      unused: {type: number, default: `+value+`}
      task: {type: string, default: Review}
jobs:
  review:
    steps:
      - id: branches
        parallel:
          - uses: agent/run
            with: {agent: a.toml, prompt: '${{ inputs.task }}'}
          - uses: agent/run
            with: {agent: a.toml, prompt: independent}
`)
			var calls atomic.Int32
			e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, Agents: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{"text": req.Prompt}, nil
			})})
			run, err := nativeTestExecutor(t, e).Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil || !strings.Contains(err.Error(), "unsupported value") || calls.Load() != 0 {
				t.Fatalf("snapshot failure did not stop dispatch: run=%+v err=%v calls=%d", run, err, calls.Load())
			}
			if run != nil && run.Status == RunStatusSucceeded {
				t.Fatalf("snapshot failure was not recorded: %+v", run)
			}
		})
	}
}

func TestStepBlocksNormalizationErrorIdentifiesStep(t *testing.T) {
	for _, test := range []struct{ name, steps, id string }{
		{"top-level explicit", "[{id: bad, uses: agent/removed}]\n", "bad"},
		{"top-level anonymous", "[{uses: agent/run, with: {agent: a.toml, prompt: ok}}, {uses: agent/removed}]\n", "step_2"},
		{"nested explicit", "[{parallel: [{sequence: [{id: bad, uses: agent/removed}]}]}]\n", "bad"},
		{"nested anonymous", "[{parallel: [{sequence: [{uses: agent/removed}]}]}]\n", "step_1_parallel_1_sequence_1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte("jobs: {review: {steps: " + test.steps + "}}"))
			want := "job review step " + test.id + ": only agent/run is supported; supply a native definition in with.agent"
			if err == nil || err.Error() != want {
				t.Fatalf("diagnostic = %v, want %q", err, want)
			}
		})
	}
}

func TestStepBlocksPersistenceFailureCannotContinue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	e, store := durableExecutor(t, `jobs:
  x:
    steps:
      - parallel:
          - id: fail
            continue-on-error: true
            uses: agent/run
            with:
              agent: a.toml
              prompt: fail
      - uses: agent/run
        with:
          agent: a.toml
          prompt: forbidden
`, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		calls.Add(1)
		return nil, ErrProviderFailure
	}))
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "fail"}
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || calls.Load() != 1 || run.FailureClass != "storage" {
		t.Fatalf("persistence error swallowed: %+v %v calls=%d", run, err, calls.Load())
	}
}

func TestStepBlocksRecursiveAdmissionAndNamedSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const source = `schemas:
  Decision:
    type: object
    properties: {approved: {type: boolean}}
    required: [approved]
jobs:
  x:
    outputs: {approved: '${{ steps.result.approved }}'}
    steps:
      - parallel:
          - sequence:
              - id: result
                with: {output: {schema: Decision, repair-attempts: 0}, agent: nested.toml, prompt: Decide}
                uses: agent/run
`
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", source)
	var checked, called atomic.Bool
	runner := retainedRolesRunner{AgentRunner: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		called.Store(true)
		if req.AgentID != "nested" || req.Output == nil || req.Output.Schema["type"] != "object" {
			return nil, errors.New("nested schema or role was not admitted")
		}
		return map[string]any{"text": `{"approved":true}`}, nil
	}), check: func(ids []string) error {
		checked.Store(true)
		if !reflect.DeepEqual(ids, []string{"codex"}) {
			return fmt.Errorf("nested roles omitted from preflight: %v", ids)
		}
		return nil
	}}
	e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, Agents: runner})
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || !checked.Load() || !called.Load() || run.Jobs["x"].Outputs["approved"] != true {
		t.Fatalf("nested admission: %+v %v", run, err)
	}
	// The same recursive traversal rejects missing application registrations
	// before starting any unrelated provider work.
	writeWorkflow(t, root, "main.yml", `jobs:
  x:
    steps:
      - parallel:
          - uses: agent/run
            with:
              agent: nested.toml
              prompt: forbidden
          - sequence:
              - uses: action/missing
                with: {}
`)
	called.Store(false)
	_, err = nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "missing registered Go action") || called.Load() {
		t.Fatalf("nested action bypassed admission: %v called=%v", err, called.Load())
	}
}

func TestStepBlocksConditionalAndContinuedFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", `jobs:
  x:
    steps:
      - id: skip
        if: 'false'
        parallel:
          - id: forbidden
            uses: agent/run
            with:
              agent: a.toml
              prompt: forbidden
      - id: branches
        parallel:
          - id: continued
            continue-on-error: true
            uses: agent/run
            with:
              agent: a.toml
              prompt: fail
          - id: success
            sequence:
              - uses: agent/run
                with:
                  agent: a.toml
                  prompt: success
      - id: after
        uses: agent/run
        with:
          agent: a.toml
          prompt: after
`)
	var calls atomic.Int32
	e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, Agents: agentFunc(func(_ context.Context, req AgentRequest) (map[string]any, error) {
		calls.Add(1)
		if req.Prompt == "forbidden" {
			return nil, errors.New("skipped block admitted a child")
		}
		if req.Prompt == "fail" {
			return nil, errors.New("handled failure")
		}
		return map[string]any{"text": req.Prompt}, nil
	})})
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusSucceeded || calls.Load() != 3 {
		t.Fatalf("continued execution: %+v %v", run, err)
	}
	steps := run.Jobs["x"].Steps
	if _, exists := steps["forbidden"]; exists {
		t.Fatal("skipped child has a result")
	}
	if steps["skip"].Status != RunStatusSkipped || steps["continued"].Status != RunStatusFailed || steps["branches"].Status != RunStatusSucceeded || steps["after"].Status != RunStatusSucceeded {
		t.Fatalf("unexpected statuses: %+v", steps)
	}
}

func TestStepBlocksCallerCancellationJoinsEveryBranch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deadline, stopDeadline := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopDeadline()
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", `jobs: {x: {steps: [{parallel: [{uses: agent/run, with: {agent: a.toml, prompt: left}}, {uses: agent/run, with: {agent: a.toml, prompt: right}}]}]}}
`)
	started := make(chan struct{}, 2)
	var stopped atomic.Int32
	e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, MaxParallel: 2, Agents: agentFunc(func(ctx context.Context, _ AgentRequest) (map[string]any, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			stopped.Add(1)
			return nil, ctx.Err()
		case <-deadline.Done():
			return nil, deadline.Err()
		}
	})})
	type completed struct {
		run *Run
		err error
	}
	done := make(chan completed, 1)
	go func() {
		run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
		done <- completed{run, err}
	}()
	for range 2 {
		select {
		case <-started:
		case <-deadline.Done():
			t.Fatal("branches did not start")
		}
	}
	cancel()
	select {
	case got := <-done:
		if got.err == nil || got.run.Status != RunStatusCanceled || stopped.Load() != 2 {
			t.Fatalf("cancellation lost branches: %+v %v stopped=%d", got.run, got.err, stopped.Load())
		}
	case <-deadline.Done():
		t.Fatal("caller cancellation failed to join branches")
	}
}
