package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dkropachev/agent-workflow/pkg/clirunner"
	"github.com/dkropachev/agent-workflow/pkg/process"
)

func bridgeSetup(ctx context.Context, args []string, out, diagnostic io.Writer) error {
	if len(args) == 0 || args[0] != "setup" {
		return fmt.Errorf("usage: agent-workflow bridge setup [--directory DIR]")
	}
	flags := flag.NewFlagSet("bridge setup", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	directory := flags.String("directory", "", "packaged bridge directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *directory == "" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		for _, candidate := range []string{filepath.Join(filepath.Dir(executable), "bridge", "claude"), filepath.Join(filepath.Dir(executable), "..", "bridge", "claude"), filepath.Join("bridge", "claude")} {
			if _, err = os.Stat(filepath.Join(candidate, "package-lock.json")); err == nil {
				*directory = candidate
				break
			}
		}
	}
	if *directory == "" {
		return fmt.Errorf("locked bridge package unavailable; specify --directory")
	}
	data, err := os.ReadFile(filepath.Join(*directory, "package.json"))
	if err != nil {
		return err
	}
	var metadata struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil {
		return err
	}
	if metadata.Dependencies["@anthropic-ai/claude-agent-sdk"] != clirunner.ClaudeSDKVersion {
		return fmt.Errorf("bridge package SDK pin does not match this binary")
	}
	version, err := exec.CommandContext(ctx, "node", "--version").Output()
	if err != nil {
		return fmt.Errorf("Node 22+ is required: %w", err)
	}
	major, _ := strconv.Atoi(strings.Split(strings.TrimPrefix(strings.TrimSpace(string(version)), "v"), ".")[0])
	if major < 22 {
		return fmt.Errorf("Node 22+ is required")
	}
	cmd := exec.CommandContext(ctx, "npm", "ci", "--prefix", *directory, "--ignore-scripts", "--no-audit", "--no-fund")
	cmd.Stdout = out
	cmd.Stderr = diagnostic
	cleanup, err := process.Supervise(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	return cmd.Run()
}
