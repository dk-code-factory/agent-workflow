package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// Every port is owned by one case. No port embeds a live runner or delegates
// execution, credential lookup, discovery, commands or recovery to one.
type fixturePorts struct {
	mu          sync.Mutex
	operations  []FixtureOperation
	used        []bool
	calls       map[string]int
	invocations []FixtureInvocation
	failure     error
	contracts   map[string]string
}
type FixtureInvocation struct {
	Uses    string                    `json:"uses"`
	Phase   string                    `json:"phase"`
	Call    int                       `json:"call"`
	Job     string                    `json:"job,omitempty"`
	Step    string                    `json:"step,omitempty"`
	Prompt  string                    `json:"prompt,omitempty"`
	Role    string                    `json:"role,omitempty"`
	Runtime string                    `json:"runtime,omitempty"`
	Worker  string                    `json:"worker,omitempty"`
	Session string                    `json:"session,omitempty"`
	Command *workflows.CommandRequest `json:"command,omitempty"`
}

func newFixturePorts(operations []FixtureOperation) *fixturePorts {
	return &fixturePorts{operations: operations, used: make([]bool, len(operations)), calls: map[string]int{}}
}
func (p *fixturePorts) fail(err error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failure = errors.Join(p.failure, err)
	return err
}
func (p *fixturePorts) take(q FixtureInvocation) (FixtureOperation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := q.Uses + "/" + q.Phase
	p.calls[key]++
	q.Call = p.calls[key]
	matches := []int{}
	for i, op := range p.operations {
		phase := op.Phase
		if phase == "" {
			phase = "execute"
		}
		if !p.used[i] && op.Uses == q.Uses && phase == q.Phase && (op.Call == 0 || op.Call == q.Call) && (op.Job == "" || op.Job == q.Job) && (op.Step == "" || op.Step == q.Step) && (op.Prompt == "" || op.Prompt == q.Prompt) && (op.Role == "" || op.Role == q.Role) {
			matches = append(matches, i)
		}
	}
	if len(matches) != 1 {
		err := fmt.Errorf("fixture response for %s phase %s call %d (%s/%s): matched %d operations; provide one exact response with call/job/step/prompt selectors", q.Uses, q.Phase, q.Call, q.Job, q.Step, len(matches))
		p.failure = errors.Join(p.failure, err)
		return FixtureOperation{}, err
	}
	i := matches[0]
	p.used[i] = true
	p.invocations = append(p.invocations, q)
	op := p.operations[i]
	if op.Error != "" {
		return op, errors.New(op.Error)
	}
	return op, nil
}
func (p *fixturePorts) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, q := range p.invocations {
		if q.Phase == "execute" {
			n++
		}
	}
	return n
}
func (p *fixturePorts) finish() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.failure
	for i, used := range p.used {
		if !used {
			err = errors.Join(err, fmt.Errorf("unused fixture operation %d (%s)", i+1, p.operations[i].Uses))
		}
	}
	return err
}
func (p *fixturePorts) CheckPolicy(policy runtimeexec.Policy) error {
	if err := policy.Validate(); err != nil {
		return p.fail(err)
	}
	if policy.Mode == "container" || policy.NetworkRuntime != "" || policy.Network != nil {
		return p.fail(fmt.Errorf("fixture mode does not support container or network execution policies; no runtime was discovered"))
	}
	return nil
}
func (p *fixturePorts) CheckDefinitions(all map[string]*workflows.Workflow) error {
	var visit func(string, []workflows.Step) error
	visit = func(ref string, steps []workflows.Step) error {
		for _, s := range steps {
			if strings.HasPrefix(s.Uses, "action/") {
				if _, ok := p.contracts[strings.TrimPrefix(s.Uses, "action/")]; !ok {
					return fmt.Errorf("%s: missing explicit fixture action contract", s.Uses)
				}
			}
			if strings.HasPrefix(s.Uses, "docker://") || s.Protocol != "" || s.ContainerImplementation != nil || len(s.Conversation) > 0 {
				return fmt.Errorf("%s step %s: fixtures do not support containers, helper protocols or conversations; use explicit supported fixture ports", ref, s.ID)
			}
			for _, children := range [][]workflows.Step{s.Sequence, s.Parallel, s.OnResult, s.OnComplete, s.OnIncomplete, s.OnError, s.OnFailure, s.Finally} {
				if err := visit(ref, children); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for ref, w := range all {
		for job, j := range w.Jobs {
			if j.Container != nil {
				return p.fail(fmt.Errorf("%s job %s: fixtures do not support job containers; no container/image/guardian was discovered", ref, job))
			}
			if err := visit(ref, j.Steps); err != nil {
				return p.fail(err)
			}
		}
	}
	return nil
}
func (p *fixturePorts) RunCommand(ctx context.Context, q workflows.CommandRequest) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	op, err := p.take(FixtureInvocation{Uses: "command/run", Phase: "execute", Job: q.JobID, Step: q.StepID, Command: &q})
	out := op.Result
	if out == nil {
		out = map[string]any{}
	}
	for _, name := range []string{"stdout", "stderr"} {
		if out[name] == nil {
			out[name] = ""
		}
		if _, ok := out[name].(string); !ok {
			return nil, p.fail(fmt.Errorf("command fixture %s must be a string", name))
		}
	}
	if out["exit_code"] == nil {
		out["exit_code"] = 0
	}
	out["queue_time_ms"] = 0
	return out, err
}
func (p *fixturePorts) Admission(context.Context, map[string]*workflows.Workflow) (map[string]any, error) {
	return map[string]any{}, nil
}
func (p *fixturePorts) RunAction(ctx context.Context, q workflows.ActionRequest) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	op, err := p.take(FixtureInvocation{Uses: q.Uses, Phase: "execute", Job: q.JobID, Step: q.StepID})
	return op.Result, err
}
func fixtureConfig(provider string) clirunner.Config {
	if provider == "" {
		provider = "codex-app-server"
	}
	role := "codex"
	if provider == "claude-sdk" {
		role = "claude"
	}
	return clirunner.Config{AgentRuntime: role, NativeTaskAdmission: 1, Agents: map[string]clirunner.Agent{role: {Provider: provider}}}
}

type fixtureNative struct {
	ports  *fixturePorts
	config clirunner.Config
}

func (f *fixtureNative) validator() *clirunner.Runner { return &clirunner.Runner{Config: f.config} }
func (f *fixtureNative) NativeTaskRole() string       { return f.config.NativeRole() }
func (f *fixtureNative) PinRoles(context.Context) (json.RawMessage, error) {
	return json.Marshal(f.config)
}
func (f *fixtureNative) AdmitNativeTask(d *workflows.AgentDefinition) (*workflows.AgentDefinition, error) {
	return f.validator().AdmitNativeTask(d)
}
func (f *fixtureNative) AdmitNativeOutput(c *workflows.AgentOutputContract) error {
	return f.validator().AdmitNativeOutput(c)
}
func (f *fixtureNative) ValidateNativeOutput(c *workflows.AgentOutputContract) error {
	return f.validator().ValidateNativeOutput(c)
}
func (f *fixtureNative) ValidateNativeModel(m any) error { return f.validator().ValidateNativeModel(m) }
func (f *fixtureNative) Capabilities(role string) (workflows.ProviderCapabilities, error) {
	if role != f.NativeTaskRole() {
		return workflows.ProviderCapabilities{}, fmt.Errorf("fixture runtime has no role %q", role)
	}
	return workflows.ProviderCapabilities{Sessions: true, Resume: true, ManagedPolicy: true}, nil
}
func (f *fixtureNative) RunAgent(context.Context, workflows.AgentRequest) (map[string]any, error) {
	return nil, f.ports.fail(fmt.Errorf("fixture native tasks must use durable workers"))
}
func (f *fixtureNative) RunTurn(ctx context.Context, q workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if err := ctx.Err(); err != nil {
		return workflows.SessionResult{}, err
	}
	session := q.SessionID
	if session == "" {
		session = "fixture-" + q.ParentWorkerID
	}
	role := f.NativeTaskRole()
	op, err := f.ports.take(FixtureInvocation{Uses: "agent/run", Phase: "execute", Prompt: q.Prompt, Role: q.AgentID, Runtime: f.config.Agents[role].Provider, Worker: q.ParentWorkerID, Session: session})
	emit(workflows.ProviderEvent{Type: "session", SessionID: session})
	status := workflows.RunStatusSucceeded
	if err != nil {
		status = workflows.RunStatusFailed
		err = fmt.Errorf("%w: %v", workflows.ErrProviderFailure, err)
	}
	return workflows.SessionResult{SessionID: session, Status: status, Outputs: op.Result}, err
}
func (p *fixturePorts) registry(contracts map[string]string) (*workflows.ActionRegistry, error) {
	p.contracts = contracts
	registry := &workflows.ActionRegistry{}
	for name, effect := range contracts {
		uses := "action/" + name
		request := func(ac workflows.ActionContext, phase string) FixtureInvocation {
			return FixtureInvocation{Uses: uses, Phase: phase, Job: ac.JobID, Step: ac.StepID}
		}
		err := workflows.RegisterAction[map[string]any, map[string]any](registry, name, workflows.ActionSpec[map[string]any, map[string]any]{
			Implementation: "plugin-fixture-v1", Effect: workflows.ActionEffect(effect),
			Bindings: func(_ context.Context, ac workflows.ActionContext, _ map[string]any) (json.RawMessage, error) {
				op, err := p.take(request(ac, "bindings"))
				data, e := json.Marshal(op.Result)
				return data, errors.Join(err, e)
			},
			Handler: func(_ context.Context, ac workflows.ActionContext, _ map[string]any) (workflows.ActionResult[map[string]any], error) {
				op, err := p.take(request(ac, "execute"))
				evidence, _ := json.Marshal(op.Evidence)
				receipt, _ := json.Marshal(op.Receipt)
				return workflows.ActionResult[map[string]any]{Output: op.Result, Evidence: evidence, Receipt: receipt}, err
			},
			ValidateCheckpoint: func(_ context.Context, ac workflows.ActionContext, _ workflows.ActionCheckpoint[map[string]any, map[string]any]) error {
				_, err := p.take(request(ac, "validate"))
				return err
			},
			Reconcile: func(_ context.Context, ac workflows.ActionContext, _ workflows.ActionCheckpoint[map[string]any, map[string]any]) (workflows.ActionReconciliation[map[string]any], error) {
				op, err := p.take(request(ac, "reconcile"))
				proof, _ := json.Marshal(op.Evidence)
				receipt, _ := json.Marshal(op.Receipt)
				return workflows.ActionReconciliation[map[string]any]{Outcome: op.Outcome, Proof: proof, Result: workflows.ActionResult[map[string]any]{Output: op.Result, Evidence: proof, Receipt: receipt}}, err
			},
		})
		if err != nil {
			return nil, err
		}
	}
	return registry, nil
}
