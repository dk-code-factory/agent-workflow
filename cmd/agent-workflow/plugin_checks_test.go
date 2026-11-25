package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
)

func TestPluginCheckAndTestCLIReports(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := plugins.Init(root, "sample", "1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"check", "test"} {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := pluginCommand(context.Background(), []string{verb, "--json", root}, &stdout, &stderr); err != nil {
				t.Fatal(err, stdout.String(), stderr.String())
			}
			var report plugins.CheckReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || !report.Passed || report.SchemaVersion != 1 {
				t.Fatal(stdout.String(), err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "skills/sample/SKILL.md"), []byte("invalid skill"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"check", "test"} {
		t.Run(verb+"-broken", func(t *testing.T) {
			var textOut, jsonOut, stderr bytes.Buffer
			if err := pluginCommand(context.Background(), []string{verb, root}, &textOut, &stderr); err == nil {
				t.Fatal("invalid package passed")
			}
			if err := pluginCommand(context.Background(), []string{verb, "--json", root}, &jsonOut, &stderr); err == nil {
				t.Fatal("invalid JSON check passed")
			}
			var report plugins.CheckReport
			if err := json.Unmarshal(jsonOut.Bytes(), &report); err != nil {
				t.Fatal(jsonOut.String(), err)
			}
			if report.Passed || len(report.Diagnostics) != 1 || report.Diagnostics[0].File != "skills/sample/SKILL.md" || !strings.Contains(textOut.String(), report.Diagnostics[0].Reason) {
				t.Fatal(textOut.String(), jsonOut.String())
			}
		})
	}
}
func TestPluginCheckTestFlagsAndHelp(t *testing.T) {
	for _, args := range [][]string{{"check"}, {"test"}, {"check", "one", "two"}, {"test", "--unknown"}, {"check", "directory", "--json"}} {
		var out bytes.Buffer
		if err := pluginCommand(t.Context(), args, &out, &out); err == nil {
			t.Fatal("accepted invalid arguments", args)
		}
	}
	for _, verb := range []string{"check", "test"} {
		var out bytes.Buffer
		if err := pluginCommand(t.Context(), []string{verb, "--help"}, &out, &out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPluginFixtureInvalidCLIReportsLocation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	if err := plugins.Init(root, "sample", "1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixtures/example.yaml"), []byte("schema-version: 1\ncases:\n  - name: wrong-state\n    skill: sample\n    expect: {state: completed, invocation-count: 0}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var textOut, jsonOut, stderr bytes.Buffer
	if err := pluginCommand(t.Context(), []string{"test", root}, &textOut, &stderr); err == nil {
		t.Fatal("invalid fixture passed")
	}
	if err := pluginCommand(t.Context(), []string{"test", "--json", root}, &jsonOut, &stderr); err == nil {
		t.Fatal("invalid JSON fixture passed")
	}
	var report plugins.TestReport
	if err := json.Unmarshal(jsonOut.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Diagnostics) != 1 {
		t.Fatal(report)
	}
	d := report.Diagnostics[0]
	if d.File != "fixtures/example.yaml" || d.Case != "wrong-state" || !strings.Contains(textOut.String(), d.File) || !strings.Contains(textOut.String(), d.Reason) {
		t.Fatal(textOut.String(), jsonOut.String())
	}
}
