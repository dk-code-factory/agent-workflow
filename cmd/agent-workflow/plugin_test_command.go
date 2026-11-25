package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func pluginTestCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	root, asJSON, err := pluginReportFlags("test", args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	report := plugins.Test(ctx, root)
	if !asJSON {
		for _, test := range report.Cases {
			state := "failed"
			if test.Passed {
				state = "passed"
			}
			fmt.Fprintf(stdout, "%s case %s: %s (%d invocations, state %s)\n", test.File, test.Name, state, test.InvocationCount, test.State)
		}
	}
	return writePluginReport(report.CheckReport, report, asJSON, stdout)
}
