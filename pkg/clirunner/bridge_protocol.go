package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

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
func bridgeTurn(ctx context.Context, reader io.Reader, writer *bridgeWriter, input map[string]any, tools *workflows.InvocationTools, emit func(workflows.ProviderEvent), stop func()) (workflows.SessionResult, error) {
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	var callbacks sync.WaitGroup
	defer func() { cancel(); stop(); callbacks.Wait() }()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxOutput)
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
		case "native_request":
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
				if errors.Is(err, workflows.ErrUncertainTool) {
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
			stateMu.Lock()
			remaining := len(pending) + len(nativeWork)
			stateMu.Unlock()
			if remaining != 0 {
				return result, fmt.Errorf("provider terminated with unresolved callbacks")
			}
			terminal = true
			result.Status = frame.Status
			result.ProviderStatus = frame.ProviderStatus
			result.ProviderVersion = frame.ProviderVersion
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
		case "usage":
			if result.Outputs == nil {
				result.Outputs = map[string]any{}
			}
			result.Outputs["usage"] = frame.Data["usage"]
			result.Outputs["usage_incomplete"] = true
			if emit != nil {
				emit(workflows.ProviderEvent{Type: frame.Type, SessionID: result.SessionID, Data: frame.Data})
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
