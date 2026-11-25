package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dk-code-factory/agent-workflow/internal/buildinfo"
	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func pluginCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: agent-workflow plugin <init|add-skill|check|test|build|run> [flags] NAME\n\ninit [--workflow FILE] NAME\nadd-skill --plugin DIR --workflow FILE NAME\ncheck [--json] DIR\ntest [--json] DIR\nbuild --target codex,claude --output DIR SOURCE\n\nrun --plugin-root DIR --workspace REPO --request-id ID [--input NAME=VALUE] [--secret-env NAME=ENV] SKILL\n\nFlags precede positional arguments. Authoring, checking, fixture testing and building are offline.")
		return nil
	}
	verb := args[0]
	if verb == "build" {
		return pluginBuild(args[1:], stdout, stderr)
	}
	if verb == "check" {
		return pluginCheckCommand(args[1:], stdout, stderr)
	}
	if verb == "test" {
		return pluginTestCommand(ctx, args[1:], stdout, stderr)

	}
	if verb == "run" {
		return command(ctx, append([]string{"run", "--packaged"}, args[1:]...), stdout, stderr)
	}
	if verb != "init" && verb != "add-skill" {
		return fmt.Errorf("unknown plugin command %q", verb)
	}
	f := flag.NewFlagSet("plugin "+verb, flag.ContinueOnError)
	f.SetOutput(stderr)
	workflow := f.String("workflow", "", "workflow YAML file to import with its local dependency closure")
	var root *string
	if verb == "add-skill" {
		root = f.String("plugin", "", "existing plugin project directory")
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 1 {
		return fmt.Errorf("plugin %s requires one NAME; flags must precede arguments", verb)
	}
	name := f.Arg(0)
	if verb == "init" {
		if err := plugins.Init(name, name, buildinfo.Version, *workflow); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Created workflow plugin", name)
		return nil
	}
	if *root == "" || *workflow == "" {
		return fmt.Errorf("plugin add-skill requires --plugin and --workflow")
	}
	if err := plugins.AddSkill(*root, name, *workflow); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Added workflow skill", name)
	return nil
}
