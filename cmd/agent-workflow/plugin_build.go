package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func pluginBuild(args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("plugin build", flag.ContinueOnError)
	f.SetOutput(stderr)
	target := f.String("target", "", "comma-separated descriptor hosts: codex,claude")
	output := f.String("output", "", "new output directory (must not exist)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 1 || *output == "" || *target == "" {
		return fmt.Errorf("plugin build requires --target codex,claude --output DIR SOURCE; flags must precede SOURCE")
	}
	result, err := plugins.Build(f.Arg(0), *output, strings.Split(*target, ","))
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
