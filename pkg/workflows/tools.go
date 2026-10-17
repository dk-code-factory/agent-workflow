package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func ToolWireName(d ToolDefinition) string {
	h := sha256.Sum256([]byte(d.Server + "\x00" + d.Name))
	return "broker_" + hex.EncodeToString(h[:12])
}

// ToolDefinition is pinned in the admitted role snapshot. WireName is stable
// and provider-safe; server/tool names never come from provider-selected owners.
type ToolDefinition struct {
	Server      string          `json:"server" yaml:"server"`
	Name        string          `json:"name" yaml:"name"`
	Description string          `json:"description" yaml:"description"`
	InputSchema json.RawMessage `json:"input_schema" yaml:"-"`
	Fingerprint string          `json:"fingerprint" yaml:"-"`
}

type ToolGrant struct {
	Server string `json:"server" yaml:"server"`
	Tool   string `json:"tool" yaml:"tool"`
}

type ToolRequest struct {
	CallID    string          `json:"call_id"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolBinding interface {
	Catalog() []ToolDefinition
	Call(context.Context, ToolRequest) (json.RawMessage, error)
}

// InvocationTools is controller-created and never deserialized from provider
// arguments. The adapter owns the blocked barrier, and must reacquire the lease
// before returning a tool result that permits provider execution to continue.
type InvocationTools struct {
	Binding ToolBinding
	Lease   *CapacityLease
}

type ToolInvocation struct {
	Owner          string
	Dispatch       ExternalToolDispatcher
	RunID          string
	Generation     int64
	InvocationID   string
	ParentWorkerID string
	Role           string
	Workspace      string
	NativeTools    []string
	Access         WorkerAccess
	Limits         WorkerLimits
	Grants         []ToolGrant
	Catalog        []ToolDefinition
	Secrets        map[string]string
}

type ToolBinder func(context.Context, ToolInvocation) (ToolBinding, error)

func childAccessAllowed(parent, child WorkerAccess) bool {
	return (parent.Cleanup != "strict" || child.Cleanup == "strict") && (!parent.Container || child.Container) && (parent.Writable || !child.Writable) && (parent.Workspace == "" || parent.Workspace == child.Workspace)
}

func scopeWorker(runner SessionRunner, role, workspace string) (SessionRunner, error) {
	if scoped, ok := runner.(interface {
		WithWorkerWorkspace(string, string) (SessionRunner, error)
	}); ok {
		return scoped.WithWorkerWorkspace(role, workspace)
	}
	return runner, nil
}

type failedSessionRunner struct{ err error }

func (r *failedSessionRunner) Capabilities(string) (ProviderCapabilities, error) {
	return ProviderCapabilities{}, r.err
}
func (r *failedSessionRunner) RunTurn(context.Context, SessionRequest, func(ProviderEvent)) (SessionResult, error) {
	return SessionResult{Status: RunStatusFailed}, r.err
}

func grantSubset(child, parent []ToolGrant) bool {
	for _, c := range child {
		found := false
		for _, p := range parent {
			if c == p {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func intersectGrants(role, parent []ToolGrant) []ToolGrant {
	out := make([]ToolGrant, 0)
	for _, g := range role {
		if grantSubset([]ToolGrant{g}, parent) {
			out = append(out, g)
		}
	}
	return out
}
