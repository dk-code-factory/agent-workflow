package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/dkropachev/agent-workflow/internal/buildinfo"
	"github.com/dkropachev/agent-workflow/pkg/process"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

func (r *Runner) codexTurn(ctx context.Context, agent Agent, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (result workflows.SessionResult, returnErr error) {
	result = workflows.SessionResult{SessionID: req.SessionID}
	if len(agent.Args) > 0 {
		return result, fmt.Errorf("app-server raw arguments are unsupported; use explicit managed fields")
	}
	if req.RepairOnly {
		return result, fmt.Errorf("app-server tool-free repair is unverified; use output.repair-attempts: 0")
	}
	hasBrokerTools := req.Tools != nil && req.Tools.Binding != nil && len(req.Tools.Binding.Catalog()) > 0
	if hasBrokerTools || agent.Tools != nil {
		return result, fmt.Errorf("Codex managed tool subsets are not certified for upstream 0.161.0")
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
	args := []string{"app-server", "--config", "project_doc_max_bytes=0", "--config", `cli_auth_credentials_store="file"`}
	args = append(args, agent.Args...)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = providerEnvironment("")
	state, err := providerStateDir(r.Workspace, "codex", req)
	if err != nil {
		return result, err
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+state, "HOME="+filepath.Join(state, "home"))
	if agent.Authentication.Mode == "native" {
		err = referenceNativeCodexAuth(state, agent)
		if err != nil {
			return result, err
		}
	} else if agent.Authentication.Mode != "api-key" {
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
	control := newCodexControl(cleanup)
	cmd.Cancel = control.interrupt
	if err = cmd.Start(); err != nil {
		return result, err
	}
	rpc := newAppRPC(context.WithoutCancel(ctx), stdout, stdin, codexRequestHandler(agent, req, control), control.notify)
	control.mu.Lock()
	control.rpc = rpc
	control.mu.Unlock()
	defer func() {
		control.finish(ctx)
		cleanup()
		rpc.Close()
		waitErr := cmd.Wait()
		if cmd.ProcessState != nil && (cmd.ProcessState.ExitCode() == 125 || cmd.ProcessState.ExitCode() < 0) {
			returnErr = errors.Join(returnErr, fmt.Errorf("%w: %v", process.ErrCleanup, waitErr))
		}
		if ctx.Err() != nil {
			returnErr = errors.Join(returnErr, ctx.Err())
		}
	}()
	initializeResult, initErr := rpc.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "agent-workflow", "version": buildinfo.Version}, "capabilities": map[string]bool{"experimentalApi": hasBrokerTools}})
	if initErr != nil {
		return result, initErr
	}
	var initializeInfo map[string]any
	_ = json.Unmarshal(initializeResult, &initializeInfo)
	result.ProviderVersion, _ = initializeInfo["userAgent"].(string)
	if err = verifyCodexHandshake(result.ProviderVersion); err != nil {
		return result, err
	}
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
	if hasBrokerTools {
		dynamic := []map[string]any{}
		for _, d := range req.Tools.Binding.Catalog() {
			dynamic = append(dynamic, map[string]any{"type": "function", "name": workflows.ToolWireName(d), "description": d.Description, "inputSchema": d.InputSchema, "deferLoading": false})
		}
		params["dynamicTools"] = dynamic
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
	control.setThread(result.SessionID)
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
	control.setTurn(started.Turn.ID)
	result.Outputs = map[string]any{"text": "", "response": "", "provider": "codex-app-server", "provider_version": result.ProviderVersion, "session_id": result.SessionID}
	text := ""
	var usage any
	for {
		frame, readErr := rpc.next()
		if readErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, readErr
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
			result.Outputs["usage_incomplete"] = true
			if emit != nil {
				emit(workflows.ProviderEvent{Type: "usage", SessionID: result.SessionID, Data: map[string]any{"usage": usage, "observation_key": "turn"}})
			}
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
