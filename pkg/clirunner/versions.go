package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const ClaudeSDKVersion = "0.3.311"
const CodexProtocolVersion = "0.164.0"
const BridgeProtocolVersion = 2

func verifySDKBridge(a Agent) error {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(a.Bridge), "node_modules", "@anthropic-ai", "claude-agent-sdk", "package.json"))
	if err != nil {
		return fmt.Errorf("Claude SDK bridge dependencies unavailable; install locked dependencies with bridge setup")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil || metadata.Version != ClaudeSDKVersion {
		return fmt.Errorf("Claude SDK dependency version mismatch; required %s", ClaudeSDKVersion)
	}
	return nil
}

func verifyCodexCLI(ctx context.Context, command string) error {
	if command == "" {
		command = "codex"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, "--version")
	removeProbe, err := prepareVersionProbe(cmd)
	if err != nil {
		return fmt.Errorf("Codex version probe storage unavailable")
	}
	defer removeProbe()
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cleanup, err := process.Supervise(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("Codex version prerequisite unavailable")
	}
	fields := strings.Fields(output.String())
	if len(fields) != 2 || fields[0] != "codex-cli" || fields[1] != CodexProtocolVersion {
		return fmt.Errorf("Codex app-server requires pinned upstream %s", CodexProtocolVersion)
	}
	return nil
}

// Version probes need neither credentials nor a caller-owned working directory.
// Keep even custom compatibility executables inside disposable engine storage.
func prepareVersionProbe(cmd *exec.Cmd) (func(), error) {
	dir, err := os.MkdirTemp("", "agent-workflow-version-")
	if err != nil {
		return nil, err
	}
	cmd.Dir = dir
	cmd.Env = append(providerEnvironment(""), "HOME="+dir, "CODEX_HOME="+dir, "CLAUDE_CONFIG_DIR="+dir)
	return func() { _ = os.RemoveAll(dir) }, nil
}
func verifyCodexHandshake(userAgent string) error {
	pattern := `(^|[/[:space:]])` + regexp.QuoteMeta(CodexProtocolVersion) + `($|[[:space:]();])`
	if !regexp.MustCompile(pattern).MatchString(userAgent) {
		return fmt.Errorf("Codex app-server handshake does not identify pinned upstream %s", CodexProtocolVersion)
	}
	return nil
}
