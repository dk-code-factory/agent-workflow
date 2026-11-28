package workflows

import (
	"context"
	"encoding/json"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

// ExecutionBoundary supplies controlled command effects and checks effect
// admission before runtime discovery. It is scoped to an executor and inherited
// by its children and recovery clones. A nil boundary retains normal execution.
// Implementations must fail closed for effects they cannot control; this is an
// integration seam, not a replacement for structural validation or the journal.
type ExecutionBoundary interface {
	CheckPolicy(runtimeexec.Policy) error
	CheckDefinitions(map[string]*Workflow) error
	RunCommand(context.Context, CommandRequest) (map[string]any, error)
}

// CommandRequest is rendered by the interpreter before reaching the boundary.
// RunID/JobID/StepID/Iteration identify the same durable attempt as execution.
type CommandRequest struct {
	RunID, JobID, StepID string
	Uses                 string
	Iteration            int
	Argv                 []string
	Environment          []string
	Directory            string
	Input                []byte
}

// ContainerExecutionBoundary adds structured helper phases to a command
// boundary. An executor with a boundary never falls back to live containers.
type ContainerExecutionBoundary interface {
	ExecutionBoundary
	RunContainerAction(context.Context, ContainerActionRequest) (ContainerActionResponse, error)
}

// ContainerActionRequest crosses the same durable action journal as a real
// helper, after input validation. The boundary owns every helper phase and must
// never delegate unconfigured calls to a container runtime.
type ContainerActionRequest struct {
	Uses, Phase     string
	Context         ActionContext
	Effect          ActionEffect
	Input, Bindings json.RawMessage
	Result          ActionValue
}

// ContainerActionResponse is the action-v1 protocol response, including the
// declared read-only recovery capabilities.
type ContainerActionResponse = containerResponse
