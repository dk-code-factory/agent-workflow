package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentConvertDispatcherOfflineWithoutWorkspace(t *testing.T) {
	t.Setenv("PATH", "")
	t.Chdir(t.TempDir())
	for _, from := range []string{"codex", "claude"} {
		input := filepath.Join(t.TempDir(), "native")
		output := input + ".yaml"
		source := "name='reviewer'\ndescription='Review'\ndeveloper_instructions='Work'\n"
		if from == "claude" {
			source = "---\nname: reviewer\ndescription: Review\n---\nWork\n"
		}
		if err := os.WriteFile(input, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		var out, errout bytes.Buffer
		if err := command(t.Context(), []string{"agent", "convert", "--from", from, "--input", input, "--output", output}, &out, &errout); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), output) {
			t.Fatal("missing output report")
		}
	}
	if _, err := os.Stat("agents.yml"); !os.IsNotExist(err) {
		t.Fatal("created runtime authority")
	}
	if _, err := os.Stat(".agent-workflow"); !os.IsNotExist(err) {
		t.Fatal("initialized workspace runtime")
	}
}
