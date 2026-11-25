package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func pluginReportFlags(verb string, args []string, stderr io.Writer) (string, bool, error) {
	f := flag.NewFlagSet("plugin "+verb, flag.ContinueOnError)
	f.SetOutput(stderr)
	asJSON := f.Bool("json", false, "emit versioned JSON diagnostics")
	if err := f.Parse(args); err != nil {
		return "", false, err
	}
	if f.NArg() != 1 {
		return "", false, fmt.Errorf("plugin %s requires one DIR; flags must precede arguments", verb)
	}
	return f.Arg(0), *asJSON, nil
}
func writePluginReport(report plugins.CheckReport, payload any, asJSON bool, stdout io.Writer) error {
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(payload); err != nil {
			return err
		}
	} else {
		for _, d := range report.Diagnostics {
			if _, err := fmt.Fprintf(stdout, "%s: %s: %s\n", d.File, d.Code, d.Reason); err != nil {
				return err
			}
		}
		if report.Passed {
			if _, err := fmt.Fprintf(stdout, "Plugin %s checks passed.\n", report.Kind); err != nil {
				return err
			}
		}
	}
	if !report.Passed {
		return fmt.Errorf("plugin %s checks failed (%d diagnostic(s))", report.Kind, len(report.Diagnostics))
	}
	return nil
}
func pluginCheckCommand(args []string, stdout, stderr io.Writer) error {
	root, asJSON, err := pluginReportFlags("check", args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	report := plugins.Check(root)
	return writePluginReport(report, report, asJSON, stdout)
}
