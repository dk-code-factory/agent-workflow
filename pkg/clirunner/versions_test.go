package clirunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexPinnedVersionAndHandshake(t *testing.T) {
	for _, value := range []string{"", "0.160.0", "0.164.01", "0.164.0.1", "0.164.0-dev", "0.164.0+local", "codex_cli_rs/0.162.0"} {
		if err := verifyCodexHandshake(value); err == nil {
			t.Fatalf("accepted unsupported handshake %q", value)
		}
	}
	for _, value := range []string{"0.164.0", "codex_cli_rs/0.164.0 (linux; fixture)"} {
		if err := verifyCodexHandshake(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"0.164.0", "0.160.0", "0.164.01"} {
		command := filepath.Join(t.TempDir(), "codex")
		script := "#!/bin/sh\n[ \"$1\" = --version ] || exit 97\nprintf 'codex-cli " + version + "\\n'\n"
		if err := os.WriteFile(command, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		err := verifyCodexCLI(context.Background(), command)
		if (err == nil) != (version == CodexProtocolVersion) {
			t.Fatalf("version %s: %v", version, err)
		}
	}
}

func TestVersionProbeIsPrivateCredentialFreeAndRemoved(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("GH_TOKEN", "controller-only")
	t.Setenv("EXTERNAL_MCP_TOKEN", "broker-only")
	t.Setenv("ANTHROPIC_API_KEY", "not-needed-for-version")
	command := filepath.Join(root, "probe")
	body := "#!/bin/sh\n[ -z \"$GH_TOKEN$EXTERNAL_MCP_TOKEN$ANTHROPIC_API_KEY\" ] || exit 91\nprintf effect > probe-marker\npwd\n"
	if err := os.WriteFile(command, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	runner := Runner{Workspace: root}
	probe := runner.providerVersion(context.Background(), Agent{Provider: "claude-cli", Command: command})
	if probe == "unavailable" || probe == root {
		t.Fatalf("version probe was not isolated: %s", probe)
	}
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Fatal("version probe directory was retained", err)
	}
	if _, err := os.Stat(filepath.Join(root, "probe-marker")); !os.IsNotExist(err) {
		t.Fatal("version probe wrote into caller workspace", err)
	}
}
