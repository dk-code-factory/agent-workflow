package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type FixtureResult struct {
	File            string              `json:"file"`
	Name            string              `json:"name"`
	Passed          bool                `json:"passed"`
	State           string              `json:"state,omitempty"`
	InvocationCount int                 `json:"invocation-count"`
	Invocations     []FixtureInvocation `json:"invocations"`
	Outputs         map[string]any      `json:"outputs,omitempty"`
	WorkerRemaining []int               `json:"worker-remaining,omitempty"`
}
type TestReport struct {
	CheckReport
	Evidence string          `json:"evidence"`
	Cases    []FixtureResult `json:"cases"`
}

// Test executes fixture-backed control flow in disposable engine-owned state.
// It never loads workspace configuration, credentials or host settings. A case
// has ten seconds; the suite has two minutes, including all recovery controls.
func Test(ctx context.Context, root string) TestReport {
	captured, structure := checkCaptured(root)
	report := TestReport{CheckReport: structure, Evidence: "fixture-control-flow", Cases: []FixtureResult{}}
	report.Kind = "fixtures"
	if !report.Passed {
		return report
	}
	report.Passed = false
	cases, err := readFixtures(root, captured)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "fixture.invalid", File: "fixtures", Reason: err.Error()})
		return report
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, source := range cases {
		result, err := runFixtureCase(ctx, captured, source)
		report.Cases = append(report.Cases, result)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: "fixture.failed", File: source.file, Case: source.test.Name, Reason: err.Error()})
		}
		if ctx.Err() != nil {
			break
		}
	}
	report.Passed = len(report.Diagnostics) == 0 && len(report.Cases) == len(cases)
	return report
}

type fixtureEngine struct {
	store    *workflows.SQLiteStore
	executor *workflows.Executor
	stop     context.CancelFunc
	done     chan struct{}
	dir      string
	ports    *fixturePorts
}

func (f *fixtureEngine) start(ctx context.Context) error {
	store, err := workflows.OpenSQLite(filepath.Join(f.dir, "state"))
	if err != nil {
		return err
	}
	f.store = store
	if err = store.Recover(ctx); err != nil {
		store.Close()
		return err
	}
	e := f.executor
	e.Store = store
	m := &workflows.WorkerManager{Store: store, Slots: e.Slots, Capacity: e.Capacity, Limits: workflows.WorkerLimits{MaxWorkers: 32, MaxTurns: 32, MaxDepth: 8, MaxQueued: 32, TurnDuration: 5 * time.Second, MaxOutput: 1 << 20}}
	m.Factory = func(ctx context.Context, run, role string, tools []string) (workflows.SessionRunner, []string, map[string]string, error) {
		if len(tools) > 0 {
			return nil, nil, nil, f.ports.fail(fmt.Errorf("fixture workers cannot grant tools"))
		}
		_, snap, err := store.Load(ctx, run)
		if err != nil {
			return nil, nil, nil, err
		}
		runner, err := f.restore(snap.Roles)
		if err != nil {
			return nil, nil, nil, err
		}
		if role != runner.NativeTaskRole() {
			return nil, nil, nil, f.ports.fail(fmt.Errorf("fixture worker runtime role changed"))
		}
		return runner, nil, nil, nil
	}
	if err = m.Init(ctx); err != nil {
		store.Close()
		return err
	}
	e.Workers = m
	e.BeforeFinalize = m.StopRun
	serveCtx, stop := context.WithCancel(ctx)
	f.stop = stop
	f.done = make(chan struct{})
	go func() { defer close(f.done); m.Serve(serveCtx) }()
	return nil
}
func (f *fixtureEngine) restore(raw json.RawMessage) (*fixtureNative, error) {
	var cfg clirunner.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	// Only the runner's own synthetic snapshots are accepted. No current host or
	// provider environment can add an execution path during recovery.
	provider := cfg.Agents[cfg.NativeRole()].Provider
	if !fixtureRuntime(provider) || provider == "" {
		return nil, f.ports.fail(fmt.Errorf("unsupported retained fixture runtime"))
	}
	expected, _ := json.Marshal(fixtureConfig(provider))
	actual, _ := json.Marshal(cfg)
	if !bytes.Equal(actual, expected) {
		return nil, f.ports.fail(fmt.Errorf("retained fixture runtime configuration changed"))
	}
	return &fixtureNative{ports: f.ports, config: cfg}, nil
}
func (f *fixtureEngine) close() error {
	if f.stop != nil {
		f.stop()
		select {
		case <-f.done:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("fixture worker cleanup exceeded two seconds")
		}
		f.stop = nil
	}
	if f.store != nil {
		err := f.store.Close()
		f.store = nil
		return err
	}
	return nil
}
func runFixtureCase(parent context.Context, captured *CapturedPackage, source fixtureSource) (result FixtureResult, err error) {
	test := source.test
	result = FixtureResult{File: source.file, Name: test.Name, Invocations: []FixtureInvocation{}}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "aw-fixture-")
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, removeFixtureDirectory(dir)); result.Passed = err == nil }()
	workspace := filepath.Join(dir, "workspace")
	if err = os.Mkdir(workspace, 0700); err != nil {
		return result, err
	}
	ports := newFixturePorts(test.Operations)
	defer func() {
		result.InvocationCount = ports.count()
		ports.mu.Lock()
		result.Invocations = append([]FixtureInvocation{}, ports.invocations...)
		ports.mu.Unlock()
		err = errors.Join(err, ports.finish())
	}()
	registry, err := ports.registry(test.Actions)
	if err != nil {
		return result, err
	}
	e := &workflows.Executor{WorkspaceDir: workspace, MaxParallel: 1, Slots: make(chan struct{}, 1), Agents: &fixtureNative{ports: ports, config: fixtureConfig(test.Runtime)}, Actions: ports, ApplicationActions: registry, Boundary: ports}
	e.Capacity = &workflows.Capacity{Slots: e.Slots}
	engine := &fixtureEngine{dir: dir, executor: e, ports: ports}
	e.RoleRunner = func(raw json.RawMessage) (workflows.AgentRunner, error) { return engine.restore(raw) }
	e.ActionFactory = func(json.RawMessage) (workflows.ActionRunner, error) { return ports, nil }
	if err = engine.start(ctx); err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, engine.close()) }()
	admission, err := captured.Admission(test.Skill)
	if err != nil {
		return result, err
	}
	run, runErr := e.Prepare(ctx, workflows.RunRequest{Ref: admission.Binding.Workflow, Package: admission, Inputs: test.Inputs, RequestID: "fixture:" + test.Name})
	var snapshot []byte
	if runErr == nil {
		_, snap, loadErr := engine.store.Load(ctx, run.ID)
		if loadErr != nil {
			return result, loadErr
		}
		snapshot, _ = json.Marshal(snap)
		run, runErr = e.Resume(ctx, run.ID, nil, false)
	}
	for i, control := range test.Controls {
		if run == nil {
			return result, fmt.Errorf("control %d requires an admitted run: %v", i+1, runErr)
		}
		id := run.ID
		var controlErr error
		switch control.Action {
		case "answer":
			gates, e := engine.store.Inputs(ctx, id)
			controlErr = e
			if e == nil {
				var matching []workflows.ExternalInputGate
				for _, gate := range gates {
					if gate.StepID == control.Step {
						matching = append(matching, gate)
					}
				}
				if len(matching) != 1 {
					return result, fmt.Errorf("control %d: answer step %q matches %d gates", i+1, control.Step, len(matching))
				}
				gate := matching[0]
				evidence := control.Evidence
				if evidence == "" || evidence == "current" {
					evidence = gate.EvidenceID
				}
				answerID := control.AnswerID
				if answerID == "" {
					answerID = fmt.Sprintf("fixture-answer-%d", i+1)
				}
				value, _ := json.Marshal(control.Value)
				_, controlErr = engine.store.AcceptInput(ctx, id, gate.ID, workflows.ExternalInputAnswer{AnswerID: answerID, EvidenceID: evidence, Value: value})
			}
		case "resume":
			run, controlErr = e.Resume(ctx, id, nil, control.Retry)
			runErr = controlErr
			if run == nil {
				run, _, _ = engine.store.Load(ctx, id)
			}
		case "interrupt":
			controlErr = engine.store.InterruptInput(ctx, id, run.Generation)
			run, _, _ = engine.store.Load(ctx, id)
		case "reopen":
			controlErr = engine.close()
			if controlErr == nil {
				controlErr = engine.start(ctx)
			}
			if controlErr == nil {
				run, _, controlErr = engine.store.Load(ctx, id)
			}
		case "configure-runtime":
			e.Agents = &fixtureNative{ports: ports, config: fixtureConfig(control.Runtime)}
		}
		if err := matchFixtureError(controlErr, control.ExpectError); err != nil {
			return result, fmt.Errorf("control %d %s: %w", i+1, control.Action, err)
		}
		if control.ExpectState != "" && (run == nil || run.Status != control.ExpectState) {
			return result, fmt.Errorf("control %d: expected state %s; got %+v", i+1, control.ExpectState, run)
		}
		if control.InvocationCount != nil && ports.count() != *control.InvocationCount {
			return result, fmt.Errorf("control %d: expected %d invocations, got %d (state %s: %v)", i+1, *control.InvocationCount, ports.count(), run.Status, runErr)
		}
	}
	result.InvocationCount = ports.count()
	ports.mu.Lock()
	result.Invocations = append(result.Invocations, ports.invocations...)
	ports.mu.Unlock()

	if run != nil {
		result.State, result.Outputs = run.Status, run.Outputs
		workers, workerErr := e.Workers.List(ctx, run.ID)
		err = errors.Join(err, workerErr)
		sessions := map[string]bool{}
		for _, w := range workers {
			result.WorkerRemaining = append(result.WorkerRemaining, w.Remaining)
			if w.SessionID != "" {
				sessions[w.SessionID] = true
			}
		}
		slices.Sort(result.WorkerRemaining)
		if test.Expect.Sessions > 0 && len(sessions) != test.Expect.Sessions {
			err = errors.Join(err, fmt.Errorf("expected %d retained sessions; got %d", test.Expect.Sessions, len(sessions)))
		}
		_, snap, loadErr := engine.store.Load(ctx, run.ID)
		err = errors.Join(err, loadErr)
		after, _ := json.Marshal(snap)
		if !bytes.Equal(snapshot, after) {
			err = errors.Join(err, fmt.Errorf("admitted definitions, package identity or runtime bindings changed during execution/recovery"))
		}
	}
	if test.Expect.State != "" && result.State != test.Expect.State {
		err = errors.Join(err, fmt.Errorf("expected state %s; got %q", test.Expect.State, result.State))
	}
	err = errors.Join(err, matchFixtureError(runErr, test.Expect.Error))
	if result.InvocationCount != *test.Expect.InvocationCount {
		err = errors.Join(err, fmt.Errorf("expected %d invocations; got %d", *test.Expect.InvocationCount, result.InvocationCount))
	}
	if test.Expect.Outputs != nil && !sameFixtureJSON(test.Expect.Outputs, result.Outputs) {
		err = errors.Join(err, fmt.Errorf("workflow outputs differ: expected %v; got %v", test.Expect.Outputs, result.Outputs))
	}
	if test.Expect.WorkerRemaining != nil {
		want := slices.Clone(test.Expect.WorkerRemaining)
		slices.Sort(want)
		if !slices.Equal(want, result.WorkerRemaining) {
			err = errors.Join(err, fmt.Errorf("worker allowances differ: expected %v; got %v", want, result.WorkerRemaining))
		}
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	result.Passed = err == nil
	return result, err
}
func matchFixtureError(err error, want string) error {
	if want == "" {
		return err
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		return fmt.Errorf("expected error containing %q; got %v", want, err)
	}
	return nil
}
func sameFixtureJSON(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

// Engine asset directories are intentionally read-only. Restore permissions only
// within this case-owned tree before removal; never chmod a source/workspace.
func removeFixtureDirectory(root string) error {
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(name, 0700)
		}
		return nil
	})
	return errors.Join(err, os.RemoveAll(root))
}
