package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Native task fixtures exercise the same durable mailbox path as production.
// Definition names identify each fake's scenario; all provider calls use codex.
type nativeFixtureRunner struct{ AgentRunner }

func (r nativeFixtureRunner) AdmitNativeTask(d *AgentDefinition) (*AgentDefinition, error) {
	c := *d
	return &c, nil
}
func (r nativeFixtureRunner) Capabilities(string) (ProviderCapabilities, error) {
	if sr, ok := r.AgentRunner.(SessionRunner); ok {
		return sr.Capabilities("codex")
	}
	return ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (r nativeFixtureRunner) RunTurn(ctx context.Context, q SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	if q.Definition == nil {
		if sr, ok := r.AgentRunner.(SessionRunner); ok {
			return sr.RunTurn(ctx, q, emit)
		}
	}
	if q.Definition != nil {
		q.AgentID = q.Definition.Name
	}
	id := q.SessionID
	if id == "" {
		id = "fixture-" + q.ParentWorkerID
	}
	emit(ProviderEvent{Type: "session", SessionID: id})
	out, err := r.RunAgent(ctx, q.AgentRequest)
	return SessionResult{SessionID: id, Status: RunStatusSucceeded, Outputs: out}, err
}
func nativeFixtureStore(v reflect.Value) *SQLiteStore {
	if !v.IsValid() {
		return nil
	}
	if v.CanInterface() {
		if s, ok := v.Interface().(*SQLiteStore); ok {
			return s
		}
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil
		}
		return nativeFixtureStore(v.Elem())
	}
	if v.Kind() == reflect.Struct {
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Anonymous {
				if s := nativeFixtureStore(v.Field(i)); s != nil {
					return s
				}
			}
		}
	}
	return nil
}

var nativeFixtureFinalizers sync.Map

func nativeTestExecutor(t *testing.T, e *Executor) *Executor {
	t.Helper()
	hasTask := false
	filepath.WalkDir(filepath.Join(e.WorkspaceDir, "workflows"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".yml") {
			b, _ := os.ReadFile(path)
			hasTask = hasTask || strings.Contains(string(b), "agent/run")
		}
		return nil
	})
	if !hasTask {
		if _, sessions := e.Agents.(SessionRunner); sessions || e.Agents == nil {
			return e
		}
	}
	if _, ok := e.Agents.(*nativeTaskFixture); ok {
		return e
	}
	if e.Agents == nil {
		e.Agents = agentFunc(func(context.Context, AgentRequest) (map[string]any, error) {
			return nil, fmt.Errorf("unexpected fixture execution")
		})
	}
	if e.Workers != nil {
		actual := nativeFixtureStore(reflect.ValueOf(e.Store))
		if actual != nil && actual != e.Workers.Store {
			if old, ok := nativeFixtureFinalizers.Load(e.Workers); ok {
				e.BeforeFinalize = old.(func(context.Context, string) error)
			}
			e.Workers = nil
		} else {
			if e.Slots != nil && e.Slots != e.Workers.Slots {
				e.Workers.Slots = e.Slots
				e.Capacity = &Capacity{Slots: e.Slots}
				e.Workers.Capacity = e.Capacity
			}
			e.Workers.BindTools = e.BindTools
			if e.RoleRunner != nil {
				old := e.RoleRunner
				e.RoleRunner = func(b json.RawMessage) (AgentRunner, error) {
					r, err := old(b)
					if err != nil {
						return nil, err
					}
					if _, ok := r.(NativeTaskRunner); !ok {
						r = nativeFixtureRunner{r}
					}
					return r, nil
				}
			}
			return e
		}
	}
	if e.WorkspaceDir == "" {
		return e
	}
	if _, ok := e.Agents.(NativeTaskRunner); !ok {
		e.Agents = nativeFixtureRunner{e.Agents}
	}
	if e.RoleRunner != nil {
		old := e.RoleRunner
		e.RoleRunner = func(b json.RawMessage) (AgentRunner, error) {
			r, err := old(b)
			if err != nil {
				return nil, err
			}
			if _, ok := r.(NativeTaskRunner); !ok {
				r = nativeFixtureRunner{r}
			}
			return r, nil
		}
	}
	s := nativeFixtureStore(reflect.ValueOf(e.Store))
	if s == nil {
		var err error
		s, err = OpenSQLite(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		e.Store = s
	}
	slots := e.Slots
	if slots == nil {
		n := e.MaxParallel
		if n < 1 {
			n = 4
		}
		slots = make(chan struct{}, n)
		e.Slots = slots
	}
	if e.Capacity == nil {
		e.Capacity = &Capacity{Slots: slots}
	}
	m := &WorkerManager{Store: s, Slots: slots, Capacity: e.Capacity, Factory: func(_ context.Context, run, role string, _ []string) (SessionRunner, []string, map[string]string, error) {
		r := e.Agents
		roleRunner := e.RoleRunner
		if roleRunner == nil {
			if source, ok := e.Agents.(interface {
				RunnerForRoles(json.RawMessage) (AgentRunner, error)
			}); ok {
				roleRunner = source.RunnerForRoles
			}
		}
		if roleRunner != nil {
			_, snap, err := s.Load(context.Background(), run)
			if err != nil {
				return nil, nil, nil, err
			}
			if len(snap.Roles) > 0 {
				r, err = roleRunner(snap.Roles)
				if err != nil {
					return nil, nil, nil, err
				}
			}
		}
		if sr, ok := r.(SessionRunner); ok {
			return sr, nil, nil, nil
		}
		return nativeFixtureRunner{r}, nil, nil, nil
	}}
	m.BindTools = e.BindTools
	if err := m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Workers = m
	previous := e.BeforeFinalize
	saved := previous
	if saved == nil {
		saved = func(context.Context, string) error { return nil }
	}
	nativeFixtureFinalizers.Store(m, saved)
	t.Cleanup(func() { nativeFixtureFinalizers.Delete(m) })
	e.BeforeFinalize = func(c context.Context, id string) error {
		if err := m.StopRun(c, id); err != nil {
			return err
		}
		if previous != nil {
			return previous(c, id)
		}
		return nil
	}
	serveWorkers(t, m)
	return e
}
func writeNativeFixtureDefinitions(t *testing.T, root, name, source string) {
	t.Helper()
	// Fixture YAML files refer only to local TOML definitions. Missing production
	// definitions are tested separately through LoadAgentDefinition.
	for _, field := range strings.FieldsFunc(source, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == ',' || r == '}' || r == '{' || r == '\'' || r == '"'
	}) {
		if !strings.HasSuffix(field, ".toml") || strings.Contains(field, "${") {
			continue
		}
		path := field
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, "workflows", filepath.Dir(name), path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if _, err = os.Stat(path); err == nil {
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0700)
		role := strings.TrimSuffix(filepath.Base(path), ".toml")
		if err = os.WriteFile(path, []byte(fmt.Sprintf("name = %q\ndescription = 'Deterministic fixture'\ndeveloper_instructions = 'Follow the fixture task.'\n", role)), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func (f agentFunc) AdmitNativeTask(d *AgentDefinition) (*AgentDefinition, error) {
	c := *d
	return &c, nil
}
func (r nativeFixtureRunner) AdmittedRoles(ctx context.Context) (json.RawMessage, error) {
	if s, ok := r.AgentRunner.(interface {
		AdmittedRoles(context.Context) (json.RawMessage, error)
	}); ok {
		return s.AdmittedRoles(ctx)
	}
	return nil, nil
}
func (r nativeFixtureRunner) RunnerForRoles(b json.RawMessage) (AgentRunner, error) {
	if s, ok := r.AgentRunner.(interface {
		RunnerForRoles(json.RawMessage) (AgentRunner, error)
	}); ok {
		v, err := s.RunnerForRoles(b)
		if err != nil {
			return nil, err
		}
		return nativeFixtureRunner{v}, nil
	}
	return r, nil
}
func (r nativeFixtureRunner) Preflight(c context.Context, ids []string) error {
	if s, ok := r.AgentRunner.(interface {
		Preflight(context.Context, []string) error
	}); ok {
		return s.Preflight(c, ids)
	}
	return nil
}
func (r nativeFixtureRunner) SecretValues() map[string]string {
	if s, ok := r.AgentRunner.(interface{ SecretValues() map[string]string }); ok {
		return s.SecretValues()
	}
	return nil
}

func (r nativeFixtureRunner) ToolPolicy(role string) ([]ToolGrant, []ToolDefinition) {
	if s, ok := r.AgentRunner.(interface {
		ToolPolicy(string) ([]ToolGrant, []ToolDefinition)
	}); ok {
		return s.ToolPolicy(role)
	}
	return nil, nil
}

func fixtureAgent(role, prompt string) Step {
	return Step{Uses: "agent/run", With: map[string]any{"agent": role + ".toml", "prompt": prompt}}
}
func nativeWorkflowSource(t *testing.T, w *Workflow) string {
	t.Helper()
	b, err := yaml.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func editNativeWorkflow(t *testing.T, source string, edit func(*Workflow)) string {
	t.Helper()
	w, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	edit(w)
	return nativeWorkflowSource(t, w)
}

func TestNativeFixtureReleasesFinalizerRegistry(t *testing.T) {
	var manager *WorkerManager
	t.Run("fixture", func(t *testing.T) {
		executor, _ := durableExecutor(t, "jobs: {task: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: work}}]}}", nil)
		manager = executor.Workers
		if _, ok := nativeFixtureFinalizers.Load(manager); !ok {
			t.Fatal("fixture finalizer was not registered")
		}
	})
	if _, ok := nativeFixtureFinalizers.Load(manager); ok {
		t.Fatal("completed fixture remains in finalizer registry")
	}
}
