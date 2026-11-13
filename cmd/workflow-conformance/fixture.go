package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/engine"
	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

type turnInput struct {
	Role       string `json:"role"`
	Prompt     string `json:"prompt"`
	Structured bool   `json:"structured"`
}
type turnOutput struct {
	Text       string `json:"text"`
	Provider   string `json:"provider"`
	Version    string `json:"provider_version"`
	Invocation string `json:"invocation_id"`
}
type observedInvocation struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Version  string `json:"version"`
}

func structuredContract() *workflows.AgentOutputContract {
	return &workflows.AgentOutputContract{Format: "json", Schema: map[string]any{"type": "object", "required": []any{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean", "const": true}}, "additionalProperties": false}}
}

// The adapter records physical provider identity and usage independently of
// action success. Credentials and provider response bodies stay in private state.
func providerTurn(ctx context.Context, store *workflows.SQLiteStore, runner *clirunner.Runner, req workflows.AgentRequest) (map[string]any, error) {
	provider, model := runner.UsageProvider(req.AgentID, req.Model)
	identity := workflows.UsageIdentity{ID: req.InvocationID, RunID: req.RunID, Kind: "step", Provider: provider, Model: model}
	if err := store.BeginUsage(ctx, identity); err != nil {
		return nil, err
	}
	req.ObserveUsage = func(update workflows.UsageUpdate) error {
		return store.ObserveUsage(context.WithoutCancel(ctx), identity, update)
	}
	var out map[string]any
	var err error
	if provider == "claude-sdk" {
		result, callErr := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: req}, func(event workflows.ProviderEvent) {
			if event.Type == "provider_identity" {
				version, _ := event.Data["provider_version"].(string)
				_ = saveObservation(store.Dir, observedInvocation{req.InvocationID, provider, version})
			}
		})
		out, err = result.Outputs, callErr
		if out == nil {
			out = map[string]any{}
		}
		out["provider"], out["provider_version"] = provider, result.ProviderVersion
	} else {
		out, err = runner.RunAgent(ctx, req)
	}
	usage, _ := out["usage"].(map[string]any)
	incomplete, _ := out["usage_incomplete"].(bool)
	recordErr := store.ObserveUsage(context.WithoutCancel(ctx), identity, workflows.UsageUpdate{Key: "final", Final: true, Incomplete: incomplete || err != nil, Provider: provider, Model: model, Usage: usage, CostUSD: out["cost_usd"]})
	return out, errors.Join(err, recordErr)
}

func fixtureRegistry(store *workflows.SQLiteStore, cfg clirunner.Config, root string, binder workflows.ToolBinder, capacity *workflows.Capacity) (*workflows.ActionRegistry, error) {
	registry := &workflows.ActionRegistry{}
	err := workflows.RegisterAction(registry, "conformance-turn", workflows.ActionSpec[turnInput, turnOutput]{
		Implementation: "conformance-turn-v1", Effect: workflows.ActionExternalMutation,
		Bindings: func(_ context.Context, _ workflows.ActionContext, in turnInput) (json.RawMessage, error) {
			a, ok := cfg.Agents[in.Role]
			if !ok || a.Provider != "claude-sdk" {
				return nil, fmt.Errorf("conformance action requires an admitted Claude SDK role")
			}
			return json.Marshal(map[string]string{"provider": a.Provider, "role": in.Role})
		},
		ValidateCheckpoint: func(context.Context, workflows.ActionContext, workflows.ActionCheckpoint[turnInput, turnOutput]) error {
			return nil
		},
		Handler: func(ctx context.Context, ac workflows.ActionContext, in turnInput) (workflows.ActionResult[turnOutput], error) {
			var result workflows.ActionResult[turnOutput]
			_, snapshot, err := store.Load(ctx, ac.RunID)
			if err != nil {
				return result, err
			}
			var pinned clirunner.Config
			if err = json.Unmarshal(snapshot.Roles, &pinned); err != nil {
				return result, err
			}
			runner := &clirunner.Runner{Config: pinned, Workspace: root}
			lease, err := capacity.Acquire(ctx)
			if err != nil {
				return result, err
			}
			defer lease.Release()
			if err := runner.Preflight(ctx, []string{in.Role}); err != nil {
				return result, err
			}
			run, _, err := store.Load(ctx, ac.RunID)
			if err != nil {
				return result, err
			}
			invocation := runtimeexec.NewID()
			ctx = runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: run.ID, Owner: run.Owner, Generation: run.Generation, LogicalID: invocation})
			bound, err := binder(ctx, workflows.ToolInvocation{RunID: run.ID, Generation: run.Generation, Owner: run.Owner, InvocationID: invocation, Role: in.Role, Workspace: root})
			if err != nil {
				return result, err
			}
			req := workflows.AgentRequest{AgentID: in.Role, RunID: run.ID, Generation: run.Generation, InvocationID: invocation, Prompt: in.Prompt, Tools: &workflows.InvocationTools{Binding: bound, Lease: lease}}
			if in.Structured {
				req.Output = structuredContract()
			}
			out, err := providerTurn(ctx, store, runner, req)
			if err != nil {
				return result, err
			}
			if req.Output != nil && !workflows.ValidateAgentStructuredOutput(fmt.Sprint(out["text"]), req.Output).Valid {
				return result, fmt.Errorf("structured schema was not satisfied")
			}
			result.Output = turnOutput{Text: fmt.Sprint(out["text"]), Provider: fmt.Sprint(out["provider"]), Version: fmt.Sprint(out["provider_version"]), Invocation: invocation}
			return result, nil
		},
	})
	return registry, err
}

// Only the conformance executable registers fixture actions. Production's
// controller, worker factory, broker, capacity and native task adapter are reused.
func serveFixture(ctx context.Context, root, configuration string) error {
	cfg, err := clirunner.LoadConfig(configuration)
	if err != nil {
		return err
	}
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		return err
	}
	defer store.Close()
	executor, workers := engine.Executor(cfg, store, root, 1)
	executor.Capacity = &workflows.Capacity{Slots: make(chan struct{}, 1)}
	executor.ApplicationActions, err = fixtureRegistry(store, cfg, root, executor.BindTools, executor.Capacity)
	if err != nil {
		return err
	}
	return (&controller.Server{Dir: store.Dir, Store: store, Executor: executor, Workers: workers, Operational: cfg.Controller, RunTimeout: 5 * time.Minute, MaxRuns: 1}).Serve(ctx)
}

type fixtureProcess struct {
	*exec.Cmd
	done chan struct{}
	err  error
}

func (p *fixtureProcess) Wait() error { <-p.done; return p.err }
func startFixture(ctx context.Context, root, configuration string) (*fixtureProcess, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, "fixture-controller", "--allow-live", root, configuration)
	log, err := os.OpenFile(filepath.Join(root, "fixture-controller.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		_ = log.Close()
		return nil, err
	}
	_ = log.Close()
	process := &fixtureProcess{Cmd: cmd, done: make(chan struct{})}
	go func() { process.err = cmd.Wait(); close(process.done) }()
	client := controller.NewClient(filepath.Join(root, ".agent-workflow"))
	for {
		var status any
		if client.Call(ctx, "GET", "/health", nil, &status) == nil {
			return process, nil
		}
		select {
		case <-process.done:
			return nil, fmt.Errorf("fixture controller failed before readiness; inspect private fixture-controller.log")
		case <-ctx.Done():
			stopFixture(process)
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func stopFixture(cmd *fixtureProcess) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func writeManagedWorkflow(root, role, prompt string, structured bool) (string, error) {
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(map[string]any{"jobs": map[string]any{"conformance": map[string]any{"steps": []any{map[string]any{"uses": "action/conformance-turn", "with": turnInput{role, prompt, structured}}}}}})
	if err != nil {
		return "", err
	}
	return "workflows/conformance.yml", os.WriteFile(filepath.Join(root, "workflows/conformance.yml"), data, 0600)
}

func directScenario(ctx context.Context, runner *clirunner.Runner, role string, spec manifest, root string, report *evidence) error {
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		return err
	}
	defer store.Close()
	run := &workflows.Run{ID: runtimeexec.NewID(), Owner: runtimeexec.NewID(), Status: workflows.RunStatusRunning}
	if err = store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		return err
	}
	id := runtimeexec.NewID()
	ctx = runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: run.ID, Owner: run.Owner, Generation: run.Generation, LogicalID: id})
	req := workflows.AgentRequest{AgentID: role, RunID: run.ID, Generation: run.Generation, InvocationID: id, Prompt: spec.Prompt}
	if spec.Driver == "structured" {
		req.Output = structuredContract()
	}
	out, err := providerTurn(ctx, store, runner, req)
	if err != nil {
		return err
	}
	if req.Output != nil && !workflows.ValidateAgentStructuredOutput(fmt.Sprint(out["text"]), req.Output).Valid {
		return fmt.Errorf("structured schema was not satisfied")
	}
	if spec.Driver == "agent" && !containsMarker(out) {
		return fmt.Errorf("provider did not return response marker")
	}
	version, _ := out["provider_version"].(string)
	report.ProviderVersions[runner.Config.Agents[role].Provider] = version
	report.Invocations = append(report.Invocations, observedInvocation{id, runner.Config.Agents[role].Provider, version})
	usage, err := store.Usage(ctx, run.ID)
	if err != nil {
		return err
	}
	report.Usage = &usage.Summary
	return nil
}

func containsMarker(out map[string]any) bool {
	return strings.Contains(fmt.Sprint(out["text"]), "CONFORMANCE_OK")
}
func observedBothProviders(records []observedInvocation) bool {
	found := map[string]bool{}
	for _, r := range records {
		if r.ID != "" && r.Version != "" && r.Version != "unavailable" {
			found[r.Provider] = true
		}
	}
	return found["claude-sdk"] && found["codex-app-server"]
}
func runtimeVersion(ctx context.Context, runtime string) (string, error) {
	data, err := exec.CommandContext(ctx, runtime, "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("runtime version unavailable")
	}
	return value, nil
}

func saveObservation(state string, value observedInvocation) error {
	dir := filepath.Join(state, "conformance-observations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, value.ID+".json"), data, 0600)
}
func collectObservations(root string, report *evidence) error {
	paths, err := filepath.Glob(filepath.Join(root, ".agent-workflow/conformance-observations/*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var v observedInvocation
		if err = json.Unmarshal(data, &v); err != nil {
			return err
		}
		report.ProviderVersions[v.Provider] = v.Version
		if err = addObservation(report, v); err != nil {
			return err
		}
	}
	return nil
}

func addObservation(report *evidence, value observedInvocation) error {
	for _, existing := range report.Invocations {
		if existing.ID == value.ID && existing.Provider == value.Provider {
			if existing.Version != value.Version {
				return fmt.Errorf("observed invocation changed provider version")
			}
			return nil
		}
	}
	report.Invocations = append(report.Invocations, value)
	return nil
}
