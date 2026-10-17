package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type agentFunc func(context.Context, AgentRequest) (map[string]any, error)

func (f agentFunc) RunAgent(ctx context.Context, r AgentRequest) (map[string]any, error) {
	return f(ctx, r)
}

func writeWorkflow(t *testing.T, root, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", name), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

const parallelWorkflow = `name: parallel
on:
  manual: {}
  workflow_call:
    outputs:
      summary:
        value: ${{ jobs.combine.outputs.summary }}
jobs:
  first:
    outputs:
      text: ${{ steps.run.outputs.text }}
    steps:
      - id: run
        uses: agent/claude
        with: {prompt: first}
  second:
    outputs:
      text: ${{ steps.run.outputs.text }}
    steps:
      - id: run
        uses: agent/codex
        with: {prompt: second}
  combine:
    needs: [first, second]
    outputs:
      summary: ${{ steps.run.outputs.text }}
    steps:
      - id: run
        uses: agent/claude
        with:
          prompt: "${{ needs.first.outputs.text }} + ${{ needs.second.outputs.text }}"
`

func TestParallelJobsWaitForBothAndPassOutputs(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", parallelWorkflow)
	started := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	runner := agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		calls.Add(1)
		if req.Prompt == "first" || req.Prompt == "second" {
			started <- req.Prompt
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else if req.Prompt != "first + second" {
			return nil, errors.New("wrong dependency outputs: " + req.Prompt)
		}
		return map[string]any{"text": req.Prompt}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := Executor{WorkspaceDir: root, Agents: runner, MaxParallel: 2, Store: FileStore{Dir: filepath.Join(root, "runs")}}
	type result struct {
		run *Run
		err error
	}
	finished := make(chan result, 1)
	go func() { r, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"}); finished <- result{r, err} }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent jobs did not start concurrently")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("combine started before dependencies finished")
	}
	close(release)
	got := <-finished
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.run.Outputs["summary"] != "first + second" || got.run.Status != RunStatusSucceeded {
		t.Fatalf("unexpected run: %+v", got.run)
	}
	data, err := os.ReadFile(filepath.Join(root, "runs", got.run.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Run
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != RunStatusSucceeded || len(saved.Jobs) != 3 {
		t.Fatalf("bad checkpoint: %+v", saved)
	}
}

func TestParallelLimitAndFailurePropagation(t *testing.T) {
	for _, continued := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "continue"}[continued], func(t *testing.T) {
			root := t.TempDir()
			source := parallelWorkflow
			if continued {
				source = strings.Replace(source, "  first:\n", "  first:\n    continue-on-error: true\n", 1)
			}
			writeWorkflow(t, root, "main.yml", source)
			var active, maximum atomic.Int32
			runner := agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := maximum.Load(); n > old; old = maximum.Load() {
					if maximum.CompareAndSwap(old, n) {
						break
					}
				}
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if req.Prompt == "first" {
					return nil, errors.New("planned failure")
				}
				return map[string]any{"text": req.Prompt}, nil
			})
			run, err := (&Executor{WorkspaceDir: root, Agents: runner, MaxParallel: 1}).Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if maximum.Load() != 1 {
				t.Fatalf("parallel cap exceeded: %d", maximum.Load())
			}
			if !continued {
				if err == nil || run.Jobs["combine"].Status != RunStatusSkipped {
					t.Fatalf("failure not propagated: %+v %v", run, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReusableWorkflowAndStructuredOutput(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "child.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
    outputs:
      answer: {value: '${{ jobs.child.outputs.answer }}'}
jobs:
  child:
    outputs:
      answer: ${{ steps.ask.outputs.structured.answer }}
    steps:
      - id: ask
        uses: agent/claude
        with:
          prompt: ${{ inputs.task }}
          output:
            format: json
            schema:
              type: object
              required: [answer]
              properties:
                answer: {type: string}
`)
	writeWorkflow(t, root, "main.yml", `on:
  workflow_call:
    outputs:
      answer: {value: '${{ jobs.call.outputs.answer }}'}
jobs:
  call:
    uses: workflows/child.yml
    with: {task: hello}
`)
	var calls atomic.Int32
	runner := agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		if calls.Add(1) == 1 {
			return map[string]any{"text": `{"answer":2}`}, nil
		}
		if !strings.Contains(req.Prompt, "failed validation") {
			return nil, errors.New("missing repair instruction")
		}
		return map[string]any{"text": `{"answer":"ok"}`}, nil
	})
	run, err := (&Executor{WorkspaceDir: root, Agents: runner, MaxParallel: 1}).Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Outputs["answer"] != "ok" || calls.Load() != 2 {
		t.Fatalf("unexpected run %+v calls %d", run, calls.Load())
	}
}

func TestCancellationAndInvalidClosureHaveNoFurtherEffects(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "main.yml", parallelWorkflow)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	runner := agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	type result struct {
		r   *Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := (&Executor{WorkspaceDir: root, Agents: runner}).Run(ctx, RunRequest{Ref: "workflows/main.yml"})
		done <- result{r, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no agent started")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.r.Status != RunStatusCanceled {
			t.Fatalf("bad cancel: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	writeWorkflow(t, root, "cycle.yml", `jobs:
  child:
    uses: workflows/cycle.yml
`)
	if _, err := (&Executor{WorkspaceDir: root, Agents: runner}).Load("workflows/cycle.yml"); err == nil {
		t.Fatal("accepted reusable cycle")
	}
}

func TestStrictParseAndValidation(t *testing.T) {
	for _, source := range []string{
		"jobs: {}\nunknown: true", "jobs: {}\njobs: {}", "jobs: {}\n---\njobs: {}",
		"jobs: &jobs {}\non: {manual: *jobs}",
	} {
		if _, err := Parse([]byte(source)); err == nil {
			t.Errorf("accepted invalid YAML %q", source)
		}
	}
	for _, source := range []string{
		"on: {schedule: [{cron: '* * * * *'}]}\njobs: {x: {steps: [{uses: agent/claude, with: {prompt: hi}}]}}",
		"jobs: {x: {needs: y, steps: [{uses: agent/claude, with: {prompt: hi}}]}}",
		"jobs: {x: {needs: x, steps: [{uses: agent/claude, with: {prompt: hi}}]}}",
		"jobs: {x: {steps: [{uses: tool/shell, with: {prompt: hi}}]}}",
		"jobs: {x: {steps: [{run: echo hi}]}}",
	} {
		w, err := Parse([]byte(source))
		if err == nil {
			err = Validate(w)
		}
		if err == nil {
			t.Errorf("accepted unsupported workflow %q", source)
		}
	}
}
