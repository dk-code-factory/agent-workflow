package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type rpcFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}
type appRPC struct {
	scanner  *bufio.Scanner
	writer   io.Writer
	sequence int
	total    int
	pending  []rpcFrame
	emit     func(workflows.ProviderEvent)
}

func (rpc *appRPC) write(v any) error { return json.NewEncoder(rpc.writer).Encode(v) }
func (rpc *appRPC) next() (rpcFrame, error) {
	var frame rpcFrame
	if !rpc.scanner.Scan() {
		if err := rpc.scanner.Err(); err != nil {
			return frame, err
		}
		return frame, io.ErrUnexpectedEOF
	}
	rpc.total += len(rpc.scanner.Bytes())
	if rpc.total > maxOutput {
		return frame, fmt.Errorf("app-server stream exceeds limit")
	}
	if err := json.Unmarshal(rpc.scanner.Bytes(), &frame); err != nil {
		return frame, fmt.Errorf("invalid app-server frame: %w", err)
	}
	// All unconfigured interactive permissions and dynamic tools fail closed.
	if len(frame.ID) > 0 && frame.Method != "" {
		if err := rpc.write(map[string]any{"id": json.RawMessage(frame.ID), "error": map[string]any{"code": -32601, "message": "engine has no permission handler for this request"}}); err != nil {
			return frame, err
		}
	}
	return frame, nil
}
func (rpc *appRPC) call(method string, params any) (json.RawMessage, error) {
	rpc.sequence++
	id := rpc.sequence
	if err := rpc.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		frame, err := rpc.next()
		if err != nil {
			return nil, err
		}
		if string(frame.ID) == strconv.Itoa(id) && frame.Method == "" {
			if len(frame.Error) > 0 {
				return nil, fmt.Errorf("app-server %s failed: %s", method, frame.Error)
			}
			return frame.Result, nil
		}
		if frame.Method != "" && len(frame.ID) == 0 {
			rpc.pending = append(rpc.pending, frame)
		}
	}
}
func (r *Runner) codexTurn(ctx context.Context, agent Agent, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (result workflows.SessionResult, returnErr error) {
	result = workflows.SessionResult{SessionID: req.SessionID}
	if req.RepairOnly {
		return result, fmt.Errorf("app-server tool-free repair is unverified; use output.repair-attempts: 0")
	}
	workspace := r.Workspace
	if agent.Workspace != "" {
		workspace = agent.Workspace
	}
	if req.Workspace != "" {
		workspace = req.Workspace
	}
	command := agent.Command
	if command == "" {
		command = "codex"
	}
	args := []string{"app-server", "--config", "project_doc_max_bytes=0"}
	args = append(args, agent.Args...)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = providerEnvironment("")
	state := filepath.Join(r.Workspace, ".agent-workflow", "providers", "codex", req.AgentID)
	if err := os.MkdirAll(state, 0700); err != nil {
		return result, err
	}
	if agent.Authentication.Mode == "native" {
		for _, key := range []string{"HOME", "CODEX_HOME"} {
			if v, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+v)
			}
		}
	} else if agent.Authentication.Mode == "api-key" {
		state = filepath.Join(r.Workspace, ".agent-workflow", "providers", "codex", req.AgentID)
		if err := os.MkdirAll(state, 0700); err != nil {
			return result, err
		}
		cmd.Env = append(cmd.Env, "CODEX_HOME="+state)
	} else {
		return result, fmt.Errorf("codex-app-server requires explicit native or api-key authentication")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	cleanup, err := superviseContainer(ctx, cmd, agent, workspace, state, false)
	if err != nil {
		return result, err
	}
	defer cleanup()
	if err = cmd.Start(); err != nil {
		return result, err
	}
	// Always stop the server and reap its guardian after a turn, including bad frames.
	defer func() {
		_ = stdin.Close()
		cleanup()
		waitErr := cmd.Wait()
		if cmd.ProcessState != nil && (cmd.ProcessState.ExitCode() == 125 || cmd.ProcessState.ExitCode() < 0) {
			returnErr = errors.Join(returnErr, fmt.Errorf("%w: %v", process.ErrCleanup, waitErr))
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxOutput)
	rpc := appRPC{scanner: scanner, writer: stdin, emit: emit}
	initializeResult, initErr := rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "agent-workflow", "version": "0.2.0"}, "capabilities": map[string]bool{"experimentalApi": false}})
	if initErr != nil {
		return result, initErr
	}
	var initializeInfo map[string]any
	_ = json.Unmarshal(initializeResult, &initializeInfo)
	result.ProviderVersion, _ = initializeInfo["userAgent"].(string)
	if err = rpc.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return result, err
	}
	if agent.Authentication.Mode == "api-key" {
		key, ok := os.LookupEnv(agent.Authentication.CredentialEnv)
		if !ok || key == "" {
			return result, fmt.Errorf("Codex credential reference unavailable")
		}
		if _, err = rpc.call("account/login/start", map[string]any{"type": "apiKey", "apiKey": key}); err != nil {
			return result, err
		}
	}
	sandbox := agent.Sandbox
	if sandbox == "" || req.RepairOnly {
		sandbox = "read-only"
	}
	model := agent.Model
	if req.Model != "" {
		model = req.Model
	}
	if agent.Execution.Mode == "container" {
		workspace = "/workspace"
	}
	params := map[string]any{"cwd": workspace, "approvalPolicy": "never", "sandbox": sandbox, "baseInstructions": agent.InstructionText}
	if model != "" {
		params["model"] = model
	}
	method := "thread/start"
	if req.SessionID != "" {
		method = "thread/resume"
		params["threadId"] = req.SessionID
	}
	raw, err := rpc.call(method, params)
	if err != nil {
		return result, err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = json.Unmarshal(raw, &thread); err != nil || thread.Thread.ID == "" {
		return result, fmt.Errorf("app-server returned no thread ID")
	}
	if req.SessionID != "" && thread.Thread.ID != req.SessionID {
		return result, fmt.Errorf("app-server returned an unexpected resumed thread identity")
	}
	result.SessionID = thread.Thread.ID
	if emit != nil {
		emit(workflows.ProviderEvent{Type: "session", SessionID: result.SessionID})
	}
	turn := map[string]any{"threadId": result.SessionID, "input": []any{map[string]any{"type": "text", "text": req.Prompt}}}
	if req.Output != nil {
		turn["outputSchema"] = req.Output.Schema
	}
	turnResult, startErr := rpc.call("turn/start", turn)
	if startErr != nil {
		return result, startErr
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = json.Unmarshal(turnResult, &started); err != nil || started.Turn.ID == "" {
		return result, fmt.Errorf("app-server returned no turn identity")
	}
	result.Outputs = map[string]any{"text": "", "response": "", "provider": "codex-app-server", "provider_version": result.ProviderVersion, "session_id": result.SessionID}
	text := ""
	var usage any
	for {
		var frame rpcFrame
		if len(rpc.pending) > 0 {
			frame = rpc.pending[0]
			rpc.pending = rpc.pending[1:]
		} else {
			frame, err = rpc.next()
			if err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, err
			}
		}
		var payload map[string]any
		if len(frame.Params) > 0 {
			if err = json.Unmarshal(frame.Params, &payload); err != nil {
				return result, err
			}
		}
		if identity, ok := payload["threadId"].(string); ok && identity != result.SessionID {
			continue
		}
		if identity, ok := payload["turnId"].(string); ok && identity != started.Turn.ID {
			continue
		}
		if frame.Method == "item/completed" || frame.Method == "turn/completed" {
			if identity, ok := payload["threadId"].(string); !ok || identity == "" {
				return result, fmt.Errorf("app-server notification omitted thread identity")
			}
		}
		if frame.Method == "item/completed" {
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "agentMessage" {
				if s, ok := item["text"].(string); ok {
					text = s
					result.Outputs["text"] = text
					result.Outputs["response"] = text
				}
			}
		}
		if frame.Method == "thread/tokenUsage/updated" {
			usage = payload["tokenUsage"]
			result.Outputs["usage"] = usage
		}
		if frame.Method == "turn/completed" {
			turn, _ := payload["turn"].(map[string]any)
			identity, _ := turn["id"].(string)
			if identity == "" {
				return result, fmt.Errorf("app-server notification omitted turn identity")
			}
			if identity != started.Turn.ID {
				continue
			}
			status, _ := turn["status"].(string)
			result.Status = status
			result.ProviderStatus = status
			result.Outputs["provider_status"] = status
			if status != "completed" {
				cause, _ := json.Marshal(turn["error"])
				return result, fmt.Errorf("Codex terminal status %s: %s", status, cause)
			}
			result.Status = workflows.RunStatusSucceeded
			result.Outputs = map[string]any{"text": text, "response": text, "usage": usage, "provider": "codex-app-server", "provider_status": status, "provider_version": result.ProviderVersion, "session_id": result.SessionID}
			return result, nil
		}
		if emit != nil && frame.Method != "" && len(frame.ID) == 0 {
			emit(workflows.ProviderEvent{Type: frame.Method, SessionID: result.SessionID})
		}
	}
}
