//go:build linux

package clirunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/dk-code-factory/agent-workflow/internal/buildinfo"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func (r *Runner) codexManagedTurn(ctx context.Context, agent Agent, req workflows.SessionRequest, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
	if err := validateManagedBuildRole(agent); err != nil {
		return workflows.SessionResult{}, err
	}
	return r.codexManagedTransport(ctx, agent, req, emit, agent.ManagedBuild.Identity)
}

// The public entry validates the retained canonical profile before reaching
// this transport. Local package tests can supply a prepared immutable build to
// exercise this identical transport while canonical admission stays disabled.
// There is no public option, environment bypass or alternative agent loop.
func (r *Runner) codexManagedTransport(parent context.Context, agent Agent, req workflows.SessionRequest, emit func(workflows.ProviderEvent), identity *ManagedCodexBuildIdentity) (result workflows.SessionResult, returnErr error) {
	result = workflows.SessionResult{SessionID: req.SessionID, Outputs: map[string]any{"provider": "codex-app-server", "usage_incomplete": true}}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := workflows.CheckManagedCodexAuthority(ctx, req.RunID, req.InvocationID); err != nil {
		return result, err
	}
	workspace := r.Workspace
	if agent.Workspace != "" {
		workspace = agent.Workspace
	}
	if req.Workspace != "" {
		workspace = req.Workspace
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return result, err
	}
	state, err := providerStateDir(r.Workspace, "codex", req)
	if err != nil {
		return result, err
	}
	image, err := openManagedExecutable(identity)
	if err != nil {
		return result, err
	}
	defer image.file.Close()
	argv := append([]string(nil), identity.StartupArgv...)
	argv = append(argv, "--config", "skills.bundled.enabled=false", "--config", "skills.include_instructions=false", "--config", "features.plugins=false")
	cmd := image.command(ctx, argv)
	cmd.Dir = workspace
	cmd.Env = providerEnvironment("")
	cmd.Env = append(cmd.Env, "CODEX_HOME="+state, "HOME="+filepath.Join(state, "home"))
	switch agent.Authentication.Mode {
	case "native":
		if err = referenceNativeCodexAuth(state, agent); err != nil {
			return result, err
		}
	case "api-key":
		key := os.Getenv(agent.Authentication.CredentialEnv)
		if key == "" {
			return result, fmt.Errorf("Codex credential reference unavailable")
		}
		cmd.Env = append(cmd.Env, "OPENAI_API_KEY="+key)
	default:
		return result, fmt.Errorf("managed Codex requires explicit native or api-key authentication")
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
	stop, err := runtimeexec.SuperviseLocalDescriptors(ctx, cmd, agent.Execution.policy(), []int{3})
	if err != nil {
		return result, err
	}
	defer stop()
	control := newCodexControl(stop)
	cmd.Cancel = control.interrupt
	if err = cmd.Start(); err != nil {
		return result, err
	}
	var nativePID int
	var controller atomic.Pointer[codexManagedBatchController]
	var accounting atomic.Pointer[codexManagedAccounting]
	interrupt := func(failure error) { cancel(failure); _ = control.interrupt() }
	rpc := newAppRPC(context.WithoutCancel(ctx), stdout, stdin, func(callbackCtx context.Context, frame rpcFrame) (any, error) {
		if active := controller.Load(); active != nil {
			return active.handle(callbackCtx, frame)
		}
		err := fmt.Errorf("managed empty catalog denies all callbacks")
		interrupt(err)
		return nil, err
	}, func(frame rpcFrame) {
		control.notify(frame)
		if frame.Method == "agentWorkflow/modelCompleted" {
			var witness codexModelWitness
			if active := accounting.Load(); active == nil {
				interrupt(fmt.Errorf("managed completion arrived before thread binding"))
			} else if err := strictCodexDecode(frame.Params, &witness); err != nil {
				interrupt(err)
			} else if err := active.observe(witness); err != nil {
				interrupt(err)
			}
		}
		if active := controller.Load(); active != nil {
			active.notify(frame)
		} else if frame.Method == "agentWorkflow/batchParked" {
			interrupt(fmt.Errorf("empty managed catalog produced a broker batch"))
		}
	})
	control.mu.Lock()
	control.rpc = rpc
	control.mu.Unlock()
	defer func() {
		// Establish loss before cleanup can terminate a healthy process. The
		// native executable is the verified owned child, never a persisted PID.
		cause := context.Cause(ctx)
		if result.ProviderStatus == "" && parent.Err() == nil && (cause == nil || errors.Is(cause, context.Canceled)) && nativePID > 0 {
			if _, statErr := os.Stat(fmt.Sprintf("/proc/%d/exe", nativePID)); os.IsNotExist(statErr) {
				select {
				case <-rpc.done:
					returnErr = errors.Join(returnErr, workflows.ErrManagedCodexProcessLost)
				default:
				}
			}
		}
		if active := controller.Load(); active != nil {
			active.close()
		}
		control.finish(ctx)
		stop()
		rpc.Close()
		waitErr := cmd.Wait()
		if cause := context.Cause(ctx); cause != nil {
			returnErr = errors.Join(returnErr, cause)
		}
		if cmd.ProcessState != nil && (cmd.ProcessState.ExitCode() == 125 || cmd.ProcessState.ExitCode() < 0) {
			returnErr = errors.Join(returnErr, fmt.Errorf("%w: %v", process.ErrCleanup, waitErr))
		}
	}()
	// The guardian is only a launcher. Hash and match the actual owned child
	// image, through its executable fd, before even sending initialize data.
	if nativePID, err = image.verifyGuardianChild(ctx, cmd.Process); err != nil {
		return result, err
	}
	call := func(method string, params any) (json.RawMessage, error) { return rpc.callContext(ctx, method, params) }
	raw, err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "agent-workflow", "version": buildinfo.Version}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err != nil {
		return result, err
	}
	var initialized struct {
		UserAgent string `json:"userAgent"`
	}
	if err = json.Unmarshal(raw, &initialized); err != nil {
		return result, err
	}
	result.ProviderVersion = initialized.UserAgent
	if !strings.Contains(initialized.UserAgent, "agent-workflow-managed-v1/"+identity.BuildID) {
		return result, fmt.Errorf("executed managed build handshake differs from retained identity")
	}
	if err = verifyCodexHandshake(initialized.UserAgent); err != nil {
		return result, err
	}
	if err = rpc.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return result, err
	}
	// Reuse the maintained host skill isolation helper, including its verified
	// listing, per-path disables and final readback, before start or cold resume.
	if err = isolateCodexSkills(rpc, workspace); err != nil {
		return result, err
	}
	catalog := []workflows.ToolDefinition{}
	if req.Tools != nil && req.Tools.Binding != nil {
		catalog = append(catalog, req.Tools.Binding.Catalog()...)
	}
	if len(catalog) > 128 {
		return result, fmt.Errorf("managed broker catalog exceeds 128 tools")
	}
	sort.Slice(catalog, func(i, j int) bool { return workflows.ToolWireName(catalog[i]) < workflows.ToolWireName(catalog[j]) })
	dynamic := []map[string]any{}
	names := []string{}
	for _, tool := range catalog {
		names = append(names, workflows.ToolWireName(tool))
		dynamic = append(dynamic, map[string]any{"name": workflows.ToolWireName(tool), "description": tool.Description, "inputSchema": tool.InputSchema})
	}
	policyRaw, _ := json.Marshal(map[string]any{"build": identity.BuildID, "workspace": workspace, "tools": dynamic})
	policyDigest := sha256.Sum256(policyRaw)
	policy := hex.EncodeToString(policyDigest[:])
	model := agent.Model
	if req.Model != "" {
		model = req.Model
	}
	if model == "" {
		return result, fmt.Errorf("managed Codex accounting requires its admitted model")
	}
	if err = workflows.BeginManagedCodexInvocation(ctx, req.RunID, req.InvocationID, identity.BuildID, policy, model); err != nil {
		return result, err
	}
	params := map[string]any{"cwd": workspace, "approvalPolicy": "never", "sandbox": "read-only", "model": model, "baseInstructions": agent.InstructionText, "agentWorkflowPolicy": map[string]any{"protocol": 1, "buildId": identity.BuildID, "policyId": policy, "brokerTools": names}}
	method := "thread/start"
	if req.SessionID != "" {
		method = "thread/resume"
		params["threadId"] = req.SessionID
	} else {
		params["dynamicTools"] = dynamic
	}
	raw, err = call(method, params)
	if err != nil {
		return result, err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err = json.Unmarshal(raw, &thread); err != nil || thread.Thread.ID == "" {
		return result, fmt.Errorf("managed thread response lacks identity")
	}
	if req.SessionID != "" && thread.Thread.ID != req.SessionID {
		return result, fmt.Errorf("managed cold resume changed original thread")
	}
	result.SessionID = thread.Thread.ID
	control.setThread(result.SessionID)
	if err = workflows.BindManagedCodexInvocation(ctx, req.RunID, req.InvocationID, result.SessionID, ""); err != nil {
		return result, err
	}
	account := &codexManagedAccounting{ctx: ctx, run: req.RunID, invocation: req.InvocationID, thread: result.SessionID, policy: policy, build: identity.BuildID, model: model}
	accounting.Store(account)
	authorize := func(checkCtx context.Context) error {
		if err := workflows.CheckManagedCodexAuthority(checkCtx, req.RunID, req.InvocationID); err != nil {
			return err
		}
		if len(catalog) > 0 {
			return workflows.CheckToolBindingAuthority(checkCtx, req.Tools.Binding, req.RunID, req.InvocationID)
		}
		return nil
	}
	if len(catalog) > 0 {
		active, err := newCodexManagedBatchController(ctx, req.Tools, result.SessionID, req.InvocationID, policy, identity.BuildID, authorize, func(_ context.Context, batch codexParkedBatch) error {
			if err := account.seal(batch); err != nil {
				return err
			}
			return authorize(ctx)
		}, interrupt)
		if err != nil {
			return result, err
		}
		controller.Store(active)
	}
	if emit != nil {
		emit(workflows.ProviderEvent{Type: "session", SessionID: result.SessionID})
	}
	turn := map[string]any{"threadId": result.SessionID, "agentWorkflowInvocation": req.InvocationID, "input": []any{map[string]any{"type": "text", "text": req.Prompt}}}
	if recovery := workflows.ManagedCodexRecoveryFrom(ctx); len(recovery) > 0 {
		var original codexSemanticBatch
		if err = strictCodexDecode(recovery, &original); err != nil {
			return result, err
		}
		if original.ThreadID != result.SessionID || original.InvocationID != req.InvocationID || original.PolicyID != policy || original.BuildID != identity.BuildID {
			return result, fmt.Errorf("original managed recovery policy or session changed")
		}
		turn["input"] = []any{}
		turn["agentWorkflowRecovery"] = json.RawMessage(recovery)
	} else {
		if req.Definition != nil && req.Definition.ReasoningEffort != "" {
			turn["effort"] = req.Definition.ReasoningEffort
		}
		if req.Output != nil {
			turn["outputSchema"] = req.Output.Schema
		}
	}
	raw, err = call("turn/start", turn)
	if err != nil {
		return result, err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err = json.Unmarshal(raw, &started); err != nil || started.Turn.ID == "" {
		return result, fmt.Errorf("managed turn response lacks identity")
	}
	control.setTurn(started.Turn.ID)
	if err = workflows.BindManagedCodexInvocation(ctx, req.RunID, req.InvocationID, result.SessionID, started.Turn.ID); err != nil {
		return result, err
	}
	if err = account.bindStart(started.Turn.ID); err != nil {
		return result, err
	}
	if active := controller.Load(); active != nil {
		if err = active.bindStart(started.Turn.ID); err != nil {
			return result, err
		}
	}
	result.Outputs["session_id"] = result.SessionID
	result.Outputs["provider_version"] = result.ProviderVersion
	text := ""
	for {
		frame, err := rpc.next()
		if err != nil {
			return result, err
		}
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		var payload map[string]any
		if len(frame.Params) > 0 {
			if err = json.Unmarshal(frame.Params, &payload); err != nil {
				return result, err
			}
		}
		if thread, ok := payload["threadId"].(string); ok && thread != result.SessionID {
			continue
		}
		if turn, ok := payload["turnId"].(string); ok && turn != started.Turn.ID {
			continue
		}
		switch frame.Method {
		case "item/completed":
			if payload["threadId"] != result.SessionID || payload["turnId"] != started.Turn.ID {
				return result, fmt.Errorf("managed output omitted original turn identity")
			}
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "agentMessage" {
				text, _ = item["text"].(string)
				result.Outputs["text"] = text
				result.Outputs["response"] = text
			}
		case "turn/completed":
			turn, _ := payload["turn"].(map[string]any)
			if payload["threadId"] != result.SessionID || turn["id"] != started.Turn.ID {
				return result, fmt.Errorf("managed completion omitted original turn identity")
			}
			status, _ := turn["status"].(string)
			result.ProviderStatus = status
			result.Outputs["provider_status"] = status
			if status != "completed" {
				return result, fmt.Errorf("%w: managed terminal status %s", workflows.ErrProviderFailure, status)
			}
			if active := controller.Load(); active != nil {
				active.mu.Lock()
				settled := active.err == nil && active.batch == nil && !active.running && len(active.pending) == 0
				active.mu.Unlock()
				if !settled {
					return result, fmt.Errorf("managed terminal turn has unsettled broker work")
				}
			}
			totals, err := account.totals()
			if err != nil {
				return result, err
			}
			if err = authorize(ctx); err != nil {
				return result, err
			}
			result.Status = workflows.RunStatusSucceeded
			result.Outputs["usage"] = totals
			result.Outputs["usage_incomplete"] = false
			result.Outputs["text"] = text
			result.Outputs["response"] = text
			if err = account.checkpoint(result); err != nil {
				return result, err
			}
			return result, nil
		}
		if emit != nil && frame.Method != "" && frame.Method != "thread/tokenUsage/updated" && frame.Method != "agentWorkflow/modelCompleted" {
			emit(workflows.ProviderEvent{Type: frame.Method, SessionID: result.SessionID})
		}
	}
}
