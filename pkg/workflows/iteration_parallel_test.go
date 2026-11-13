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

	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

type parallelIterationResult struct {
	run *Run
	err error
}

func parallelIterationExecutor(t *testing.T, concurrency, capacity int, runner AgentRunner) (*Executor, *SQLiteStore) {
	t.Helper()
	e, s := iterationExecutor(t, runner)
	source := strings.Replace(iterationSource, "      key: item.id", fmt.Sprintf("      key: item.id\n      concurrency: %d", concurrency), 1)
	writeWorkflow(t, e.WorkspaceDir, "main.yml", source)
	e.Slots = make(chan struct{}, capacity)
	// The durable native-task scheduler must use the same capacity as the
	// iteration executor after this fixture overrides its serial default.
	nativeTestExecutor(t, e)
	return e, s
}

// Test cleanup cancels the run and waits for completion or a timeout.
func startParallelIteration(t *testing.T, e *Executor, req RunRequest) (context.Context, context.CancelFunc, <-chan parallelIterationResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result := make(chan parallelIterationResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		run, err := e.Run(ctx, req)
		result <- parallelIterationResult{run, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("iteration failed to join after cancellation")
		}
	})
	return ctx, cancel, result
}

func iterationReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-ctx.Done():
		t.Fatal("bounded iteration wait:", ctx.Err())
		var zero T
		return zero
	}
}

func TestParallelIterationBoundAndOrderedResults(t *testing.T) {
	started := make(chan string, 4)
	releases := map[string]chan struct{}{}
	for _, task := range []string{"a", "b", "c", "d"} {
		releases[task] = make(chan struct{})
	}
	var active, maximum atomic.Int32
	e, s := parallelIterationExecutor(t, 2, 4, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		started <- req.Prompt
		select {
		case <-releases[req.Prompt]:
			return map[string]any{"text": req.Prompt}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	ctx, _, done := startParallelIteration(t, e, iterationRequest("a", "b", "c", "d"))
	first := []string{iterationReceive(t, ctx, started), iterationReceive(t, ctx, started)}
	if !(first[0] == "a" && first[1] == "b" || first[0] == "b" && first[1] == "a") {
		t.Fatal(first)
	}
	// Both children are physically blocked; the item journal proves that the
	// higher host capacity has not admitted a third item through this bound.
	// Read the originating run from its pinned plan.
	var origin string
	if err := s.db.QueryRowContext(ctx, "SELECT run_id FROM iteration_plans WHERE job_id='review'").Scan(&origin); err != nil {
		t.Fatal(err)
	}
	records, err := s.IterationAttempts(ctx, origin, "review")
	if err != nil || len(records) != 2 {
		t.Fatal(records, err)
	}
	close(releases["b"])
	if task := iterationReceive(t, ctx, started); task != "c" {
		t.Fatal(task)
	}
	records, err = s.IterationAttempts(ctx, origin, "review")
	if err != nil {
		t.Fatal(err)
	}
	completed := []string{}
	for _, r := range records {
		if r.Status == "completed" {
			completed = append(completed, r.Key)
		}
	}
	if !reflect.DeepEqual(completed, []string{"r2"}) {
		t.Fatal("out-of-order completion not retained:", records)
	}
	close(releases["c"])
	if task := iterationReceive(t, ctx, started); task != "d" {
		t.Fatal(task)
	}
	close(releases["d"])
	close(releases["a"])
	result := iterationReceive(t, ctx, done)
	if result.err != nil || maximum.Load() != 2 || active.Load() != 0 {
		t.Fatal(result.err, maximum.Load(), active.Load())
	}
	job := result.run.Jobs["review"]
	if !job.Complete {
		t.Fatal(job)
	}
	for i, task := range []string{"a", "b", "c", "d"} {
		r := job.Items[i]
		if r.Number != i+1 || r.Key != fmt.Sprintf("r%d", i+1) || r.Outputs["text"] != task || r.Status != "completed" {
			t.Fatal(job.Items)
		}
	}
}

func TestParallelIterationSharedCapacityOne(t *testing.T) {
	var active, maximum atomic.Int32
	e, _ := parallelIterationExecutor(t, 3, 1, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	ctx, _, done := startParallelIteration(t, e, iterationRequest("a", "b", "c"))
	result := iterationReceive(t, ctx, done)
	if result.err != nil || !result.run.Jobs["review"].Complete || maximum.Load() != 1 {
		t.Fatal(result.run, result.err, maximum.Load())
	}
}

func TestParallelIterationFailureCancelsAndJoinsSiblings(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	allowCleanup := make(chan struct{})
	var pendingCalls atomic.Int32
	var outer context.Context
	e, _ := parallelIterationExecutor(t, 2, 2, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		switch req.Prompt {
		case "fail":
			select {
			case <-started:
				return nil, process.ErrCleanup
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "sibling":
			close(started)
			<-ctx.Done()
			close(canceled)
			select {
			case <-allowCleanup:
			case <-outer.Done():
			}
			return nil, ctx.Err()
		default:
			pendingCalls.Add(1)
			return nil, nil
		}
	}))
	// Publish the outer context before dispatch; unlike the sibling context it
	// stays live during the deliberately delayed cancellation cleanup.
	outerCtx, outerCancel := context.WithTimeout(context.Background(), 10*time.Second)
	outer = outerCtx
	ctx, _, done := startParallelIteration(t, e, iterationRequest("fail", "sibling", "pending"))
	t.Cleanup(outerCancel)
	iterationReceive(t, ctx, canceled)
	select {
	case result := <-done:
		t.Fatal("returned before joining sibling:", result)
	default:
	}
	close(allowCleanup)
	result := iterationReceive(t, ctx, done)
	if result.err == nil || result.run.Status != RunStatusNeedsAttention || pendingCalls.Load() != 0 {
		t.Fatal(result.run, result.err, pendingCalls.Load())
	}
	job := result.run.Jobs["review"]
	if job.Complete || job.Items[0].Status != "interrupted" || job.Items[1].Status != "interrupted" || job.Items[2].Status != "pending" || job.FailureClass == "canceled" {
		t.Fatal(job)
	}
}

func TestParallelIterationProviderSkipsAndRetry(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	var retry atomic.Bool
	e, s := parallelIterationExecutor(t, 3, 3, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		mu.Lock()
		calls[req.Prompt]++
		mu.Unlock()
		if !retry.Load() {
			switch req.Prompt {
			case "exhaust":
				return nil, ErrUsageExhausted
			case "fail":
				return nil, ErrProviderFailure
			}
		}
		return map[string]any{"text": req.Prompt}, nil
	}))
	ctx, _, done := startParallelIteration(t, e, iterationRequest("ok", "exhaust", "fail", "last"))
	result := iterationReceive(t, ctx, done)
	if !errors.Is(result.err, ErrProviderFailure) {
		t.Fatal(result.err)
	}
	want := []string{"completed", "exhausted", "provider_failure", "completed"}
	for i, item := range result.run.Jobs["review"].Items {
		if item.Status != want[i] {
			t.Fatal(result.run.Jobs["review"])
		}
	}
	before, err := s.IterationAttempts(ctx, result.run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	retry.Store(true)
	run, err := e.Resume(ctx, result.run.ID, nil, true)
	if err != nil || !run.Jobs["review"].Complete {
		t.Fatal(run, err)
	}
	if !reflect.DeepEqual(calls, map[string]int{"ok": 1, "last": 1, "exhaust": 2, "fail": 2}) {
		t.Fatal(calls)
	}
	after, err := s.IterationAttempts(ctx, run.ID, "review")
	if err != nil || len(after) != len(before)+4 {
		t.Fatal(after, err)
	}
	for i := range before {
		if actionHash(before[i]) != actionHash(after[i]) {
			t.Fatal("prior result changed")
		}
	}
}

type iterationCancelOnCompletion struct {
	*SQLiteStore
	cancel context.CancelFunc
	once   sync.Once
}

func (s *iterationCancelOnCompletion) AppendIteration(ctx context.Context, run, job string, r IterationItemResult) error {
	err := s.SQLiteStore.AppendIteration(ctx, run, job, r)
	if err == nil && r.Status == "completed" && r.Key == "r1" {
		s.once.Do(s.cancel)
	}
	return err
}

func TestParallelIterationRestartKeepsCompletedAndGuardsInterrupted(t *testing.T) {
	siblingStarted := make(chan struct{})
	var mu sync.Mutex
	calls := map[string]int{}
	e, store := parallelIterationExecutor(t, 2, 2, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		mu.Lock()
		calls[req.Prompt]++
		mu.Unlock()
		if req.Prompt == "done" {
			select {
			case <-siblingStarted:
				return map[string]any{"text": "done"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		close(siblingStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Store = &iterationCancelOnCompletion{SQLiteStore: store, cancel: cancel}
	run, err := e.Run(ctx, iterationRequest("done", "interrupted", "pending"))
	if err == nil || run.Jobs["review"].Items[0].Status != "completed" {
		t.Fatal(run, err)
	}
	before, err := store.IterationAttempts(context.Background(), run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	e.Agents = agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
		mu.Lock()
		calls[req.Prompt]++
		mu.Unlock()
		return map[string]any{"text": req.Prompt}, nil
	})
	// Retry acknowledgement is mandatory for the interrupted physical child.
	interrupted := run.Jobs["review"].Items[1]
	if err := e.guardIterationRetry(context.Background(), interrupted, RunRequest{}); err == nil {
		t.Fatal("interrupted work admitted without acknowledgement")
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resumeCancel()
	run, err = nativeTestExecutor(t, e).Resume(resumeCtx, run.ID, nil, true)
	if err != nil || !run.Jobs["review"].Complete {
		t.Fatal(run, err)
	}
	if !reflect.DeepEqual(calls, map[string]int{"done": 1, "interrupted": 2, "pending": 1}) {
		t.Fatal(calls)
	}
	after, err := reopened.IterationAttempts(resumeCtx, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		if actionHash(before[i]) != actionHash(after[i]) {
			t.Fatal("retained observation changed")
		}
	}
}

func TestParallelIterationConcurrencyValidationAndPinning(t *testing.T) {
	for _, value := range []string{"0", "-1", "1.5", "1.0", "true", "null", "'2'", "'${{ inputs.bound }}'", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			source := "jobs: {review: {uses: workflows/child.yml, for-each: {items: [], key: item.id, concurrency: " + value + "}}}"
			w, err := Parse([]byte(source))
			if err == nil {
				err = Validate(w)
			}
			if err == nil {
				t.Fatal("invalid concurrency accepted:", value)
			}
		})
	}
	e, s := parallelIterationExecutor(t, 2, 2, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) { return map[string]any{"text": "ok"}, nil }))
	ctx, _, done := startParallelIteration(t, e, iterationRequest("one"))
	result := iterationReceive(t, ctx, done)
	if result.err != nil {
		t.Fatal(result.err)
	}
	p, err := s.IterationPlan(ctx, result.run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := s.Load(ctx, result.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	job := snap.Definitions["workflows/main.yml"].Jobs["review"]
	if job.ForEach.Concurrency == nil || *job.ForEach.Concurrency != 2 {
		t.Fatal("bound not retained", job)
	}
	req := RunRequest{runID: p.RunID, Inputs: snap.Inputs, Metadata: snap.Metadata, Event: snap.Event, Execution: snap.Execution, actionContracts: snap.ActionContracts}
	unchanged, err := e.iterationPlan("review", job, snap.Inputs, req, nil, snap.Definitions)
	if err != nil || unchanged.Fingerprint != p.Fingerprint {
		t.Fatal("identical admitted bound changed the plan", err, unchanged.Fingerprint, p.Fingerprint)
	}
	changedJob := job
	changedEach := *job.ForEach
	changedJob.ForEach = &changedEach
	bound := 3
	changedJob.ForEach.Concurrency = &bound
	changed, err := e.iterationPlan("review", changedJob, snap.Inputs, req, nil, snap.Definitions)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PinIteration(ctx, changed); !errors.Is(err, ErrActionIntegrity) {
		t.Fatal("changed bound reused plan", err)
	}
}

func TestParallelIterationValidatesAllInputsBeforeChildren(t *testing.T) {
	for _, invalid := range []any{
		[]any{map[string]any{"id": "a", "task": "good"}, map[string]any{"id": "a", "task": "duplicate"}},
		[]any{map[string]any{"id": "a", "task": "good"}, map[string]any{"id": "b", "task": 42}},
	} {
		e, _ := parallelIterationExecutor(t, 3, 3, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
			t.Error("invalid later item dispatched a child")
			return nil, nil
		}))
		req := iterationRequest()
		req.Inputs["reviewers"] = invalid
		ctx, _, done := startParallelIteration(t, e, req)
		result := iterationReceive(t, ctx, done)
		if result.err == nil || result.run.Status != RunStatusNeedsAttention {
			t.Fatal(result.run, result.err)
		}
	}
}

func TestParallelIterationSuspensionJoinsAndPreservesSiblingFailures(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, hardFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("nested=%t/hard=%t", nested, hardFailure), func(t *testing.T) {
				started := make(chan struct{})
				joined := make(chan struct{})
				var pendingCalls atomic.Int32
				e, s := parallelIterationExecutor(t, 2, 2, agentFunc(func(ctx context.Context, req AgentRequest) (map[string]any, error) {
					if req.Prompt != "sibling" {
						pendingCalls.Add(1)
						return nil, errors.New("unexpected pending dispatch")
					}
					close(started)
					<-ctx.Done()
					defer close(joined)
					if hardFailure {
						return nil, errors.Join(ctx.Err(), process.ErrCleanup)
					}
					return nil, ctx.Err()
				}))
				writeWorkflow(t, e.WorkspaceDir, "child.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
      ordinal: {type: number}
      identity: {type: string}
jobs:
  work:
    steps:
      - id: question
        if: inputs.task == 'gate'
        uses: input/wait
        with: {prompt: 'Continue?', evidence-id: retained}
      - id: run
        if: inputs.task != 'gate'
        uses: agent/run
        with: {prompt: '${{ inputs.task }}', agent: reviewer.toml}
`)
				if nested {
					writeWorkflow(t, e.WorkspaceDir, "child.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
      ordinal: {type: number}
      identity: {type: string}
jobs:
  question:
    if: inputs.task == 'gate'
    steps:
      - id: question
        uses: input/wait
        with: {prompt: 'Continue?', evidence-id: retained}
  nested:
    if: inputs.task != 'gate'
    uses: workflows/leaf.yml
    for-each:
      items: [{id: leaf}]
      key: item.id
      concurrency: 2
    with: {task: '${{ inputs.task }}'}
`)
					writeWorkflow(t, e.WorkspaceDir, "leaf.yml", `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
jobs:
  work:
    steps:
      - uses: agent/run
        with: {prompt: '${{ inputs.task }}', agent: reviewer.toml}
`)
				}
				wrapper := &executionBoundaryJournal{SQLiteStore: s}
				wrapper.afterGate = func(ctx context.Context, gate ExternalInputGate) error {
					select {
					case <-started:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				e.Store = wrapper
				ctx, _, done := startParallelIteration(t, e, iterationRequest("gate", "sibling", "pending"))
				result := iterationReceive(t, ctx, done)
				select {
				case <-joined:
				default:
					t.Fatalf("suspended iteration did not join sibling: %+v err=%v", result.run, result.err)
				}
				if pendingCalls.Load() != 0 {
					t.Fatal("suspension admitted pending work")
				}
				job := result.run.Jobs["review"]
				if job.Items[0].Status != "waiting" || job.Items[1].Status != "interrupted" || job.Items[2].Status != "pending" || job.Complete {
					t.Fatal(job)
				}
				if hardFailure {
					if result.err == nil || result.run.Status != RunStatusNeedsAttention || job.Status != RunStatusNeedsAttention {
						t.Fatal("suspension hid sibling cleanup failure", result.run, result.err)
					}
				} else if result.err != nil || result.run.Status != RunStatusWaiting || job.Status != RunStatusWaiting {
					t.Fatal(result.run, result.err)
				}
				gate := inputGate(t, s, result.run.ID)
				if gate.State != "pending" || gate.EvidenceID != "retained" {
					t.Fatal(gate)
				}
			})
		}
	}
}
