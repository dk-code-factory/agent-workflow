package controller

import (
	"context"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"path/filepath"
	"testing"
)

type nativeAgentFixture struct{ workflows.AgentRunner }

func (r nativeAgentFixture) AdmitNativeTask(d *workflows.AgentDefinition) (*workflows.AgentDefinition, error) {
	c := *d
	return &c, nil
}
func (r nativeAgentFixture) Capabilities(string) (workflows.ProviderCapabilities, error) {
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (r nativeAgentFixture) RunTurn(ctx context.Context, q workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	id := q.SessionID
	if id == "" {
		id = "fixture-" + q.ParentWorkerID
	}
	if emit != nil {
		emit(workflows.ProviderEvent{Type: "session", SessionID: id})
	}
	out, err := r.RunAgent(ctx, q.AgentRequest)
	return workflows.SessionResult{SessionID: id, Status: workflows.RunStatusSucceeded, Outputs: out}, err
}
func (r nativeAgentFixture) Preflight(ctx context.Context, roles []string) error {
	if p, ok := r.AgentRunner.(interface {
		Preflight(context.Context, []string) error
	}); ok {
		return p.Preflight(ctx, roles)
	}
	return nil
}
func nativeControllerFixture(t *testing.T, s *Server) {
	t.Helper()
	for _, name := range []string{"a", "fixture"} {
		if err := os.WriteFile(filepath.Join(s.Executor.WorkspaceDir, "workflows", name+".toml"), []byte("name = '"+name+"'\ndescription = 'Fixture'\ndeveloper_instructions = 'Execute the deterministic fixture.'\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if s.Executor.Agents != nil {
		s.Executor.Agents = nativeAgentFixture{s.Executor.Agents}
	}
	old := s.Workers
	s.Workers = &workflows.WorkerManager{Store: s.Store, Factory: func(ctx context.Context, run, role string, tools []string) (workflows.SessionRunner, []string, map[string]string, error) {
		if s.Executor.Agents != nil {
			return nativeAgentFixture{s.Executor.Agents}, nil, nil, nil
		}
		return old.Factory(ctx, run, role, tools)
	}}
}
