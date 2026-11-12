package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/prfix"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func prFixCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: agent-workflow pr-fix <prepare|run|resume|continue-run> [flags] [PR_URL]\n\nPreparation is read-only on GitHub. Selected custom skills require --adapters. Flags precede arguments.")
		return nil
	}
	verb := args[0]
	explicit := map[string]bool{}
	if verb != "prepare" && verb != "run" && verb != "resume" && verb != "continue-run" {
		return fmt.Errorf("unknown pr-fix command %q", verb)
	}
	fs := flag.NewFlagSet("pr-fix "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", ".", "workspace containing workflows/")
	configPath := fs.String("config", "agents.yml", "Codex runtime configuration")
	role := fs.String("role", "codex", "native task runtime (codex with codex-app-server required)")
	runID := fs.String("run", "", "retained logical PR-fix run ID")
	instruction := fs.String("instruction", "", "exact original user instruction")
	instructionFile := fs.String("instruction-file", "", "file containing exact original user instruction")
	optionsFile := fs.String("options", "", "invocation-local dk-pr-fix JSON options")
	adapterFile := fs.String("adapters", "", "deterministic custom adapter YAML")
	authorizationFile := fs.String("authorizations", "", "JSON object of exact user authorizations by operation kind (or iterate)")
	otherFile := fs.String("maintain-other", "", "JSON user authorization to maintain another author's PR")
	upstream := fs.String("upstream", "", "optional upstream HOST/OWNER/REPO scope")
	preparedFile := fs.String("prepared", "", "immutable prepared plan path")
	requestID := fs.String("request-id", "", "stable user request ID for admission replay or continuation")
	authorization := fs.String("authorization", "", "exact renewed user instruction (continuation only)")
	source := fs.String("source", "", "renewed instruction source (continuation only)")
	cycles := fs.Int("cycles", 5, "additional review cycles, one to five")
	parallel := fs.Int("parallel", 4, "shared engine capacity")
	detach := fs.Bool("detach", false, "return execution handle without waiting")
	timeout := fs.Duration("timeout", 30*time.Minute, "client wait timeout; zero disables")
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if fs.NArg() > 1 {
		return fmt.Errorf("at most one PR URL; flags precede arguments")
	}
	root, e := filepath.Abs(*workspace)
	if e != nil {
		return e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	encode := func(v any) error { enc := json.NewEncoder(stdout); enc.SetIndent("", "  "); return enc.Encode(v) }
	if verb == "continue-run" {
		if fs.NArg() != 0 || *runID == "" {
			return fmt.Errorf("continue-run requires --run and renewed authorization flags")
		}
		l, e := prfix.OpenLedger(root)
		if e != nil {
			return e
		}
		defer l.Close()
		allowance, e := l.Continue(ctx, *runID, prfix.Authorization{RequestID: *requestID, Instruction: *authorization, Source: *source}, *cycles)
		if e != nil {
			return e
		}
		return encode(map[string]any{"logical_run": *runID, "review_allowance": allowance, "request_id": *requestID})
	}
	var prepared prfix.Prepared
	var saved prfix.Plan
	if *preparedFile != "" {
		saved, e = prfix.ReadPlan(*preparedFile)
		if e != nil {
			return e
		}
		prepared = prfix.Prepared{LogicalRun: saved.LogicalRun, Workflow: saved.Ref, Plan: *preparedFile, Roles: saved.Roles, Inputs: map[string]any{"plan": *preparedFile}, RequestID: *requestID, Omitted: saved.Omitted}
		*runID = saved.LogicalRun
		if !explicit["config"] && saved.ConfigPath != "" {
			*configPath = saved.ConfigPath
		}
	}
	if verb == "resume" {
		if *runID == "" || fs.NArg() != 0 {
			return fmt.Errorf("resume requires --run and no PR URL")
		}
		l, e := prfix.OpenLedger(root)
		if e != nil {
			return e
		}
		saved, e = l.Plan(ctx, *runID)
		l.Close()
		if e != nil {
			return e
		}
		if *preparedFile == "" {
			if *instruction == "" {
				*instruction = saved.Instructions
			}
			if !explicit["upstream"] {
				*upstream = saved.Upstream
			}
			if !explicit["config"] {
				*configPath = saved.ConfigPath
			}
		}
	}
	configAbs := *configPath
	if !filepath.IsAbs(configAbs) {
		configAbs = filepath.Join(root, configAbs)
	}
	cfg, e := loadPRFixConfig(ctx, root, configAbs)
	if e != nil {
		return e
	}
	if prepared.Plan == "" && verb != "resume" {
		if fs.NArg() != 1 {
			return fmt.Errorf("%s requires a canonical PR URL or --prepared", verb)
		}
		if *instructionFile != "" {
			b, e := os.ReadFile(*instructionFile)
			if e != nil {
				return e
			}
			*instruction = string(b)
		}
		options := map[string]any{}
		if *optionsFile != "" {
			if e = readPRFixJSON(*optionsFile, &options); e != nil {
				return e
			}
		}
		adapters, e := prfix.LoadAdapters(*adapterFile)
		if e != nil {
			return e
		}
		auth := map[string]prfix.Authorization{}
		if *authorizationFile != "" {
			if e = readPRFixJSON(*authorizationFile, &auth); e != nil {
				return e
			}
		}
		var other *prfix.Authorization
		if *otherFile != "" {
			other = &prfix.Authorization{}
			if e = readPRFixJSON(*otherFile, other); e != nil {
				return e
			}
		}
		prepared, e = prfix.Prepare(ctx, root, &prfix.GitHub{Root: root}, prfix.PrepareRequest{RequestID: *requestID, PR: fs.Arg(0), Run: *runID, Instructions: *instruction, Role: *role, Options: options, Upstream: *upstream, Adapters: adapters, Config: cfg, ConfigPath: configAbs, Authorizations: auth, MaintainOther: other})
		if e != nil {
			return e
		}
	}
	if verb == "resume" {
		req := prfix.ResumeRequest(saved, cfg)
		req.RequestID = *requestID
		req.ConfigPath = configAbs
		if explicit["upstream"] {
			req.Upstream = *upstream
		}
		if *otherFile != "" {
			req.MaintainOther = &prfix.Authorization{}
			if e = readPRFixJSON(*otherFile, req.MaintainOther); e != nil {
				return e
			}
		}
		if *role != "" {
			req.Role = *role
		}
		if *instruction != "" {
			req.Instructions = *instruction
		}
		if *instructionFile != "" {
			b, e := os.ReadFile(*instructionFile)
			if e != nil {
				return e
			}
			req.Instructions = string(b)
		}
		if *optionsFile != "" {
			req.Options = map[string]any{}
			if e = readPRFixJSON(*optionsFile, &req.Options); e != nil {
				return e
			}
		}
		if *adapterFile != "" {
			req.Adapters, e = prfix.LoadAdapters(*adapterFile)
			if e != nil {
				return e
			}
		}
		if *authorizationFile != "" {
			if e = readPRFixJSON(*authorizationFile, &req.Authorizations); e != nil {
				return e
			}
		}
		prepared, e = prfix.Prepare(ctx, root, &prfix.GitHub{Root: root}, req)
		if e != nil {
			return e
		}
	}
	if *preparedFile != "" && verb != "resume" {
		l, e := prfix.OpenLedger(root)
		if e != nil {
			return e
		}
		prepared.RequestID, e = l.Admission(ctx, saved.LogicalRun, *preparedFile, saved.Fingerprint, *requestID)
		l.Close()
		if e != nil {
			return e
		}
	}
	if prepared.LogicalRun != "" {
		*runID = prepared.LogicalRun
	}
	if verb == "prepare" {
		return encode(prepared)
	}
	if *parallel < 1 || *parallel > 128 || *timeout < 0 {
		return fmt.Errorf("invalid capacity or timeout")
	}
	client := controller.NewClient(filepath.Join(root, ".agent-workflow"))
	if e = ensureController(ctx, client, root, configAbs, *parallel, stderr); e != nil {
		return e
	}
	l, e := prfix.OpenLedger(root)
	if e != nil {
		return e
	}
	defer l.Close()
	var engine workflows.Run
	resumeExisting := false
	if verb == "resume" {
		request, engineID, e := l.LatestExecution(ctx, *runID)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil && request == prepared.RequestID {
			if engineID == "" { // Replay the original idempotent admission after a lost response.
				if e = client.Call(ctx, "POST", "/runs", controller.StartRequest{RequestID: request, Ref: prepared.Workflow, Inputs: prepared.Inputs}, &engine); e != nil {
					return e
				}
				if e = l.Execution(ctx, *runID, request, prepared.Plan, engine.ID); e != nil {
					return e
				}
			} else {
				if e = client.Call(ctx, "GET", "/runs/"+engineID, nil, &engine); e != nil {
					return e
				}
				if engine.Status != workflows.RunStatusRunning && engine.Status != workflows.RunStatusQueued && engine.Status != workflows.RunStatusSucceeded {
					// The normal engine requires this acknowledgement for a failed
					// for-each item. Registered mutations independently reconcile;
					// this flag cannot replay an unknown PR-fix publication.
					suffix := "?retry_uncertain=true&request_id=" + url.QueryEscape("resume-"+engineID+"-"+strconv.FormatInt(engine.Generation, 10))
					if e = client.Call(ctx, "POST", "/runs/"+engineID+"/resume"+suffix, nil, &engine); e != nil {
						return e
					}
				}
			}
			resumeExisting = true
		}
	}
	if !resumeExisting {
		if e = l.Execution(ctx, prepared.LogicalRun, prepared.RequestID, prepared.Plan, ""); e != nil {
			return e
		}
		if e = client.Call(ctx, "POST", "/runs", controller.StartRequest{RequestID: prepared.RequestID, Ref: prepared.Workflow, Inputs: prepared.Inputs}, &engine); e != nil {
			return e
		}
		if e = l.Execution(ctx, prepared.LogicalRun, prepared.RequestID, prepared.Plan, engine.ID); e != nil {
			return e
		}
	}
	if *detach {
		return encode(map[string]any{"logical_run": *runID, "engine": engine})
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	for {
		if engine.Status != workflows.RunStatusQueued && engine.Status != workflows.RunStatusRunning {
			if e = encode(engine); e != nil {
				return e
			}
			if engine.Status == workflows.RunStatusSucceeded {
				return nil
			}
			if engine.Status == workflows.RunStatusWaiting || engine.Status == workflows.RunStatusInterrupted {
				return fmt.Errorf("%w: %s", errInputWaiting, engine.ID)
			}
			return fmt.Errorf("PR-fix execution %s: %s", engine.ID, engine.Error)
		}
		if e = pause(ctx, 200*time.Millisecond); e != nil {
			return fmt.Errorf("client stopped waiting; logical run %s, execution %s remain retained: %w", *runID, engine.ID, e)
		}
		if e = client.Call(ctx, "GET", "/runs/"+engine.ID, nil, &engine); e != nil {
			return e
		}
	}
}
func loadPRFixConfig(ctx context.Context, root, path string) (clirunner.Config, error) {
	cfg, err := clirunner.LoadConfig(path)
	if err != nil {
		return cfg, err
	}
	roles, err := (&clirunner.Runner{Config: cfg, Workspace: root}).AdmittedRoles(ctx)
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(roles, &cfg)
	return cfg, err
}

func readPRFixJSON(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	const limit = 4 << 20
	data, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return e
	}
	if len(data) > limit {
		return fmt.Errorf("JSON input exceeds 4 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("one JSON document required")
	}
	return nil
}
