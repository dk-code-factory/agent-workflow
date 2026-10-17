package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	workflowassets "github.com/dkropachev/agent-workflow"
	"github.com/dkropachev/agent-workflow/pkg/clirunner"
	"github.com/dkropachev/agent-workflow/pkg/controller"
	"github.com/dkropachev/agent-workflow/pkg/development"
	"github.com/dkropachev/agent-workflow/pkg/workflows"
)

type values []string

func (v *values) String() string     { return strings.Join(*v, ",") }
func (v *values) Set(s string) error { *v = append(*v, s); return nil }
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}
func command(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: agent-workflow <run|validate|status|list|logs|attempts|cancel|resume|controller|mcp|doctor|init|export|cleanup> [flags] [workflow|run-id]\n\nFlags precede positional arguments. Use <command> --help for details.")
		return nil
	}
	verb := args[0]
	switch verb {
	case "run", "validate", "status", "list", "logs", "attempts", "cancel", "resume", "controller", "doctor", "init", "export", "cleanup", "mcp", "stop", "worker":
	default:
		return fmt.Errorf("unknown command %q", verb)
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	workspace := fs.String("workspace", ".", "workspace containing workflows/")
	configPath := fs.String("config", "agents.yml", "role configuration relative to workspace")
	parallel := fs.Int("parallel", 4, "maximum active executions across jobs and runs")
	timeout := fs.Duration("timeout", 30*time.Minute, "client wait/foreground timeout; 0 disables")
	detach := fs.Bool("detach", false, "return the durable run handle immediately")
	foreground := fs.Bool("foreground", false, "execute in this process with exclusive workspace ownership")
	retry := fs.Bool("retry-uncertain", false, "acknowledge that failed/interrupted effects may be repeated on resume")
	follow := fs.Bool("follow", false, "follow run events until terminal")
	workerOperation := fs.String("operation", "status", "worker operation: spawn, send, status, messages, interrupt, close, resume, list")
	workerRun := fs.String("run", "", "originating run ID")
	workerID := fs.String("worker", "", "worker ID")
	workerRole := fs.String("role", "", "configured worker role")
	workerParent := fs.String("parent", "", "parent worker ID")
	workerTurns := fs.Int("turns", 0, "worker turn budget (default 32)")
	workerRequest := fs.String("request-id", "", "durable mailbox request ID")
	workerPrompt := fs.String("message", "", "mailbox message")
	artifact := fs.String("artifact", "", "export the named stdout/stderr artifact as bytes")
	after := fs.Int64("after", 0, "return events after sequence")
	var inputFlags, secretFlags values
	fs.Var(&inputFlags, "input", "NAME=VALUE (JSON or string), repeatable")
	fs.Var(&secretFlags, "secret-env", "NAME=ENVIRONMENT_VARIABLE, repeatable")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	noArg := verb == "list" || verb == "controller" || verb == "doctor" || verb == "init" || verb == "mcp" || verb == "stop" || verb == "worker"
	if (noArg && fs.NArg() != 0) || (!noArg && fs.NArg() != 1) {
		return fmt.Errorf("%s: incorrect argument count; flags must precede arguments", verb)
	}
	if *parallel < 1 || *parallel > 128 {
		return fmt.Errorf("parallel must be between 1 and 128")
	}
	if *timeout < 0 {
		return fmt.Errorf("timeout must not be negative")
	}
	root, err := filepath.Abs(*workspace)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ".agent-workflow")
	id := fs.Arg(0)
	encode := func(v any) error { enc := json.NewEncoder(stdout); enc.SetIndent("", "  "); return enc.Encode(v) }
	if verb == "worker" {
		client := controller.NewClient(dir)
		method := "GET"
		path := "/runs/" + *workerRun + "/workers"
		var input any
		switch *workerOperation {
		case "spawn":
			method = "POST"
			input = workflows.WorkerSpec{RunID: *workerRun, ParentID: *workerParent, Role: *workerRole, Turns: *workerTurns}
		case "list":
		case "status":
			path += "/" + *workerID
		case "messages":
			path += "/" + *workerID + "/messages"
		case "send":
			path += "/" + *workerID + "/send"
			method = "POST"
			input = controller.SendRequest{RequestID: *workerRequest, Prompt: *workerPrompt}
		case "interrupt", "close":
			path += "/" + *workerID + "/" + *workerOperation
			method = "POST"
		case "resume":
			if !*retry {
				return fmt.Errorf("worker resume requires --retry-uncertain acknowledgement; the interrupted turn is retained without replay")
			}
			path += "/" + *workerID + "/resume?acknowledge=true"
			method = "POST"
		default:
			return fmt.Errorf("unknown worker operation")
		}
		var out any
		if err := client.Call(ctx, method, path, input, &out); err != nil {
			return err
		}
		return encode(out)
	}
	if verb == "stop" {
		client := controller.NewClient(dir)
		var result any
		if err := client.Call(ctx, "POST", "/shutdown", nil, &result); err != nil {
			return err
		}
		return encode(result)
	}
	if verb == "init" {
		return initialize(root, stdout)
	}
	if verb == "status" || verb == "export" || verb == "list" || verb == "logs" || verb == "attempts" || verb == "cleanup" {
		store, err := workflows.OpenSQLite(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		switch verb {
		case "list":
			v, err := store.List(ctx)
			if err != nil {
				return err
			}
			return encode(v)
		case "logs":
			for {
				events, err := store.Events(ctx, id, *after)
				if err != nil {
					return err
				}
				for _, event := range events {
					if err = json.NewEncoder(stdout).Encode(event); err != nil {
						return err
					}
					*after = event.Sequence
				}
				if !*follow {
					return nil
				}
				run, _, err := store.Load(ctx, id)
				if err != nil {
					return err
				}
				if run.Status != workflows.RunStatusRunning && run.Status != workflows.RunStatusQueued && len(events) < 1000 {
					return nil
				}
				if err = pause(ctx, 200*time.Millisecond); err != nil {
					return err
				}
			}
		case "attempts":
			v, err := store.Attempts(ctx, id)
			if err != nil {
				return err
			}
			return encode(v)
		case "cleanup":
			lock, err := controller.Own(dir)
			if err != nil {
				return err
			}
			defer lock.Close()
			return (&development.Runtime{Root: root, Store: store}).Cleanup(ctx, id)
		default:
			if verb == "export" {
				if *artifact != "" {
					b, err := store.Artifact(ctx, id, *artifact)
					if err != nil {
						return err
					}
					_, err = stdout.Write(b)
					return err
				}
				v, err := store.Artifacts(ctx, id)
				if err != nil {
					return err
				}
				return encode(v)
			}
			run, _, err := store.Load(ctx, id)
			if err == nil {
				return encode(run)
			}
			if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
				return fmt.Errorf("invalid run id")
			}
			// Legacy records remain inspectable, but never acquire resume semantics.
			b, legacyErr := os.ReadFile(filepath.Join(dir, "runs", id+".json"))
			if legacyErr != nil {
				return err
			}
			_, err = stdout.Write(b)
			return err
		}
	}
	path := *configPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	cfg, err := clirunner.LoadConfig(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	roles, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	executor := &workflows.Executor{WorkspaceDir: root, Agents: &clirunner.Runner{Config: cfg, Workspace: root}, MaxParallel: *parallel, Roles: roles}
	executor.RoleRunner = func(raw json.RawMessage) (workflows.AgentRunner, error) {
		var pinned clirunner.Config
		if err := json.Unmarshal(raw, &pinned); err != nil {
			return nil, err
		}
		return &clirunner.Runner{Config: pinned, Workspace: root}, nil
	}
	if verb == "validate" {
		if _, err = executor.Load(id); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Valid:", id)
		return nil
	}
	if verb == "doctor" {
		return doctor(ctx, root, cfg, stdout)
	}
	if verb == "controller" {
		store, err := workflows.OpenSQLite(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		executor.Store = store
		executor.Actions = &development.Runtime{Root: root, Store: store, Policy: cfg.Publication}
		executor.ActionFactory = func(raw json.RawMessage) (workflows.ActionRunner, error) {
			var pinned clirunner.Config
			if err := json.Unmarshal(raw, &pinned); err != nil {
				return nil, err
			}
			return &development.Runtime{Root: root, Store: store, Policy: pinned.Publication}, nil
		}
		fmt.Fprintln(stderr, "Controller listening in", dir)
		return (&controller.Server{Executor: executor, Store: store, Dir: dir, Workers: workerManager(store, root)}).Serve(ctx)
	}
	if verb == "mcp" {
		if err = ensureController(ctx, controller.NewClient(dir), root, path, *parallel, stderr); err != nil {
			return err
		}
		return serveMCP(ctx, dir, stdout)
	}
	if verb == "run" && *foreground {
		lock, err := controller.Own(dir)
		if err != nil {
			return err
		}
		defer lock.Close()
		store, err := workflows.OpenSQLite(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		executor.Store = store
		executor.Actions = &development.Runtime{Root: root, Store: store, Policy: cfg.Publication}
		executor.ActionFactory = func(raw json.RawMessage) (workflows.ActionRunner, error) {
			var pinned clirunner.Config
			if err := json.Unmarshal(raw, &pinned); err != nil {
				return nil, err
			}
			return &development.Runtime{Root: root, Store: store, Policy: pinned.Publication}, nil
		}
		if err = store.Recover(ctx); err != nil {
			return err
		}
		req, err := parseStart(id, inputFlags, secretFlags)
		if err != nil {
			return err
		}
		secrets, err := controller.ResolveSecrets(req.SecretRefs)
		if err != nil {
			return err
		}
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		run, runErr := executor.Run(ctx, workflows.RunRequest{Ref: req.Ref, Inputs: req.Inputs, SecretRefs: req.SecretRefs, Secrets: secrets})
		if run != nil {
			if err = encode(run); err != nil {
				return errors.Join(runErr, err)
			}
		}
		return runErr
	}
	client := controller.NewClient(dir)
	if verb == "run" || verb == "resume" {
		if err = ensureController(ctx, client, root, path, *parallel, stderr); err != nil {
			return err
		}
	}
	if verb == "cancel" {
		var v any
		if err = client.Call(ctx, "POST", "/runs/"+id+"/cancel", nil, &v); err != nil {
			return err
		}
		return encode(v)
	}
	if verb == "resume" {
		var v any
		suffix := ""
		if *retry {
			suffix = "?retry_uncertain=true"
		}
		if err = client.Call(ctx, "POST", "/runs/"+id+"/resume"+suffix, nil, &v); err != nil {
			return err
		}
		if *detach {
			return encode(v)
		}
	}
	if verb == "run" {
		req, err := parseStart(id, inputFlags, secretFlags)
		if err != nil {
			return err
		}
		req.RequestID = *workerRequest
		var run workflows.Run
		if err = client.Call(ctx, "POST", "/runs", req, &run); err != nil {
			return err
		}
		id = run.ID
		fmt.Fprintln(stderr, "Admitted run", id)
		if *detach {
			return encode(run)
		}
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	previous := ""
	var progressAfter int64
	for {
		var run workflows.Run
		if err = client.Call(ctx, "GET", "/runs/"+id, nil, &run); err != nil {
			return err
		}
		if run.Status != previous {
			fmt.Fprintf(stderr, "%s run %s %s\n", time.Now().UTC().Format(time.RFC3339), id, run.Status)
			previous = run.Status
		}
		var progress []workflows.Event
		if eventErr := client.Call(ctx, "GET", fmt.Sprintf("/runs/%s/events?after=%d", id, progressAfter), nil, &progress); eventErr == nil {
			for _, event := range progress {
				progressAfter = event.Sequence
				if strings.HasPrefix(event.Type, "step.") {
					var attempt workflows.Attempt
					if json.Unmarshal(event.Data, &attempt) == nil {
						fmt.Fprintf(stderr, "%s %s job=%s step=%s attempt=%s\n", event.Time.Format(time.RFC3339Nano), event.Type, attempt.JobID, attempt.StepID, attempt.ID)
					}
				}
			}
		}
		if run.Status != workflows.RunStatusQueued && run.Status != workflows.RunStatusRunning {
			if err = encode(run); err != nil {
				return err
			}
			if run.Status != workflows.RunStatusSucceeded {
				return fmt.Errorf("run %s: %s", id, run.Error)
			}
			return nil
		}
		if err = pause(ctx, 200*time.Millisecond); err != nil {
			return fmt.Errorf("client stopped waiting; run %s remains admitted: %w", id, err)
		}
	}
}
func parseStart(ref string, inputs, secrets values) (controller.StartRequest, error) {
	req := controller.StartRequest{Ref: ref, Inputs: map[string]any{}, SecretRefs: map[string]string{}}
	for _, a := range inputs {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return req, fmt.Errorf("input must be NAME=VALUE")
		}
		var x any
		if err := json.Unmarshal([]byte(v), &x); err != nil {
			x = v
		}
		req.Inputs[k] = x
	}
	for _, a := range secrets {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" || v == "" {
			return req, fmt.Errorf("secret-env must be NAME=ENVIRONMENT_VARIABLE")
		}
		req.SecretRefs[k] = v
	}
	return req, nil
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func ensureController(ctx context.Context, c *controller.Client, root, path string, parallel int, stderr io.Writer) error {
	var health struct {
		RolesHash string `json:"roles_hash"`
		Parallel  int    `json:"parallel"`
	}
	if c.Call(ctx, "GET", "/health", nil, &health) == nil {
		cfg, err := clirunner.LoadConfig(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		h := sha256.Sum256(raw)
		if health.RolesHash != hex.EncodeToString(h[:]) || health.Parallel != parallel {
			return fmt.Errorf("running controller has different roles or parallel limit; stop it before changing controller configuration")
		}
		return nil
	}
	dir := filepath.Join(root, ".agent-workflow")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "controller.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "controller", "--workspace", root, "--config", path, "--parallel", fmt.Sprint(parallel))
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Stdin = nil
	detachController(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	fmt.Fprintln(stderr, "Starting workspace controller")
	for i := 0; i < 50; i++ {
		if c.Call(ctx, "GET", "/health", nil, nil) == nil {
			return nil
		}
		if err = pause(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("controller did not start; inspect %s", filepath.Join(dir, "controller.log"))
}
func initialize(root string, out io.Writer) error {
	if err := os.MkdirAll(filepath.Join(root, "workflows"), 0755); err != nil {
		return err
	}
	config, err := workflowassets.Examples.ReadFile("agents.yml")
	if err != nil {
		return err
	}
	path := filepath.Join(root, "agents.yml")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(config); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	entries, err := workflowassets.Examples.ReadDir("workflows")
	if err != nil {
		return err
	}
	created, preserved := 0, 0
	for _, entry := range entries {
		data, err := workflowassets.Examples.ReadFile("workflows/" + entry.Name())
		if err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(root, "workflows", entry.Name()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			preserved++
			continue
		}
		if err != nil {
			return err
		}
		if _, err = file.Write(data); err != nil {
			_ = file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		created++
	}
	return json.NewEncoder(out).Encode(map[string]any{"workspace": root, "config_created": true, "workflows_created": created, "workflows_preserved": preserved})
}
