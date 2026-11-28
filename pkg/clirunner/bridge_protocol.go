package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type sdkModelBindingKey struct{}
type sdkModelBinding func(requested, resolved string) error

type bridgeWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *bridgeWriter) send(value map[string]any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	value["version"] = 2
	return json.NewEncoder(w.writer).Encode(value)
}

type bridgeFrame struct {
	Version         int             `json:"version"`
	Type            string          `json:"type"`
	SessionID       string          `json:"session_id"`
	Status          string          `json:"status"`
	ProviderStatus  string          `json:"provider_status"`
	ProviderVersion string          `json:"provider_version"`
	Outputs         map[string]any  `json:"outputs"`
	Error           string          `json:"error"`
	Data            map[string]any  `json:"data"`
	CallID          string          `json:"call_id"`
	CallIDs         []string        `json:"call_ids"`
	Server          string          `json:"server"`
	Tool            string          `json:"tool"`
	Arguments       json.RawMessage `json:"arguments"`
}

// bridgeTurn has exactly one reader. Broker calls run independently, while the
// adapter's blocked barrier serializes suspension with result delivery.
func bridgeTurn(ctx context.Context, reader io.Reader, writer *bridgeWriter, input map[string]any, tools *workflows.InvocationTools, emit func(workflows.ProviderEvent), stop func(), authorizeChild ...func(context.Context, workflows.SkillChildObservation) error) (workflows.SessionResult, error) {
	var child func(context.Context, workflows.SkillChildObservation) error
	if len(authorizeChild) == 1 {
		child = authorizeChild[0]
	}
	return bridgeTurnControlled(ctx, reader, writer, input, tools, emit, stop, child, nil, nil)
}

func bridgeTurnControlled(ctx context.Context, reader io.Reader, writer *bridgeWriter, input map[string]any, tools *workflows.InvocationTools, emit func(workflows.ProviderEvent), stop func(), authorizeChild func(context.Context, workflows.SkillChildObservation) error, observe func(sdkBrokerProof) error, authority func() error, knownTerminal ...func(workflows.SessionResult) error) (workflows.SessionResult, error) {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	var callbacks sync.WaitGroup
	defer func() { cancel(); stop(); callbacks.Wait() }()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxOutput)
	var managed *sdkBrokerBatch
	if input["managed_broker"] == true {
		managed = newSDKBrokerBatch(tools, observe, authority)
	}
	var result workflows.SessionResult
	var stateMu sync.Mutex
	pending := map[string]string{}
	nativeWork := map[string]bool{}
	known := map[string]string{}
	failures := make(chan error, 1)
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
		if parentCtx.Err() == nil {
			stop()
		}
	}
	var negotiated atomic.Bool
	negotiationTimer := time.AfterFunc(5*time.Second, func() {
		if !negotiated.Load() {
			report(fmt.Errorf("bridge protocol negotiation timed out before turn effects"))
		}
	})
	defer negotiationTimer.Stop()
	started := false
	terminal := false
	total := 0
	for scanner.Scan() {
		total += len(scanner.Bytes())
		if total > maxOutput {
			return result, fmt.Errorf("bridge stream exceeds limit")
		}
		var frame bridgeFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return result, fmt.Errorf("invalid bridge frame: %w", err)
		}
		if frame.Version != BridgeProtocolVersion {
			return result, fmt.Errorf("unsupported bridge protocol version %d; protocol 2 required", frame.Version)
		}
		if terminal {
			return result, fmt.Errorf("bridge emitted data after terminal status")
		}
		if !started {
			if frame.Type != "hello" {
				return result, fmt.Errorf("bridge must negotiate before turn effects")
			}
			if frame.ProviderVersion != "claude-agent-sdk@"+ClaudeSDKVersion {
				return result, fmt.Errorf("bridge provider dependency mismatch before turn effects")
			}
			result.ProviderVersion = frame.ProviderVersion
			if emit != nil {
				emit(workflows.ProviderEvent{Type: "provider_identity", Data: map[string]any{"provider": "claude-sdk", "provider_version": frame.ProviderVersion}})
			}
			negotiated.Store(true)
			negotiationTimer.Stop()
			input["type"] = "turn_start"
			if err := writer.send(input); err != nil {
				return result, err
			}
			started = true
			continue
		}
		if frame.SessionID != "" {
			if result.SessionID != "" && frame.SessionID != result.SessionID {
				return result, fmt.Errorf("bridge changed session identity within a turn")
			}
			result.SessionID = frame.SessionID
		}
		switch frame.Type {
		case "hello":
			return result, fmt.Errorf("duplicate bridge negotiation")
		case "broker_model_identity":
			requested, _ := input["model"].(string)
			got, _ := frame.Data["requested"].(string)
			resolved, _ := frame.Data["resolved"].(string)
			if managed == nil || input["model_bound"] == true || got != requested || resolved == "" || len(resolved) > 256 {
				return result, fmt.Errorf("invalid SDK initialization model identity")
			}
			if bind, ok := ctx.Value(sdkModelBindingKey{}).(sdkModelBinding); ok {
				if err := bind(requested, resolved); err != nil {
					return result, err
				}
			}
			input["model"], input["model_bound"] = resolved, true
			if err := writer.send(map[string]any{"type": "model_bound", "model": resolved}); err != nil {
				return result, err
			}
		case "broker_model_start":
			_, bindingRequired := ctx.Value(sdkModelBindingKey{}).(sdkModelBinding)
			if managed == nil || bindingRequired && input["model_bound"] != true {
				return result, fmt.Errorf("unadmitted SDK model start")
			}
			if err := managed.start(frame.Data); err != nil {
				return result, err
			}
		case "broker_batch":
			if managed == nil {
				return result, fmt.Errorf("unadmitted SDK response proof")
			}
			model, _ := input["model"].(string)
			if err := managed.seal(ctx, frame.Data, model, frame.SessionID); err != nil {
				return result, err
			}
		case "broker_attempt_uncertainty":
			return result, fmt.Errorf("original SDK model attempt is uncertain")
		case "native_request":
			if managed != nil {
				return result, fmt.Errorf("native work outside managed broker profile")
			}
			allowed := false
			if native, ok := input["tools"].([]string); ok {
				for _, name := range native {
					if frame.Tool == name {
						allowed = true
						break
					}
				}
			}
			if frame.Tool == "StructuredOutput" && input["schema"] != nil {
				allowed = true
			}
			if !allowed || frame.CallID == "" || len(frame.CallID) > 256 {
				return result, fmt.Errorf("unconfigured native tool permission request")
			}
			if frame.Tool == "Agent" {
				var observation workflows.SkillChildObservation
				raw, err := json.Marshal(frame.Data)
				if err != nil || json.Unmarshal(raw, &observation) != nil || observation.ParentCallID != frame.CallID || observation.Kind != "permit" || authorizeChild == nil {
					return result, fmt.Errorf("native child lacks original durable authorization")
				}
				if err = authorizeChild(ctx, observation); err != nil {
					return result, err
				}
			}
			stateMu.Lock()
			if len(nativeWork) >= 128 {
				stateMu.Unlock()
				return result, fmt.Errorf("native tool concurrency exceeds 128")
			}
			if nativeWork[frame.CallID] {
				stateMu.Unlock()
				return result, fmt.Errorf("duplicate native work identity")
			}
			nativeWork[frame.CallID] = true
			stateMu.Unlock()
			callbacks.Add(1)
			go func(id string) {
				defer callbacks.Done()
				stateMu.Lock()
				defer stateMu.Unlock()
				if tools != nil && tools.Lease != nil {
					if err := tools.Lease.Reacquire(ctx); err != nil {
						report(err)
						return
					}
				}
				if err := writer.send(map[string]any{"type": "native_permit", "call_id": id}); err != nil {
					report(err)
				}
			}(frame.CallID)
		case "native_complete":
			stateMu.Lock()
			if !nativeWork[frame.CallID] {
				stateMu.Unlock()
				return result, fmt.Errorf("unknown native work completion")
			}
			delete(nativeWork, frame.CallID)
			stateMu.Unlock()
		case "tool_request":
			if tools == nil || tools.Binding == nil || tools.Lease == nil {
				return result, fmt.Errorf("provider requested unconfigured broker tool")
			}
			if frame.CallID == "" || len(frame.CallID) > 256 {
				return result, fmt.Errorf("invalid bridge tool call identity")
			}
			request := workflows.ToolRequest{CallID: frame.CallID, Server: frame.Server, Tool: frame.Tool, Arguments: frame.Arguments}
			if managed != nil {
				if err := managed.queue(request); err != nil {
					return result, err
				}
				continue
			}
			identityBytes, _ := json.Marshal(request)
			identity := string(identityBytes)
			stateMu.Lock()
			if prior, ok := known[frame.CallID]; ok && prior != identity {
				stateMu.Unlock()
				return result, fmt.Errorf("bridge tool identity reused with conflicting content")
			}
			if _, ok := pending[frame.CallID]; ok {
				stateMu.Unlock()
				continue
			}
			if len(pending) >= 128 {
				stateMu.Unlock()
				return result, fmt.Errorf("bridge callback concurrency exceeds 128")
			}
			known[frame.CallID] = identity
			pending[frame.CallID] = identity
			stateMu.Unlock()
			callbacks.Add(1)
			go func(request workflows.ToolRequest) {
				defer callbacks.Done()
				value, err := tools.Binding.Call(ctx, request)
				if err != nil && !workflows.KnownToolDenial(ctx, tools.Binding, request, err) {
					report(err)
					return
				}
				stateMu.Lock()
				defer stateMu.Unlock()
				// Holding this lock prevents a later barrier from suspending the
				// lease between reacquisition and the result that resumes SDK work.
				if acquireErr := tools.Lease.Reacquire(ctx); acquireErr != nil {
					report(acquireErr)
					return
				}
				response := map[string]any{"type": "tool_result", "call_id": request.CallID}
				if err != nil {
					response["error"] = err.Error()
				} else {
					response["result"] = value
				}
				if writeErr := writer.send(response); writeErr != nil {
					report(writeErr)
					return
				}
				delete(pending, request.CallID)
			}(request)
		case "blocked":
			if managed != nil {
				if err := managed.blocked(ctx, frame, writer, &callbacks, report); err != nil {
					return result, err
				}
				continue
			}
			if tools == nil || tools.Lease == nil {
				return result, fmt.Errorf("unconfigured provider wait barrier")
			}
			stateMu.Lock()
			if len(nativeWork) > 0 {
				stateMu.Unlock()
				return result, fmt.Errorf("provider declared quiescence with native work still active")
			}
			ids := map[string]bool{}
			for _, id := range frame.CallIDs {
				if _, ok := known[id]; !ok {
					stateMu.Unlock()
					return result, fmt.Errorf("wait barrier names an unknown callback")
				}
				ids[id] = true
			}
			for id := range pending {
				if !ids[id] {
					stateMu.Unlock()
					return result, fmt.Errorf("provider wait barrier omitted runnable callback")
				}
			}
			if len(pending) > 0 {
				if err := tools.Lease.Suspend(); err != nil {
					stateMu.Unlock()
					return result, err
				}
			}
			stateMu.Unlock()
		case "result":
			if managed != nil {
				if err := managed.complete(frame.Outputs); err != nil {
					return result, err
				}
			}
			stateMu.Lock()
			remaining := len(pending) + len(nativeWork)
			stateMu.Unlock()
			if remaining != 0 {
				return result, fmt.Errorf("provider terminated with unresolved callbacks")
			}
			terminal = true
			result.Status = frame.Status
			result.ProviderStatus = frame.ProviderStatus
			if frame.ProviderVersion != "" {
				if frame.ProviderVersion != result.ProviderVersion {
					return result, fmt.Errorf("bridge provider identity changed during turn")
				}
				result.ProviderVersion = frame.ProviderVersion
			}
			partial := result.Outputs
			result.Outputs = frame.Outputs
			if partial != nil {
				if result.Outputs == nil {
					result.Outputs = partial
				} else if _, ok := result.Outputs["usage"]; !ok {
					result.Outputs["usage"] = partial["usage"]
					result.Outputs["usage_incomplete"] = true
				}
			}
			if frame.Status != workflows.RunStatusSucceeded {
				return result, fmt.Errorf("provider failed: %s", frame.Error)
			}
			if managed != nil && len(knownTerminal) == 1 {
				// A real terminal's known charge survives a later EOF, cleanup or
				// cancellation failure. This callback grants no settlement authority.
				if err := knownTerminal[0](result); err != nil {
					return result, err
				}
			}
		case "usage":
			if result.Outputs == nil {
				result.Outputs = map[string]any{}
			}
			result.Outputs["usage"] = frame.Data["usage"]
			result.Outputs["usage_incomplete"] = true
			if emit != nil {
				emit(workflows.ProviderEvent{Type: frame.Type, SessionID: result.SessionID, Data: frame.Data})
			}
		case "skill_evidence", "skill_child_evidence", "skill_attempt_uncertainty":
			if input["skill"] == nil {
				return result, fmt.Errorf("unadmitted skill evidence")
			}
			if emit != nil {
				emit(workflows.ProviderEvent{Type: frame.Type, SessionID: frame.SessionID, Data: frame.Data})
			}
		case "session", "progress", "interrupted":
			if emit != nil {
				emit(workflows.ProviderEvent{Type: frame.Type, SessionID: result.SessionID, Data: frame.Data})
			}
		default:
			return result, fmt.Errorf("unknown bridge protocol frame %q", frame.Type)
		}
		select {
		case err := <-failures:
			return result, err
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	select {
	case err := <-failures:
		return result, err
	default:
	}
	if !terminal {
		return result, fmt.Errorf("bridge returned no terminal status")
	}
	return result, nil
}
