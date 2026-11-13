// workflow-conformance runs explicitly requested real-provider scenarios only.
// Normal tests and build validation never authorize live calls. Tag release CI
// has an explicit, repository/workflow/scenario-scoped authorization mode.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/buildinfo"
	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type manifest struct {
	ID          string   `json:"id"`
	Driver      string   `json:"driver"`
	Provider    string   `json:"provider,omitempty"`
	Managed     bool     `json:"managed_policy_required,omitempty"`
	Publication bool     `json:"publication,omitempty"`
	Assertions  []string `json:"assertions"`
	Prompt      string   `json:"prompt"`
}
type evidence struct {
	Provider                 string                  `json:"provider"`
	Authentication           string                  `json:"authentication"`
	Scenario                 string                  `json:"scenario"`
	Engine                   map[string]string       `json:"engine"`
	OS                       string                  `json:"os"`
	Arch                     string                  `json:"arch"`
	ConfigurationFingerprint string                  `json:"configuration_fingerprint"`
	ProviderVersions         map[string]string       `json:"provider_versions"`
	Invocations              []observedInvocation    `json:"invocations"`
	RuntimeRecovery          map[string]bool         `json:"runtime_recovery,omitempty"`
	RuntimeVersion           string                  `json:"runtime_version,omitempty"`
	Runtime                  string                  `json:"runtime,omitempty"`
	Image                    string                  `json:"image_digest,omitempty"`
	Assertions               map[string]bool         `json:"assertions"`
	Usage                    *workflows.UsageSummary `json:"usage,omitempty"`
	Artifacts                []string                `json:"artifacts"`
	Error                    string                  `json:"error,omitempty"`
	Started                  time.Time               `json:"started"`
	Finished                 time.Time               `json:"finished"`
}

func main() {
	if len(os.Args) == 5 && os.Args[1] == "fixture-controller" && os.Args[2] == "--allow-live" {
		if err := authorizeCI(os.Getenv("AGENT_WORKFLOW_RELEASE_CONFORMANCE") == "1", os.Getenv("AGENT_WORKFLOW_CONFORMANCE_SCENARIO"), false); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := serveFixture(ctx, os.Args[3], os.Args[4]); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (resultErr error) {
	flags := flag.NewFlagSet("workflow-conformance", flag.ContinueOnError)
	scenario := flags.String("scenario", "", "explicit scenario manifest path")
	config := flags.String("config", "", "explicit provider configuration")
	role := flags.String("role", "", "configured primary role")
	childRole := flags.String("child-role", "", "configured nested worker role")
	engine := flags.String("engine", "", "matching agent-workflow binary for CLI/MCP scenarios")
	output := flags.String("evidence", "", "private evidence directory outside the repository")
	releaseCI := flags.Bool("release-ci", false, "authorize only the named release scenario in version-tag CI")
	allowLive := flags.Bool("allow-live", false, "explicitly authorize this scenario's real provider calls")
	allowPublication := flags.Bool("allow-publication", false, "explicitly authorize publication to the supplied disposable repository")
	disposable := flags.String("disposable-repository", "", "exact existing disposable owner/repository; never created automatically")
	repositoryPath := flags.String("repository-path", "", "existing local disposable repository to clone independently")
	workflowRef := flags.String("workflow", "", "workflow reference within the supplied disposable repository")
	engineCommit := flags.String("engine-commit", "", "exact source commit when running a development harness build")
	runtimeControl := flags.String("runtime-control", "", "owned disposable runtime stop/start helper")
	timeout := flags.Duration("timeout", 5*time.Minute, "whole scenario timeout")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if err := authorizeCI(*releaseCI, *scenario, *allowPublication); err != nil {
		return err
	}
	if !*allowLive {
		return fmt.Errorf("explicit --allow-live and user authorization for the named scenario are required")
	}
	if *scenario == "" || *config == "" || *role == "" || *output == "" {
		return fmt.Errorf("--scenario, --config, --role and private --evidence are required")
	}
	data, err := os.ReadFile(*scenario)
	if err != nil {
		return err
	}
	var spec manifest
	if err = json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if spec.ID == "" || len(spec.Assertions) == 0 {
		return fmt.Errorf("invalid scenario manifest")
	}
	if spec.Publication && (!*allowPublication || *disposable == "" || *repositoryPath == "" || *workflowRef == "") {
		return fmt.Errorf("publication requires --allow-publication, exact --disposable-repository, --repository-path and --workflow")
	}
	if err := authorizeCI(*releaseCI, spec.ID, spec.Publication || spec.Driver == "publication"); err != nil {
		return err
	}
	if *timeout <= 0 || (*releaseCI && *timeout > 5*time.Minute) {
		return fmt.Errorf("release scenarios require a positive deadline no longer than five minutes")
	}
	if *releaseCI {
		_ = os.Setenv("AGENT_WORKFLOW_RELEASE_CONFORMANCE", "1")
		_ = os.Setenv("AGENT_WORKFLOW_CONFORMANCE_SCENARIO", spec.ID)
	}
	commit := buildinfo.Commit
	if commit == "development" {
		commit = *engineCommit
	}
	if len(commit) != 40 || strings.Trim(commit, "0123456789abcdef") != "" {
		return fmt.Errorf("an exact engine commit is required in the build or --engine-commit")
	}
	if *releaseCI && (commit != os.Getenv("GITHUB_SHA") || buildinfo.Dirty != "false") {
		return fmt.Errorf("release harness must match the clean tag commit")
	}
	if err = validateEvidenceLocation(*output); err != nil {
		return err
	}
	if *engine != "" {
		if err = verifyEngineBinary(*engine, commit); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(*output, 0700); err != nil {
		return err
	}
	private, err := os.MkdirTemp(*output, "scenario-"+spec.ID+"-")
	if err != nil {
		return err
	}
	report := evidence{Scenario: spec.ID, Engine: buildinfo.Metadata(), OS: runtime.GOOS, Arch: runtime.GOARCH, Assertions: map[string]bool{}, ProviderVersions: map[string]string{}, Artifacts: []string{filepath.Join(private, "workspace")}, Started: time.Now().UTC()}
	report.Engine["commit"] = commit
	for _, assertion := range spec.Assertions {
		report.Assertions[assertion] = false
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	defer func() {
		if resultErr != nil && report.Error == "" {
			report.Error = "scenario failed before its assertions completed"
		}
		report.Finished = time.Now().UTC()
		encoded, _ := json.MarshalIndent(report, "", "  ")
		resultErr = errors.Join(resultErr, os.WriteFile(filepath.Join(private, "evidence.json"), append(encoded, '\n'), 0600))
	}()
	cfg, err := clirunner.LoadConfig(*config)
	if err != nil {
		report.Error = "configuration prerequisites unavailable"
		return err
	}
	selected, ok := cfg.Agents[*role]
	if !ok {
		return fmt.Errorf("role is not configured")
	}
	report.Provider = selected.Provider
	report.Authentication = selected.Authentication.Mode
	if report.Authentication == "" {
		report.Authentication = "native"
	}
	if spec.Provider != "" && selected.Provider != spec.Provider {
		return fmt.Errorf("scenario requires provider %s; backend substitution is forbidden", spec.Provider)
	}
	if spec.Publication && cfg.Publication.Repository != *disposable {
		return fmt.Errorf("publication policy must exactly match the supplied disposable repository")
	}
	root := filepath.Join(private, "workspace")
	temporary := filepath.Join(private, "tmp")
	if err = os.MkdirAll(temporary, 0700); err != nil {
		return err
	}
	if err = os.Setenv("TMPDIR", temporary); err != nil {
		return err
	}
	cfg.Controller.TempDirectory = temporary
	if *repositoryPath != "" {
		if err = exec.CommandContext(ctx, "git", "clone", "--no-hardlinks", *repositoryPath, root).Run(); err != nil {
			return err
		}
	} else {
		if err = os.Mkdir(root, 0700); err != nil {
			return err
		}
		if err = exec.CommandContext(ctx, "git", "-C", root, "init", "-b", "main").Run(); err != nil {
			return err
		}
	}
	if spec.Publication {
		remote, remoteErr := exec.CommandContext(ctx, "git", "-C", *repositoryPath, "remote", "get-url", "origin").Output()
		if remoteErr != nil {
			return remoteErr
		}
		value := strings.TrimSpace(string(remote))
		normalized := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(value, "https://github.com/"), "git@github.com:"), ".git")
		if normalized != *disposable {
			return fmt.Errorf("supplied disposable repository remote does not match allowlist")
		}
		if err = exec.CommandContext(ctx, "git", "-C", root, "remote", "set-url", "origin", value).Run(); err != nil {
			return err
		}
	}
	for name, agent := range cfg.Agents {
		agent.Workspace = root
		if agent.InstructionText != "" {
			path := filepath.Join(private, "instructions-"+name+".md")
			if err = os.WriteFile(path, []byte(agent.InstructionText), 0600); err != nil {
				return err
			}
			agent.Instructions = []string{path}
		}
		cfg.Agents[name] = agent
	}
	if !spec.Publication {
		cfg.Publication.Repository = ""
	}
	encoded, _ := json.Marshal(cfg)
	fingerprint := sha256.Sum256(encoded)
	report.ConfigurationFingerprint = hex.EncodeToString(fingerprint[:])
	report.Runtime = selected.Execution.Runtime
	report.Image = selected.Execution.Image
	runner := &clirunner.Runner{Config: cfg, Workspace: root}
	if err = runner.Preflight(ctx, []string{*role}); err != nil {
		report.Error = workflows.Redact(err.Error(), runner.SecretValues())
		return err
	}
	capabilities, err := runner.Capabilities(*role)
	if err != nil {
		return err
	}
	if spec.Managed && !capabilities.ManagedPolicy {
		report.Error = "upstream managed-policy capability is uncertified"
		return errors.New(report.Error)
	}
	configuration := filepath.Join(root, "conformance-agents.yml")
	encoded, err = yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err = os.WriteFile(configuration, encoded, 0600); err != nil {
		return err
	}
	var uncertainEffects *atomic.Int32
	if spec.Driver == "external-uncertain" {
		fixture, count := uncertainMCPFixture()
		defer fixture.Close()
		uncertainEffects = count
		if cfg.MCPServers == nil {
			cfg.MCPServers = map[string]clirunner.MCPServer{}
		}
		cfg.MCPServers["conformance_uncertain"] = clirunner.MCPServer{Transport: "streamable-http", Endpoint: fixture.URL, Timeout: "10s"}
		agent := cfg.Agents[*role]
		agent.MCPGrants = []workflows.ToolGrant{{Server: "conformance_uncertain", Tool: "effect"}}
		cfg.Agents[*role] = agent
		encoded, err = yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		if err = os.WriteFile(configuration, encoded, 0600); err != nil {
			return err
		}
	}
	encoded, _ = json.Marshal(cfg)
	configurationHash := sha256.Sum256(encoded)
	report.ConfigurationFingerprint = hex.EncodeToString(configurationHash[:])
	var result *workflows.Run
	if spec.Driver == "container-owner-loss" {
		if *engine == "" || selected.Execution.Mode != "container" {
			return fmt.Errorf("owner-loss scenario requires --engine and a pinned container role")
		}
		result, err = ownerLossScenario(ctx, *engine, root, configuration, *role, selected.Execution.Runtime, *runtimeControl, &report)
	} else if spec.Driver == "external-uncertain" {
		if *engine == "" {
			return fmt.Errorf("external uncertain-response scenario requires --engine")
		}
		ref, writeErr := writeManagedWorkflow(root, *role, "Call the conformance-only external effect tool exactly once with {}. If it fails, do not issue another call.", false)
		if writeErr != nil {
			return writeErr
		}
		result, err = cliScenario(ctx, *engine, root, configuration, ref)
		if result != nil && result.Status == workflows.RunStatusNeedsAttention && uncertainEffects.Load() == 1 {
			err = nil
		}
	} else if spec.Driver == "nested" {
		if *engine == "" || *childRole == "" {
			return fmt.Errorf("nested worker scenario requires --engine and --child-role")
		}
		if _, ok := cfg.Agents[*childRole]; !ok {
			return fmt.Errorf("child role is not in the admitted configuration")
		}
		prompt := fmt.Sprintf("Use engine worker tools: spawn role %s with 4 turns and a message asking it to spawn role %s with 1 turn, wait for that grandchild to reply LEAF_OK, then reply CHILD_OK. Wait for the child message result before replying CONFORMANCE_OK. Do not perform native delegation.", *childRole, *childRole)
		ref, writeErr := writeManagedWorkflow(root, *role, prompt, false)
		if writeErr != nil {
			return writeErr
		}
		result, err = cliScenario(ctx, *engine, root, configuration, ref)
	} else {
		switch spec.Driver {
		case "session", "interrupt":
			if !capabilities.Sessions || !capabilities.Resume {
				return fmt.Errorf("scenario requires certified session transport")
			}
			err = sessionScenario(ctx, runner, *role, spec.Driver, root, &report)
		case "agent", "structured":
			err = directScenario(ctx, runner, *role, spec, root, &report)
		case "managed-canary", "container":
			if spec.Driver == "container" && selected.Execution.Mode != "container" {
				return fmt.Errorf("container scenario requires pinned container role")
			}
			if spec.Driver == "managed-canary" {
				if err = writeCanaries(root); err != nil {
					return err
				}
			}
			ref, writeErr := writeManagedWorkflow(root, *role, spec.Prompt, false)
			if writeErr != nil {
				return writeErr
			}
			result, err = cliScenario(ctx, *engine, root, configuration, ref)
			if err == nil && spec.Driver == "managed-canary" {
				for _, marker := range []string{"NATIVE_HOOK_RAN", "NATIVE_MCP_RAN"} {
					if _, e := os.Stat(filepath.Join(root, marker)); !os.IsNotExist(e) {
						err = fmt.Errorf("native canary executed")
					}
				}
			}
		case "workflow-cli", "workflow-mcp", "publication":
			if *engine == "" || *workflowRef == "" || *repositoryPath == "" {
				return fmt.Errorf("workflow scenarios require --engine, --workflow and --repository-path")
			}
			if spec.Driver == "workflow-mcp" {
				result, err = mcpScenario(ctx, *engine, root, configuration, *workflowRef)
			} else if spec.Publication {
				result, err = runCLI(ctx, *engine, root, configuration, *workflowRef, true)
			} else {
				result, err = cliScenario(ctx, *engine, root, configuration, *workflowRef)
			}
			if result != nil {
				report.Usage = result.Usage
			}
		default:
			return fmt.Errorf("unknown scenario driver %q", spec.Driver)
		}
	}
	if err != nil {
		report.Error = workflows.Redact(err.Error(), runner.SecretValues())
		return errors.New(report.Error)
	}
	if result != nil && result.Status != workflows.RunStatusSucceeded && spec.Driver != "external-uncertain" && spec.Driver != "container-owner-loss" {
		return fmt.Errorf("scenario did not succeed")
	}
	if result != nil {
		report.Usage = result.Usage
		for _, job := range result.Jobs {
			for _, step := range job.Steps {
				provider, _ := step.Outputs["provider"].(string)
				version, _ := step.Outputs["provider_version"].(string)
				if provider != "" && version != "" {
					report.ProviderVersions[provider] = version
				}
			}
		}
	}
	if err = collectObservations(root, &report); err != nil {
		return err
	}
	if selected.Execution.Mode == "container" {
		report.RuntimeVersion, err = runtimeVersion(ctx, selected.Execution.Runtime)
		if err != nil {
			return err
		}
	}
	verified := map[string]bool{"scenario_completed": true, "private_workspace": true, "usage_observed": report.Usage != nil && report.Usage.Invocations > 0 && ((report.Usage.InputTokens != nil && *report.Usage.InputTokens > 0) || (report.Usage.OutputTokens != nil && *report.Usage.OutputTokens > 0))}
	if spec.Driver == "container-owner-loss" && result != nil {
		verified["owner_loss_quarantined"] = result.Status == workflows.RunStatusNeedsAttention
		verified["owner_loss_cleanup_confirmed"] = true
	}
	if spec.Driver == "external-uncertain" && result != nil {
		verified["uncertain_effect_not_replayed"] = result.Status == workflows.RunStatusNeedsAttention && uncertainEffects.Load() == 1
	}
	if result != nil {
		verified["workflow_succeeded"] = result.Status == workflows.RunStatusSucceeded
		for _, job := range result.Jobs {
			for _, step := range job.Steps {
				if strings.Contains(fmt.Sprint(step.Outputs["text"]), "CONFORMANCE_OK") {
					verified["response_marker"] = true
				}
			}
		}
	}
	if spec.Driver == "agent" || spec.Driver == "structured" {
		verified["workflow_succeeded"] = true
		verified["response_marker"] = true
		verified["structured_schema_valid"] = true
	}
	if spec.Driver == "session" {
		verified["session_context_retained"] = true
	}
	if spec.Driver == "interrupt" {
		verified["interruption_retains_session"] = true
	}
	if spec.Driver == "structured" && result != nil && result.Status == workflows.RunStatusSucceeded {
		verified["structured_schema_valid"] = true
	}
	if spec.Driver == "managed-canary" {
		verified["native_canaries_suppressed"] = true
	}
	if result != nil {
		store, openErr := workflows.OpenSQLiteInspection(filepath.Join(root, ".agent-workflow"))
		if openErr != nil {
			return openErr
		}
		usage, usageErr := store.Usage(ctx, result.ID)
		if usageErr != nil {
			_ = store.Close()
			return usageErr
		}
		for _, u := range usage.Invocations {
			if version := report.ProviderVersions[u.Provider]; version != "" {
				if err = addObservation(&report, observedInvocation{u.ID, u.Provider, version}); err != nil {
					_ = store.Close()
					return err
				}
			}
		}
		verified["both_providers_executed"] = observedBothProviders(report.Invocations)
		workerManager := &workflows.WorkerManager{Store: store}
		workers, workersErr := workerManager.List(ctx, result.ID)
		if workersErr == nil {
			depth := 0
			completed := true
			for _, worker := range workers {
				messages, messageErr := workerManager.Messages(ctx, result.ID, worker.ID)
				if messageErr != nil || len(messages) == 0 {
					completed = false
				}
				for _, message := range messages {
					if message.Status != workflows.RunStatusSucceeded {
						completed = false
					}
				}
				if worker.Depth > depth {
					depth = worker.Depth
				}
			}
			verified["nested_workers_at_capacity_one"] = len(workers) >= 2 && depth >= 2 && completed
		}
		records, recordsErr := store.Invocations(ctx, result.ID, false)
		if recordsErr == nil {
			containerFound := false
			clean := true
			for _, record := range records {
				if record.Container != nil {
					containerFound = true
				}
				if record.State != "cleaned" {
					clean = false
				}
			}
			verified["container_cleanup_confirmed"] = containerFound && clean
		}
		calls, callsErr := store.ToolCalls(ctx, result.ID, false)
		if callsErr == nil {
			for _, call := range calls {
				if call.Server != "engine" && call.Status == "completed" {
					verified["external_mcp_call_recorded"] = true
				}
			}
		}
		events, eventErr := store.Events(ctx, result.ID, 0)
		if eventErr == nil {
			publications := map[string]bool{}
			for _, event := range events {
				if event.Type == "action.completed" {
					var intent workflows.ExternalIntent
					if json.Unmarshal(event.Data, &intent) == nil && intent.Kind == "github/pr" && intent.Status == "completed" {
						publications[intent.Key] = true
					}
				}
			}
			verified["one_durable_publication"] = len(publications) == 1
		}
		_ = store.Close()
	}
	verified["both_providers_executed"] = observedBothProviders(report.Invocations)
	for _, assertion := range spec.Assertions {
		if !verified[assertion] {
			return fmt.Errorf("required assertion was not proved: %s", assertion)
		}
		report.Assertions[assertion] = true
	}
	fmt.Println(filepath.Join(private, "evidence.json"))
	return nil
}

func writeWorkflow(root, role, prompt string, structured bool) (string, error) {
	if role != "codex" {
		return "", fmt.Errorf("workflow agent-task scenarios require the codex runtime; other backends retain direct adapter scenarios")
	}

	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "conformance.toml"), []byte("name='conformance'\ndescription='Explicit disposable conformance task'\ndeveloper_instructions='Follow the supplied bounded conformance task.'\n"), 0600); err != nil {
		return "", err
	}
	with := map[string]any{"agent": "conformance.toml", "prompt": prompt}
	if structured {
		with["output"] = map[string]any{"format": "json", "repair-attempts": 0, "schema": map[string]any{"type": "object", "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean", "const": true}}, "additionalProperties": false}}
	}
	definition := map[string]any{"jobs": map[string]any{"conformance": map[string]any{"steps": []any{map[string]any{"uses": "agent/run", "with": with}}}}}
	data, err := yaml.Marshal(definition)
	if err != nil {
		return "", err
	}
	return "workflows/conformance.yml", os.WriteFile(filepath.Join(root, "workflows", "conformance.yml"), data, 0600)
}
func writeCanaries(root string) error {
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("NATIVE_DISCOVERY_CANARY_MUST_NOT_LOAD\n"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch NATIVE_HOOK_RAN"}]}]}}`), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"unapproved-canary":{"command":"sh","args":["-c","touch NATIVE_MCP_RAN; exit 1"]}}}`), 0600)
}

func sessionScenario(ctx context.Context, runner *clirunner.Runner, role, driver, root string, report *evidence) error {
	store, err := workflows.OpenSQLite(filepath.Join(root, ".agent-workflow"))
	if err != nil {
		return err
	}
	defer store.Close()
	run := &workflows.Run{ID: runtimeexec.NewID(), Owner: runtimeexec.NewID(), Status: workflows.RunStatusRunning}
	if err = store.Admit(ctx, run, workflows.Snapshot{Version: 1}); err != nil {
		return err
	}
	provider, model := runner.UsageProvider(role, "")
	call := func(callCtx context.Context, id, session, prompt string, emit func(workflows.ProviderEvent)) (workflows.SessionResult, error) {
		identity := workflows.UsageIdentity{ID: id, RunID: run.ID, Kind: "worker", WorkerID: "session-owner", Provider: provider, Model: model, SessionID: session}
		if err := store.BeginUsage(callCtx, identity); err != nil {
			return workflows.SessionResult{}, err
		}
		callCtx = runtimeexec.WithBinding(callCtx, runtimeexec.Binding{Journal: store, Dir: store.Dir, RunID: run.ID, Owner: run.Owner, Generation: run.Generation, LogicalID: id})
		request := workflows.SessionRequest{SessionID: session, AgentRequest: workflows.AgentRequest{RunID: run.ID, Generation: run.Generation, InvocationID: id, ParentWorkerID: "session-owner", AgentID: role, Prompt: prompt, ObserveUsage: func(update workflows.UsageUpdate) error {
			return store.ObserveUsage(context.WithoutCancel(callCtx), identity, update)
		}}}
		result, err := runner.RunTurn(callCtx, request, emit)
		usage, _ := result.Outputs["usage"].(map[string]any)
		incomplete, _ := result.Outputs["usage_incomplete"].(bool)
		if recordErr := store.ObserveUsage(context.WithoutCancel(callCtx), identity, workflows.UsageUpdate{Key: "final", Final: true, Incomplete: incomplete, Provider: provider, Model: model, SessionID: result.SessionID, Usage: usage, CostUSD: result.Outputs["cost_usd"]}); recordErr != nil {
			return result, recordErr
		}
		return result, err
	}
	nonce := runtimeexec.NewID()
	if driver == "session" {
		first, err := call(ctx, "first", "", "Remember the public nonce "+nonce+". Reply FIRST_OK.", nil)
		if err != nil {
			return err
		}
		second, err := call(ctx, "second", first.SessionID, "Return only the public nonce remembered in the previous turn.", nil)
		if err != nil {
			return err
		}
		if first.SessionID == "" || second.SessionID != first.SessionID || !strings.Contains(fmt.Sprint(second.Outputs["text"]), nonce) {
			return fmt.Errorf("session identity/context was not retained")
		}
		report.ProviderVersions[provider] = second.ProviderVersion
		report.Invocations = append(report.Invocations, observedInvocation{"first", provider, first.ProviderVersion}, observedInvocation{"second", provider, second.ProviderVersion})
	} else {
		turn, cancel := context.WithCancel(ctx)
		defer cancel()
		started := make(chan struct{}, 1)
		done := make(chan struct{})
		var result workflows.SessionResult
		var callErr error
		go func() {
			defer close(done)
			result, callErr = call(turn, "interrupt", "", "Use an approved shell tool to sleep for 60 seconds; do not complete before it finishes.", func(event workflows.ProviderEvent) {
				if event.Type == "usage" && event.SessionID != "" {
					select {
					case started <- struct{}{}:
					default:
					}
				}
			})
		}()
		select {
		case <-started:
		case <-ctx.Done():
			return ctx.Err()
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-timer.C:
			cancel()
		case <-done:
			timer.Stop()
			return fmt.Errorf("provider completed before interruption boundary")
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if callErr == nil || result.SessionID == "" {
			return fmt.Errorf("interruption did not preserve a failed turn/session")
		}
		report.ProviderVersions[provider] = result.ProviderVersion
		report.Invocations = append(report.Invocations, observedInvocation{"interrupt", provider, result.ProviderVersion})
	}
	usage, err := store.Usage(context.WithoutCancel(ctx), run.ID)
	if err != nil {
		return err
	}
	report.Usage = &usage.Summary
	return nil
}

func cliScenario(ctx context.Context, binary, root, configuration, ref string) (*workflows.Run, error) {
	fixture, err := startFixture(ctx, root, configuration)
	if err != nil {
		return nil, err
	}
	defer stopFixture(fixture)
	return runCLI(ctx, binary, root, configuration, ref, false)
}
func runCLI(ctx context.Context, binary, root, configuration, ref string, foreground bool) (*workflows.Run, error) {
	args := []string{"run", "--parallel", "1", "--workspace", root, "--config", configuration}
	if foreground {
		args = append(args, "--foreground")
	}
	args = append(args, ref)
	command := exec.CommandContext(ctx, binary, args...)
	data, runErr := command.Output()
	var result workflows.Run
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("CLI conformance did not return a durable result: %w", runErr)
	}
	return &result, runErr
}
func mcpScenario(ctx context.Context, binary, root, configuration, ref string) (*workflows.Run, error) {
	fixture, err := startFixture(ctx, root, configuration)
	if err != nil {
		return nil, err
	}
	defer stopFixture(fixture)
	command := exec.CommandContext(ctx, binary, "mcp", "--parallel", "1", "--workspace", root, "--config", configuration)
	client := mcp.NewClient(&mcp.Implementation{Name: "workflow-conformance", Version: buildinfo.Version}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	defer exec.Command(binary, "stop", "--workspace", root).Run()
	response, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "workflow_start", Arguments: map[string]any{"ref": ref, "request_id": "conformance"}})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(response.StructuredContent)
	var handle struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &handle) != nil || handle.ID == "" {
		return nil, fmt.Errorf("MCP start did not return a durable handle")
	}
	for {
		response, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "run_status", Arguments: map[string]any{"id": handle.ID}})
		if err != nil {
			return nil, err
		}
		raw, _ = json.Marshal(response.StructuredContent)
		var result workflows.Run
		if err = json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if result.Status != "queued" && result.Status != "running" {
			return &result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Resolve the existing ancestor first so a symlink cannot put private evidence
// back inside a checkout. No provider or remote command is involved.
func validateEvidenceLocation(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	existing := absolute
	for {
		_, err = os.Lstat(existing)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return fmt.Errorf("evidence location has no accessible ancestor")
		}
		existing = parent
	}
	existing, err = filepath.EvalSymlinks(existing)
	if err != nil {
		return err
	}
	for {
		if _, err := os.Lstat(filepath.Join(existing, ".git")); err == nil {
			return fmt.Errorf("private evidence must be outside a Git checkout")
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return nil
		}
		existing = parent
	}
}
func verifyEngineBinary(binary, commit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, binary, "version").Output()
	if err != nil {
		return fmt.Errorf("inspect supplied engine version: %w", err)
	}
	var version map[string]string
	if err = json.Unmarshal(data, &version); err != nil {
		return fmt.Errorf("invalid supplied engine version")
	}
	metadata := buildinfo.Metadata()
	if version["version"] != metadata["version"] || version["commit"] != commit || version["source_fingerprint"] != metadata["source_fingerprint"] || version["dirty"] != metadata["dirty"] {
		return fmt.Errorf("supplied engine does not match harness build identity")
	}
	return nil
}
