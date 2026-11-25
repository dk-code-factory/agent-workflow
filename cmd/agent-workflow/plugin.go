package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dk-code-factory/agent-workflow/internal/buildinfo"
	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func pluginCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: agent-workflow plugin <init|add-skill> [flags] NAME\n\ninit [--workflow FILE] NAME\nadd-skill --plugin DIR --workflow FILE NAME\n\nFlags precede positional arguments. Scaffolding is offline.")
		return nil
	}
	verb := args[0]
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
