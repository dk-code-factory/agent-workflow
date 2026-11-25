package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func agentCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stdout, "Usage: agent-workflow agent convert --from codex|claude --input FILE --output FILE.yaml [--acknowledge-loss FIELD]\n\nConversion is offline. Omitted native options require explicit acknowledgement.")
		return nil
	}
	if args[0] != "convert" {
		return fmt.Errorf("unknown agent command %q", args[0])
	}
	f := flag.NewFlagSet("agent convert", flag.ContinueOnError)
	f.SetOutput(stderr)
	from := f.String("from", "", "native source format: codex or claude")
	input := f.String("input", "", "existing native definition")
	output := f.String("output", "", "new YAML definition (never overwritten)")
	var acknowledged values
	f.Var(&acknowledged, "acknowledge-loss", "exact omitted native field to acknowledge; repeatable")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 || *from == "" || *input == "" || *output == "" {
		return fmt.Errorf("agent convert requires --from, --input and --output, with flags only")
	}
	report, err := workflows.ConvertAgent(*from, *input, *output, acknowledged)
	if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}
