package clirunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const maxOutput = 16 << 20

type Runner struct {
	versions  sync.Map
	Config    Config
	Workspace string
}

func (r *Runner) UsageProvider(role, model string) (string, string) {
	agent := r.Config.Agents[role]
	if model == "" {
		model = agent.Model
	}
	return agent.Provider, model
}

func (r *Runner) RunAgent(ctx context.Context, req workflows.AgentRequest) (map[string]any, error) {
	agent, ok := r.Config.Agents[req.AgentID]
	if !ok {
		return nil, fmt.Errorf("unknown configured agent %q", req.AgentID)
	}
	turn, cancel, contextErr := turnContext(ctx, agent)
	if contextErr != nil {
		return nil, contextErr
	}
	defer cancel()
	ctx = turn
	if agent.Provider == "claude-sdk" || agent.Provider == "codex-app-server" {
		result, err := r.RunTurn(ctx, workflows.SessionRequest{AgentRequest: req}, nil)
		outputs := result.Outputs
		if outputs == nil {
			outputs = map[string]any{}
		}
		outputs["provider"] = agent.Provider
		outputs["provider_status"] = result.ProviderStatus
		outputs["provider_version"] = result.ProviderVersion
		outputs["session_id"] = result.SessionID
		return outputs, err
	}
	model := agent.Model
	if req.Model != "" {
		model = req.Model
	}
	workspace := r.Workspace
	if agent.Workspace != "" {
		workspace = agent.Workspace
	}
	if req.Workspace != "" {
		workspace = req.Workspace
	}
	command := agent.Command
	var args []string
	switch agent.Provider {
	case "claude", "claude-cli":
		if command == "" {
			command = "claude"
		}
		mode := agent.PermissionMode
		if mode == "" {
			mode = "dontAsk"
		}
		args = []string{"-p", "--output-format", "json", "--no-chrome", "--no-session-persistence", "--permission-mode", mode}
		if model != "" {
			args = append(args, "--model", model)
		}
	case "codex", "codex-cli":
		if command == "" {
			command = "codex"
		}
		sandbox := agent.Sandbox
		if sandbox == "" {
			sandbox = "read-only"
		}
		args = []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "--color", "never", "--sandbox", sandbox}
		if model != "" {
			args = append(args, "-m", model)
		}
	default:
		return nil, fmt.Errorf("unknown provider %q", agent.Provider)
	}
	args = append(args, providerArgs(agent)...)
	if req.RepairOnly {
		return nil, fmt.Errorf("CLI compatibility adapters have no verified tool-free repair mode; use claude-sdk or output.repair-attempts: 0")
	}

	if nativeSchema(req.Output) {
		if agent.Provider == "claude" || agent.Provider == "claude-cli" {
			schema, err := schemaJSON(req.Output.Schema)
			if err != nil {
				return nil, err
			}
			args = append(args, "--json-schema", schema)
		} else {
			path, cleanup, err := r.schemaFile(req.Output.Schema)
			if err != nil {
				return nil, err
			}
			defer cleanup()
			args = append(args, "--output-schema", path)
		}
	}
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Env = append(nativeEnvironment(), providerEnvironment(agent.Authentication.CredentialEnv)...)
	cmd.Env = append(cmd.Env, "MAX_STRUCTURED_OUTPUT_RETRIES=0")
	cmd.Stdin = strings.NewReader(req.Prompt)
	stdout := boundedBuffer{limit: outputLimit(agent)}
	stderr := boundedBuffer{limit: outputLimit(agent)}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	cleanup, superviseErr := runtimeexec.SupervisePolicy(ctx, cmd, agent.Execution.policy())
	if superviseErr != nil {
		return nil, superviseErr
	}
	defer cleanup()
	err := cmd.Run()
	if cmd.ProcessState != nil && (cmd.ProcessState.ExitCode() == 125 || cmd.ProcessState.ExitCode() < 0) {
		return partialOutputs(stdout.String(), stderr.String()), fmt.Errorf("%w: %v", process.ErrCleanup, err)
	}
	if ctx.Err() != nil {
		return partialOutputs(stdout.String(), stderr.String()), ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return partialOutputs(stdout.String(), stderr.String()), fmt.Errorf("agent %s: CLI output exceeds %d bytes", req.AgentID, outputLimit(agent))
	}
	// A nonzero subprocess exit is always a failure, even after partial output.
	if err != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic == "" {
			diagnostic = strings.TrimSpace(stdout.String())
			if len(diagnostic) > 4096 {
				diagnostic = diagnostic[:4096]
			}
		}
		partial := partialOutputs(stdout.String(), stderr.String())
		return partial, fmt.Errorf("agent %s: %w: %s", req.AgentID, err, diagnostic)
	}
	var outputs map[string]any
	if agent.Provider == "claude" || agent.Provider == "claude-cli" {
		outputs, err = parseClaude(stdout.Bytes())
	} else {
		outputs, err = parseCodex(stdout.Bytes())
	}
	if err != nil {
		return partialOutputs(stdout.String(), stderr.String()), err
	}
	outputs["provider"] = agent.Provider
	outputs["model"] = model
	outputs["provider_version"] = r.providerVersion(ctx, agent)
	outputs["provider_status"] = "succeeded"
	return outputs, nil
}

type boundedBuffer struct {
	limit int
	bytes.Buffer
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = maxOutput
	}
	space := limit - b.Len()
	if n > space {
		b.overflow = true
		p = p[:space]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func parseClaude(data []byte) (map[string]any, error) {
	var result struct {
		Type       string         `json:"type"`
		Subtype    string         `json:"subtype"`
		IsError    bool           `json:"is_error"`
		Result     string         `json:"result"`
		Usage      map[string]any `json:"usage"`
		Structured any            `json:"structured_output"`
		Cost       *json.Number   `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("Claude JSON: %w", err)
	}
	if result.IsError || result.Subtype != "" && result.Subtype != "success" {
		return nil, fmt.Errorf("Claude failed (%s): %s", result.Subtype, result.Result)
	}
	if result.Type != "result" {
		return nil, fmt.Errorf("Claude returned no terminal result")
	}
	text := result.Result
	if text == "" && result.Structured != nil {
		b, _ := json.Marshal(result.Structured)
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("Claude returned empty result")
	}
	outputs := map[string]any{"text": text, "response": text, "usage": result.Usage}
	if result.Structured != nil {
		outputs["structured"] = result.Structured
	}
	if result.Cost != nil {
		outputs["cost_usd"] = *result.Cost
	}
	return outputs, nil
}

func parseCodex(data []byte) (map[string]any, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxOutput)
	text := ""
	completed := false
	var usage map[string]any
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Error   json.RawMessage `json:"error"`
			Item    struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("Codex JSONL: %w", err)
		}
		switch event.Type {
		case "turn.failed":
			return nil, fmt.Errorf("Codex turn failed: %s", event.Error)
		case "turn.started":
			completed = false
		case "turn.completed":
			completed = true
			usage = event.Usage
		case "item.completed":
			if event.Item.Type == "agent_message" {
				text = event.Item.Text
			}
			// Transport errors can be retried by the CLI. Terminal turn completion
			// is authoritative; failed exits are checked before parsing.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !completed || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("Codex returned no completed turn with text")
	}
	return map[string]any{"text": text, "response": text, "usage": usage}, nil
}

func (r *Runner) HasAgent(id string) bool { _, ok := r.Config.Agents[id]; return ok }

func (r *Runner) providerVersion(ctx context.Context, a Agent) string {
	cacheKey := a.Provider + "/" + a.Command
	if v, ok := r.versions.Load(cacheKey); ok {
		return v.(string)
	}
	command := a.Command
	if command == "" {
		command = "codex"
		if a.Provider == "claude" || a.Provider == "claude-cli" {
			command = "claude"
		}
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, command, "--version")
	removeProbe, err := prepareVersionProbe(cmd)
	if err != nil {
		return "unavailable"
	}
	defer removeProbe()
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	var cleanup func()
	if a.Execution.NetworkRuntime != "" {
		cleanup, err = runtimeexec.SupervisePolicy(versionCtx, cmd, a.Execution.policy())
	} else {
		cleanup, err = process.Supervise(cmd)
	}
	if err != nil {
		return "unavailable"
	}
	defer cleanup()
	if err = cmd.Run(); err != nil {
		return "unavailable"
	}
	version := strings.TrimSpace(output.String())
	if len(version) > 512 {
		version = version[:512]
	}
	version = workflows.Redact(version, r.SecretValues())
	r.versions.Store(cacheKey, version)
	return version
}

func partialOutputs(stdout, stderr string) map[string]any {
	out := map[string]any{"stdout": stdout, "stderr": stderr}
	var frame map[string]any
	decoder := json.NewDecoder(strings.NewReader(stdout))
	decoder.UseNumber()
	if decoder.Decode(&frame) == nil && frame["type"] == "result" {
		if usage, ok := frame["usage"]; ok {
			out["usage"] = usage
		}
		if cost, ok := frame["total_cost_usd"]; ok {
			out["cost_usd"] = cost
		}
	}
	return out
}
