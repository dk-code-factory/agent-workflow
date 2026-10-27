package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/ownership"
	"github.com/dk-code-factory/agent-workflow/pkg/operational"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
)

const iterationSource = `jobs:
  review:
    uses: workflows/child.yml
    for-each:
      items: ${{ inputs.reviewers }}
      key: item.id
    with:
      task: ${{ item.task }}
      ordinal: ${{ iteration.number }}
      identity: ${{ iteration.key }}
`
const iterationChild = `on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
      ordinal: {type: number}
      identity: {type: string}
    outputs:
      text: {value: '${{ jobs.review.outputs.text }}'}
jobs:
  review:
    outputs: {text: '${{ steps.run.outputs.text }}'}
    steps:
      - id: run
        uses: agent/reviewer
        with: {prompt: '${{ inputs.task }}'}
`

func iterationExecutor(t *testing.T, runner AgentRunner) (*Executor, *SQLiteStore) {
	e, s := durableExecutor(t, iterationSource, runner)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", iterationChild)
	e.Slots = make(chan struct{}, 1)
	return e, s
}
func iterationRequest(tasks ...string) RunRequest {
	items := make([]any, len(tasks))
	for i, task := range tasks {
		items[i] = map[string]any{"id": fmt.Sprintf("r%d", i+1), "task": task}
	}
	return RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"reviewers": items}}
}
func TestOrderedIterationPinnedDataAndSequentialCapacity(t *testing.T) {
	var calls []string
	e, s := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.Prompt)
		return map[string]any{"text": r.Prompt}, nil
	}))
	ctx := context.Background()
	req := iterationRequest("third", "${{ secrets.do_not_render }}", "first")
	run, err := e.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"third", "${{ secrets.do_not_render }}", "first"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
	job := run.Jobs["review"]
	if !job.Complete || !job.Outputs["complete"].(bool) || len(job.Items) != 3 {
		t.Fatalf("%+v", job)
	}
	p, err := s.IterationPlan(ctx, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if p.Items[1].Inputs["identity"] != "r2" || nativeInt(p.Items[1].Inputs, "ordinal", 0) != 2 {
		t.Fatal(p)
	}
	before, _ := json.Marshal(p)
	result, err := e.Resume(ctx, run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded || len(calls) != 3 {
		t.Fatal(result, err, calls)
	}
	after, _ := s.IterationPlan(ctx, run.ID, "review")
	encoded, _ := json.Marshal(after)
	if string(before) != string(encoded) {
		t.Fatal("completed plan changed")
	}
}
func TestOrderedIterationIdentitiesBeforeDispatch(t *testing.T) {
	for _, items := range []any{[]any{map[string]any{"id": "x", "task": "a"}, map[string]any{"id": "x", "task": "b"}}, []any{map[string]any{"task": "missing"}}, []any{map[string]any{"id": 4, "task": "numeric"}}, map[string]any{"bad": "array"}, nil} {
		t.Run(fmt.Sprint(items), func(t *testing.T) {
			e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				t.Fatal("invalid list dispatched")
				return nil, nil
			}))
			req := iterationRequest()
			req.Inputs["reviewers"] = items
			run, err := e.Run(context.Background(), req)
			if err == nil || run.Status != RunStatusNeedsAttention {
				t.Fatal(run, err)
			}
		})
	}
}
func TestOrderedIterationAggregateAndExplicitRetry(t *testing.T) {
	ctx := context.Background()
	var calls []string
	retry := false
	e, s := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.Prompt)
		if !retry {
			switch r.Prompt {
			case "exhaust":
				return map[string]any{"text": "partial"}, ErrUsageExhausted
			case "fail":
				return nil, ErrProviderFailure
			}
		}
		return map[string]any{"text": r.Prompt}, nil
	}))
	run, err := e.Run(ctx, iterationRequest("ok", "exhaust", "fail", "last"))
	if err == nil {
		t.Fatal("unresolved failure accepted")
	}
	job := run.Jobs["review"]
	statuses := []string{}
	for _, r := range job.Items {
		statuses = append(statuses, r.Status)
	}
	if !reflect.DeepEqual(statuses, []string{"completed", "exhausted", "provider_failure", "completed"}) || job.Complete {
		t.Fatal(job)
	}
	p, _ := s.IterationPlan(ctx, run.ID, "review")
	before, _ := s.IterationAttempts(ctx, run.ID, "review")
	if _, err = e.Resume(ctx, run.ID, nil, false); err == nil {
		t.Fatal("uncertainty not acknowledged")
	}
	retry = true
	run, err = e.Resume(ctx, run.ID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Jobs["review"].Complete || !reflect.DeepEqual(calls, []string{"ok", "exhaust", "fail", "last", "exhaust", "fail"}) {
		t.Fatal(run, calls)
	}
	after, _ := s.IterationAttempts(ctx, run.ID, "review")
	if len(after) != len(before)+4 {
		t.Fatal(after)
	}
	for i := range before {
		if actionHash(before[i]) != actionHash(after[i]) {
			t.Fatal("prior attempt mutated")
		}
	}
	current, _ := s.IterationPlan(ctx, run.ID, "review")
	if actionHash(p) != actionHash(current) {
		t.Fatal("plan changed")
	}
}
func TestOrderedIterationEmptyAndAllExhausted(t *testing.T) {
	for _, tasks := range [][]string{{}, {"a", "b"}, {"ok", "exhaust"}} {
		t.Run(fmt.Sprint(tasks), func(t *testing.T) {
			var calls int
			e, _ := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls++
				if r.Prompt != "ok" {
					return nil, ErrUsageExhausted
				}
				return map[string]any{"text": "ok"}, nil
			}))
			run, err := e.Run(context.Background(), iterationRequest(tasks...))
			if err != nil {
				t.Fatal(err)
			}
			if run.Jobs["review"].Complete != (len(tasks) > 0 && tasks[0] == "ok") {
				t.Fatal(run)
			}
			if len(tasks) > 0 {
				_, err = e.Resume(context.Background(), run.ID, nil, true)
				if err != nil || calls != len(tasks)+len(tasks)-boolInt(tasks[0] == "ok") {
					t.Fatal(calls, err)
				}
			}
		})
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func TestOrderedIterationGuardsCannotContinue(t *testing.T) {
	for _, cause := range []error{ErrActionIntegrity, process.ErrCleanup, operational.ErrUnavailable, errors.New("sqlite disk full"), errors.New("instruction guard changed"), errors.Join(ErrProviderFailure, process.ErrCleanup)} {
		t.Run(cause.Error(), func(t *testing.T) {
			var calls int
			e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) { calls++; return nil, cause }))
			writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.Replace(iterationSource, "    uses:", "    continue-on-error: true\n    uses:", 1))
			writeWorkflow(t, e.WorkspaceDir, "child.yml", strings.Replace(iterationChild, "        uses:", "        continue-on-error: true\n        uses:", 1))
			run, err := e.Run(context.Background(), iterationRequest("bad", "untouched"))
			if err == nil || calls != 1 || run.Status != RunStatusNeedsAttention {
				t.Fatal(run, calls, err)
			}
			items := run.Jobs["review"].Items
			if items[0].Status != "interrupted" || items[1].Status != "pending" {
				t.Fatal(items)
			}
		})
	}
}

type iterationFaultStore struct {
	*SQLiteStore
	failResult atomic.Bool
	failParent atomic.Bool
	failChild  atomic.Bool
}

func (s *iterationFaultStore) AppendIteration(ctx context.Context, run, job string, r IterationItemResult) error {
	if r.Status == "completed" && s.failResult.CompareAndSwap(true, false) {
		return errors.New("storage checkpoint lost")
	}
	return s.SQLiteStore.AppendIteration(ctx, run, job, r)
}
func (s *iterationFaultStore) Save(ctx context.Context, r *Run) error {
	if r.OriginID != r.ID && r.Status == RunStatusSucceeded && s.failChild.CompareAndSwap(true, false) {
		return errors.New("child storage checkpoint lost")
	}
	if r.OriginID == r.ID && len(r.Jobs) > 0 && s.failParent.CompareAndSwap(true, false) {
		return errors.New("parent storage checkpoint lost")
	}
	return s.SQLiteStore.Save(ctx, r)
}
func TestOrderedIterationLostCheckpoints(t *testing.T) {
	for _, target := range []string{"item", "parent", "child"} {
		t.Run(target, func(t *testing.T) {
			var calls []string
			e, s := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.Prompt)
				return map[string]any{"text": r.Prompt}, nil
			}))
			fault := &iterationFaultStore{SQLiteStore: s}
			if target == "item" {
				fault.failResult.Store(true)
			} else if target == "child" {
				fault.failChild.Store(true)
			} else {
				fault.failParent.Store(true)
			}
			e.Store = fault
			ctx := context.Background()
			run, err := e.Run(ctx, iterationRequest("first", "second"))
			if err == nil {
				t.Fatal("fault not reached")
			}
			if err = s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			e.Store = s
			run, err = e.Resume(ctx, run.ID, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, []string{"first", "second"}) || !run.Jobs["review"].Complete {
				t.Fatal(run, calls)
			}
		})
	}
}
func TestOrderedIterationInterruptionRestartAndConcurrentResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls []string
	e, s := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.Prompt)
		if r.Prompt == "interrupt" {
			cancel()
			return nil, context.Canceled
		}
		return map[string]any{"text": r.Prompt}, nil
	}))
	run, err := e.Run(ctx, iterationRequest("done", "interrupt", "pending"))
	if err == nil {
		t.Fatal("not interrupted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	e.Store = reopened
	e.Agents = agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls = append(calls, r.Prompt)
		return map[string]any{"text": r.Prompt}, nil
	})
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reopened.QueueResume(context.Background(), run.ID, "", true, 0); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("concurrent resume admitted", successes.Load())
	}
	run, err = e.Resume(context.Background(), run.ID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"done", "interrupt", "interrupt", "pending"}) || !run.Jobs["review"].Complete {
		t.Fatal(calls, run)
	}
}
func TestOrderedIterationLegacyStoreRejected(t *testing.T) {
	e, _ := iterationExecutor(t, nil)
	e.Store = FileStore{Dir: t.TempDir()}
	if _, err := e.Run(context.Background(), iterationRequest("a")); err == nil || !strings.Contains(err.Error(), "IterationJournal") {
		t.Fatal(err)
	}
}
func TestOrderedIterationMigrationFromV8(t *testing.T) {
	ctx := context.Background()
	e, s := iterationExecutor(t, nil)
	run, err := e.Prepare(ctx, iterationRequest("a"))
	if err != nil {
		t.Fatal(err)
	}
	_, snap, _ := s.Load(ctx, run.ID)
	before := actionHash(snap)
	if _, err = s.db.Exec("DROP TABLE iteration_attempts; DROP TABLE iteration_plans; PRAGMA user_version=8"); err != nil {
		t.Fatal(err)
	}
	owner, err := ownership.Own(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = s.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	_, snap, _ = s.Load(ctx, run.ID)
	if actionHash(snap) != before {
		t.Fatal("snapshot rewritten")
	}
	path := filepath.Join(s.Dir, "state.pre-v9.db")
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var version int
	if err = old.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
		t.Fatal(version, err)
	}
}

func TestOrderedIterationIndependentJobsAndNestedCalls(t *testing.T) {
	var calls atomic.Int32
	e, _ := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"text": r.Prompt}, nil
	}))
	writeWorkflow(t, e.WorkspaceDir, "wrapper.yml", `on:
  workflow_call:
    inputs: {task: {type: string}, ordinal: {type: number}, identity: {type: string}}
    outputs: {text: {value: '${{ jobs.nested.outputs.text }}'}}
jobs:
  nested:
    uses: workflows/child.yml
    with: {task: '${{ inputs.task }}', ordinal: '${{ inputs.ordinal }}', identity: '${{ inputs.identity }}'}
`)
	writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.ReplaceAll(iterationSource, "workflows/child.yml", "workflows/wrapper.yml")+`  other:
    uses: workflows/child.yml
    for-each: {items: [{id: separate, task: independent}], key: '${{ item.id }}'}
    with: {task: '${{ item.task }}'}
`)
	run, err := e.Run(context.Background(), iterationRequest("a", "b"))
	if err != nil || calls.Load() != 3 || !run.Jobs["review"].Complete || !run.Jobs["other"].Complete {
		t.Fatal(run, calls.Load(), err)
	}
}
func TestOrderedIterationBindingAndEvidenceDrift(t *testing.T) {
	for _, kind := range []string{"binding", "evidence", "instruction", "checkout", "definition"} {
		t.Run(kind, func(t *testing.T) {
			var providerCalls atomic.Int32
			var actionCalls atomic.Int32
			drift := false
			e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				providerCalls.Add(1)
				return map[string]any{"text": "done"}, nil
			}))
			source := `jobs:
  guard:
    steps: [{id: capture, uses: action/capture, with: {value: stable}}]
  review:
    needs: guard
    uses: workflows/child.yml
    for-each: {items: '${{ inputs.reviewers }}', key: item.id}
    with: {task: '${{ item.task }}'}
`
			writeWorkflow(t, e.WorkspaceDir, "main.yml", source)
			spec := readSpec(&actionCalls)
			spec.Bindings = func(context.Context, ActionContext, actionInput) (json.RawMessage, error) {
				if drift && kind == "binding" {
					return json.RawMessage(`{"repository":"changed"}`), nil
				}
				if drift && (kind == "instruction" || kind == "checkout") {
					return nil, fmt.Errorf("%s guard changed", kind)
				}
				return json.RawMessage(`{"repository":"fixture"}`), nil
			}
			validate := spec.ValidateCheckpoint
			spec.ValidateCheckpoint = func(ctx context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) error {
				if drift && kind == "evidence" {
					return errors.New("evidence missing")
				}
				return validate(ctx, ac, cp)
			}
			registerTestAction(t, e, spec)
			run, err := e.Run(context.Background(), iterationRequest("a", "b"))
			if err != nil {
				t.Fatal(err)
			}
			drift = true
			if kind == "definition" {
				spec.Implementation = "changed"
				registerTestAction(t, e, spec)
			}
			if _, err = e.Resume(context.Background(), run.ID, nil, false); !errors.Is(err, ErrActionIntegrity) || providerCalls.Load() != 2 {
				t.Fatal(err, providerCalls.Load())
			}
		})
	}
}
func TestOrderedIterationGuardRecheckedBetweenChildren(t *testing.T) {
	var calls atomic.Int32
	var guards atomic.Int32
	drift := false
	e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		calls.Add(1)
		drift = true
		return map[string]any{"text": "done"}, nil
	}))
	writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.Replace(iterationSource, "  review:", "  guard:\n    steps: [{uses: action/capture, with: {value: stable}}]\n  review:\n    needs: guard", 1))
	spec := readSpec(&guards)
	validate := spec.ValidateCheckpoint
	spec.ValidateCheckpoint = func(ctx context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) error {
		if drift {
			return errors.New("checkout guard changed")
		}
		return validate(ctx, ac, cp)
	}
	registerTestAction(t, e, spec)
	run, err := e.Run(context.Background(), iterationRequest("a", "b"))
	if !errors.Is(err, ErrActionIntegrity) || calls.Load() != 1 || run.Jobs["review"].Items[1].Status != "pending" {
		t.Fatal(run, err, calls.Load())
	}
}
func TestOrderedIterationDownstreamInputInvalidation(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(fmt.Sprint(mutation), func(t *testing.T) {
			retry := false
			var aggregates []bool
			var reviewerCalls int
			var mutations atomic.Int32
			e, _ := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				if r.Prompt == "aggregate false" || r.Prompt == "aggregate true" {
					aggregates = append(aggregates, r.Prompt == "aggregate true")
					return map[string]any{"text": r.Prompt}, nil
				}
				reviewerCalls++
				if r.Prompt == "exhaust" && !retry {
					return nil, ErrUsageExhausted
				}
				return map[string]any{"text": r.Prompt}, nil
			}))
			source := iterationSource + `  aggregate:
    needs: review
    steps:
      - id: aggregate
        uses: agent/reviewer
        with: {prompt: 'aggregate ${{ needs.review.outputs.complete }}'}
`
			if mutation {
				source = iterationSource + `  aggregate:
    needs: review
    steps:
      - id: publish
        uses: action/capture
        with: {value: 'aggregate ${{ needs.review.outputs.complete }}'}
`
				spec := readSpec(&mutations)
				spec.Effect = ActionExternalMutation
				registerTestAction(t, e, spec)
			}
			writeWorkflow(t, e.WorkspaceDir, "main.yml", source)
			run, err := e.Run(context.Background(), iterationRequest("exhaust"))
			if err != nil {
				t.Fatal(err)
			}
			retry = true
			run, err = e.Resume(context.Background(), run.ID, nil, true)
			if mutation {
				if !errors.Is(err, ErrActionIntegrity) || mutations.Load() != 1 {
					t.Fatal(run, err, mutations.Load())
				}
			} else if err != nil || !reflect.DeepEqual(aggregates, []bool{false, true}) {
				t.Fatal(run, err, aggregates)
			}
			if reviewerCalls != 2 {
				t.Fatal(reviewerCalls)
			}
		})
	}
}
func TestOrderedIterationMutationRetryReconciliation(t *testing.T) {
	for _, outcome := range []string{"no_effect", "completed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			var mutations atomic.Int32
			var reconciles atomic.Int32
			e, _ := iterationExecutor(t, nil)
			writeWorkflow(t, e.WorkspaceDir, "child.yml", strings.Replace(iterationChild, "uses: agent/reviewer\n        with: {prompt: '${{ inputs.task }}'}", "uses: action/capture\n        with: {value: '${{ inputs.task }}'}", 1))
			spec := readSpec(&mutations)
			spec.Effect = ActionExternalMutation
			spec.Handler = func(_ context.Context, _ ActionContext, i actionInput) (ActionResult[actionOutput], error) {
				if mutations.Add(1) == 1 {
					return ActionResult[actionOutput]{}, errors.New("lost remote acknowledgement")
				}
				return ActionResult[actionOutput]{Output: actionOutput{i.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}, nil
			}
			spec.Reconcile = func(_ context.Context, _ ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) (ActionReconciliation[actionOutput], error) {
				reconciles.Add(1)
				return ActionReconciliation[actionOutput]{Outcome: outcome, Proof: json.RawMessage(`{"receipt":"checked"}`), Result: ActionResult[actionOutput]{Output: actionOutput{cp.Input.Value}, Evidence: json.RawMessage(`{"digest":"fixture"}`)}}, nil
			}
			registerTestAction(t, e, spec)
			run, err := e.Run(context.Background(), iterationRequest("a"))
			if err == nil {
				t.Fatal("missing mutation acknowledgement accepted")
			}
			run, err = e.Resume(context.Background(), run.ID, nil, true)
			if reconciles.Load() != 1 {
				t.Fatal(reconciles.Load(), err)
			}
			if outcome == "no_effect" || outcome == "completed" {
				want := int32(2)
				if outcome == "completed" {
					want = 1
				}
				if err != nil || mutations.Load() != want {
					t.Fatal(run, err, mutations.Load())
				}
			} else if !errors.Is(err, ErrActionIntegrity) || mutations.Load() != 1 {
				t.Fatal(run, err, mutations.Load())
			}
		})
	}
}
func TestOrderedIterationPlanFencingRetentionAndRedaction(t *testing.T) {
	e, s := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		return map[string]any{"text": "secret-token"}, nil
	}))
	req := iterationRequest("safe")
	req.Secrets = map[string]string{"token": "secret-token"}
	run, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	records, err := s.IterationAttempts(context.Background(), run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "secret-token") {
			t.Fatal("credential retained")
		}
	}
	p, _ := s.IterationPlan(context.Background(), run.ID, "review")
	stale := fenceContext(context.Background(), &Run{ID: run.ID, Generation: run.Generation, Owner: "stale"})
	if err = s.PinIteration(stale, p); err == nil {
		t.Fatal("stale owner pinned plan")
	}
	if err = s.AppendIteration(stale, run.ID, "review", records[len(records)-1]); err == nil {
		t.Fatal("stale owner appended result")
	}
	if err = s.DeleteOrigin(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IterationPlan(context.Background(), run.ID, "review"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	records, err = s.IterationAttempts(context.Background(), run.ID, "review")
	if err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	req = iterationRequest("secret-token")
	req.Secrets = map[string]string{"token": "secret-token"}
	if _, err = e.Run(context.Background(), req); err == nil {
		t.Fatal("credential literal accepted")
	}
}

func TestOrderedIterationReadonlyActionAggregateRefreshes(t *testing.T) {
	retry := false
	var calls atomic.Int32
	e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		if !retry {
			return nil, ErrUsageExhausted
		}
		return map[string]any{"text": "done"}, nil
	}))
	registerTestAction(t, e, readSpec(&calls))
	writeWorkflow(t, e.WorkspaceDir, "main.yml", iterationSource+`  aggregate:
    needs: review
    steps: [{uses: action/capture, with: {value: 'complete=${{ needs.review.outputs.complete }}'}}]
`)
	run, err := e.Run(context.Background(), iterationRequest("a"))
	if err != nil {
		t.Fatal(err)
	}
	retry = true
	run, err = e.Resume(context.Background(), run.ID, nil, true)
	if err != nil || calls.Load() != 2 || run.Jobs["aggregate"].Steps["step_1"].Outputs["value"] != "complete=true" {
		t.Fatal(run, err, calls.Load())
	}
}
func TestOrderedIterationPriorMutationIsNotReplayed(t *testing.T) {
	retry := false
	var mutations atomic.Int32
	var providers atomic.Int32
	e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		providers.Add(1)
		if !retry {
			return nil, ErrProviderFailure
		}
		return map[string]any{"text": "done"}, nil
	}))
	spec := readSpec(&mutations)
	spec.Effect = ActionExternalMutation
	registerTestAction(t, e, spec)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", strings.Replace(iterationChild, "      - id: run", "      - id: mutate\n        uses: action/capture\n        with: {value: stable}\n      - id: run", 1))
	run, err := e.Run(context.Background(), iterationRequest("a"))
	if err == nil {
		t.Fatal("failure hidden")
	}
	retry = true
	run, err = e.Resume(context.Background(), run.ID, nil, true)
	if err != nil || mutations.Load() != 1 || providers.Load() != 2 {
		t.Fatal(run, err, mutations.Load(), providers.Load())
	}
}
func TestOrderedIterationRetainedCorruptionRejected(t *testing.T) {
	for _, target := range []string{"plan", "result", "child", "definition"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int32
			e, s := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
				calls.Add(1)
				return map[string]any{"text": "done"}, nil
			}))
			ctx := context.Background()
			run, err := e.Run(ctx, iterationRequest("a"))
			if err != nil {
				t.Fatal(err)
			}
			child := run.Jobs["review"].Items[0].ChildID
			switch target {
			case "plan":
				_, err = s.db.Exec("UPDATE iteration_plans SET record=json_set(CAST(record AS TEXT),'$.items[0].inputs.task','changed') WHERE run_id=?", run.ID)
			case "result":
				_, err = s.db.Exec("UPDATE iteration_attempts SET record=json_set(CAST(record AS TEXT),'$.outputs.text','changed') WHERE run_id=? AND phase='result'", run.ID)
			case "child":
				_, err = s.db.Exec("UPDATE runs SET record=json_set(CAST(record AS TEXT),'$.outputs.text','changed') WHERE id=?", child)
			case "definition":
				_, err = s.db.Exec("UPDATE runs SET snapshot=json_set(CAST(snapshot AS TEXT),'$.hashes.\"workflows/main.yml\"','changed') WHERE id=?", run.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.Resume(ctx, run.ID, nil, false); err == nil || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}
func TestOrderedIterationRechecksEvidenceAfterCapacityWait(t *testing.T) {
	var providers atomic.Int32
	var guards atomic.Int32
	var changed atomic.Bool
	e, s := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		providers.Add(1)
		return map[string]any{"text": "done"}, nil
	}))
	spec := readSpec(&guards)
	validate := spec.ValidateCheckpoint
	ready := make(chan struct{})
	var once sync.Once
	spec.ValidateCheckpoint = func(ctx context.Context, ac ActionContext, cp ActionCheckpoint[actionInput, actionOutput]) error {
		if changed.Load() {
			return errors.New("evidence changed while queued")
		}
		once.Do(func() { close(ready) })
		return validate(ctx, ac, cp)
	}
	registerTestAction(t, e, spec)
	writeWorkflow(t, e.WorkspaceDir, "main.yml", strings.Replace(iterationSource, "  review:", "  guard:\n    steps: [{uses: action/capture, with: {value: stable}}]\n  review:\n    needs: guard", 1))
	e.Slots <- struct{}{}
	done := make(chan error, 1)
	go func() { _, err := e.Run(context.Background(), iterationRequest("a")); done <- err }()
	<-ready
	// Ensure the provider step has been admitted while it is waiting for capacity.
	admitted := make(chan struct{})
	go func() {
		for {
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM attempts WHERE step_id='run'").Scan(&count); err != nil {
				done <- err
				return
			}
			if count == 1 {
				close(admitted)
				return
			}
			runtime.Gosched()
		}
	}()
	<-admitted
	changed.Store(true)
	<-e.Slots
	if err := <-done; !errors.Is(err, ErrActionIntegrity) || providers.Load() != 0 {
		t.Fatal(err, providers.Load())
	}
}

func TestOrderedIterationNestedResumePreservesCompletedItems(t *testing.T) {
	for _, outer := range []bool{false, true} {
		for _, failure := range []error{ErrUsageExhausted, ErrProviderFailure} {
			t.Run(fmt.Sprint(outer, failure), func(t *testing.T) {
				retry := false
				var calls []string
				e, _ := iterationExecutor(t, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
					calls = append(calls, r.Prompt)
					if r.Prompt == "retry" && !retry {
						return nil, failure
					}
					return map[string]any{"text": r.Prompt}, nil
				}))
				wrapper := `on:
  workflow_call: {}
jobs:
  inner:
    uses: workflows/child.yml
    for-each: {items: [{id: ok, task: ok}, {id: retry, task: retry}], key: item.id}
    with: {task: '${{ item.task }}'}
`
				writeWorkflow(t, e.WorkspaceDir, "wrapper.yml", wrapper)
				root := "jobs:\n  wrapper:\n    uses: workflows/wrapper.yml\n"
				if outer {
					root += "    for-each: {items: [{id: outer}], key: item.id}\n"
				}
				writeWorkflow(t, e.WorkspaceDir, "main.yml", root)
				run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
				if failure == ErrProviderFailure && err == nil {
					t.Fatal("failure hidden")
				}
				if failure == ErrUsageExhausted && err != nil {
					t.Fatal(err)
				}
				retry = true
				run, err = e.Resume(context.Background(), run.ID, nil, true)
				if err != nil || !reflect.DeepEqual(calls, []string{"ok", "retry", "retry"}) || iterationRetry(run) {
					t.Fatal(run, err, calls)
				}
			})
		}
	}
}
func TestOrderedIterationProviderClassificationRetainsReasons(t *testing.T) {
	for _, cause := range []error{fmt.Errorf("%w: authentication failed", ErrProviderFailure), fmt.Errorf("%w: budget limit", ErrUsageExhausted), fmt.Errorf("%w: structured output rejected", ErrProviderFailure)} {
		t.Run(cause.Error(), func(t *testing.T) {
			calls := 0
			e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) { calls++; return nil, cause }))
			run, _ := e.Run(context.Background(), iterationRequest("a", "b"))
			if calls != 2 || run.Jobs["review"].Items[0].Error == "" {
				t.Fatal(run, calls)
			}
		})
	}
}

func TestOrderedIterationCommandCheckpointAcrossPhysicalAttempts(t *testing.T) {
	retry := false
	e, _ := iterationExecutor(t, agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
		if !retry {
			return nil, ErrProviderFailure
		}
		return map[string]any{"text": "done"}, nil
	}))
	path := filepath.Join(e.WorkspaceDir, "command-count")
	command := fmt.Sprintf("        argv: [sh, '-c', 'echo once >> %s']", path)
	writeWorkflow(t, e.WorkspaceDir, "child.yml", strings.Replace(iterationChild, "      - id: run", "      - id: command\n"+command+"\n      - id: run", 1))
	run, err := e.Run(context.Background(), iterationRequest("a"))
	if err == nil {
		t.Fatal("failure hidden")
	}
	retry = true
	run, err = e.Resume(context.Background(), run.ID, nil, true)
	if err != nil {
		t.Fatal(run, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "once\n" {
		t.Fatal(string(data), err)
	}
}
