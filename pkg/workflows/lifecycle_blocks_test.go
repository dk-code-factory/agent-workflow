package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const lifecycleBlockWorkflow = `schemas:
  Result:
    type: object
    required: [value]
    properties:
      value: {type: string, enum: ['${{ steps.seed.text }}']}
jobs:
  x:
    steps:
      - id: joined
        parallel:
          - sequence:
              - {id: seed, uses: agent/run, with: {agent: a.toml, prompt: seed}}
              - id: work
                with: {output: {schema: Result}, agent: primary.toml, prompt: '${{ steps.seed.text }}'}
                onError:
                  - id: backup
                    with: {output: {schema: Result}, agent: backup.toml, prompt: 'fallback ${{ failed.prompt }}'}
                    uses: agent/run
                finally:
                  - {uses: agent/run, with: {agent: a.toml, prompt: cleanup}}
                uses: agent/run
          - {id: sibling, uses: agent/run, with: {agent: a.toml, prompt: sibling}}
      - {id: after, uses: agent/run, with: {agent: a.toml, prompt: '${{ steps.work.text }}'}}
`

func TestLifecycleBlocksRecoveryCapacityAndReplay(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			calls := map[string]int{}
			var active, peak atomic.Int32
			e, store := durableExecutor(t, lifecycleBlockWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				mu.Lock()
				key := r.Prompt
				if r.AgentID == "primary" || r.AgentID == "backup" {
					key = r.AgentID
				}
				calls[key]++
				mu.Unlock()
				if r.AgentID == "primary" {
					return nil, errors.New("ordinary failure")
				}
				if r.AgentID == "backup" {
					return map[string]any{"text": "recovered", "structured": map[string]any{"value": "seed"}}, nil
				}
				return map[string]any{"text": r.Prompt, "structured": map[string]any{"value": "seed"}}, nil
			}))
			e.Slots = make(chan struct{}, capacity)
			e.Store = &blockCompletionFault{SQLiteStore: store, failID: "joined"}
			run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
			if err == nil || run.FailureClass != "storage" {
				t.Fatalf("lost join: %+v %v", run, err)
			}
			life := run.Jobs["x"].Steps["work"].Lifecycle
			if life == nil || life.OriginalError != "ordinary failure" || len(life.Handlers["onError"]) != 1 {
				t.Fatalf("missing handler evidence: %+v", life)
			}
			if calls["backup"] != 1 || calls["cleanup"] != 1 || calls["recovered"] != 0 || len(e.Slots) != 0 || peak.Load() > int32(capacity) {
				t.Fatalf("recovery/capacity: %v peak=%d slots=%d", calls, peak.Load(), len(e.Slots))
			}
			e.Store = store
			run, err = nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, false)
			if err != nil || run.Status != RunStatusSucceeded || run.Jobs["x"].Steps["after"].Outputs["text"] != "recovered" {
				t.Fatalf("resume: %+v %v", run, err)
			}
			if calls["seed"] != 1 || calls["primary"] != 1 || calls["backup"] != 1 || calls["cleanup"] != 1 || calls["sibling"] != 1 || calls["recovered"] != 1 {
				t.Fatalf("replayed handler work: %v", calls)
			}
		})
	}
}

func TestLifecycleBlocksCancellationSkipsHandlerEffects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	var forbidden atomic.Int32
	source := `jobs:
  x:
    steps:
      - parallel:
          - id: work
            onError: [{uses: agent/run, with: {agent: a.toml, prompt: forbidden}}]
            finally: [{uses: agent/run, with: {agent: a.toml, prompt: forbidden}}]
            uses: agent/run
            with:
              agent: a.toml
              prompt: wait
          - {uses: agent/run, with: {agent: a.toml, prompt: fail}}
      - {uses: agent/run, with: {agent: a.toml, prompt: forbidden}}
`
	e, _ := durableExecutor(t, source, agentFunc(func(ctx context.Context, r AgentRequest) (map[string]any, error) {
		switch r.Prompt {
		case "wait":
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		case "fail":
			select {
			case <-started:
				return nil, errors.New("branch failed")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
			forbidden.Add(1)
			return nil, nil
		}
	}))
	e.Slots = make(chan struct{}, 2)
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || !strings.Contains(err.Error(), "branch failed") || forbidden.Load() != 0 || len(e.Slots) != 0 {
		t.Fatalf("cancellation admitted effects: %+v %v forbidden=%d", run, err, forbidden.Load())
	}
	life := run.Jobs["x"].Steps["work"].Lifecycle
	if life == nil || life.Skipped["onError"] == "" || life.Skipped["finally"] == "" {
		t.Fatalf("missing canceled handler evidence: %+v", life)
	}
}

func TestLifecycleBlocksAdmissionAndReferenceScope(t *testing.T) {
	for _, hook := range []string{"onError", "onFailure", "finally"} {
		t.Run(hook, func(t *testing.T) {
			leaf := Step{Uses: "agent/run", With: map[string]any{"agent": "a.toml", "prompt": "work"}}
			block := Step{Parallel: []Step{leaf}}
			switch hook {
			case "onError":
				block.OnError = []Step{leaf}
			case "onFailure":
				block.OnFailure = []Step{leaf}
			case "finally":
				block.Finally = []Step{leaf}
			}
			if err := Validate(&Workflow{Jobs: map[string]Job{"x": {Steps: []Step{block}}}}); err == nil {
				t.Fatal("block owned handlers")
			}
			block = Step{Sequence: []Step{leaf}}
			switch hook {
			case "onError":
				leaf.OnError = []Step{block}
			case "onFailure":
				leaf.OnFailure = []Step{block}
			case "finally":
				leaf.Finally = []Step{block}
			}
			if err := Validate(&Workflow{Jobs: map[string]Job{"x": {Steps: []Step{leaf}}}}); err == nil {
				t.Fatal("handler admitted block")
			}
		})
	}
	for _, reference := range []string{"steps.seed.text", "steps.sibling.text", "steps.work.text"} {
		source := strings.Replace(lifecycleBlockWorkflow, "steps.seed.text", reference, 1)
		w, err := Parse([]byte(source))
		if err == nil {
			err = Validate(w)
		}
		if (err == nil) != (reference == "steps.seed.text") {
			t.Fatalf("named nested scope %s: %v", reference, err)
		}
	}
}

func TestLifecycleBlocksDoNotReplayUncertainHandlerUnderTerminalOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	e, store := durableExecutor(t, lifecycleBlockWorkflow, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		if r.AgentID == "primary" {
			return nil, errors.New("ordinary failure")
		}
		if r.AgentID == "backup" {
			calls.Add(1)
		}
		return map[string]any{"text": "seed", "structured": map[string]any{"value": "seed"}}, nil
	}))
	e.Store = &blockCompletionFault{SQLiteStore: store, failID: "work/onError/backup"}
	run, err := nativeTestExecutor(t, e).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status == RunStatusSucceeded || calls.Load() != 1 {
		t.Fatalf("lost handler receipt: %+v %v calls=%d", run, err, calls.Load())
	}
	attempts, loadErr := store.Attempts(ctx, run.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	uncertain := false
	for _, a := range attempts {
		if a.StepID == "work/onError/backup" && a.Status != RunStatusSucceeded && a.Phase != "lifecycle_completed" {
			uncertain = true
		}
	}
	if !uncertain {
		t.Fatal("fault did not leave the handler without a completed receipt")
	}
	e.Store = store
	for _, retry := range []bool{false, true} {
		_, err = nativeTestExecutor(t, e).Resume(ctx, run.ID, nil, retry)
		if err == nil || !strings.Contains(err.Error(), "lost checkpoint write") || calls.Load() != 1 {
			t.Fatalf("terminal owner replayed uncertain handler retry=%v: %v calls=%d", retry, err, calls.Load())
		}
	}
}
