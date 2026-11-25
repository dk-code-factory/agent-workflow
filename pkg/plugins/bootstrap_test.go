package plugins

import (
	"bytes"
	"encoding/json"
	"testing"

	agentworkflow "github.com/dk-code-factory/agent-workflow"
)

func TestBootstrapFilesPreserveMaintainedSourceAndReleaseAuthority(t *testing.T) {
	files, err := BootstrapFiles("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	source, err := agentworkflow.PluginBootstrap.ReadFile("plugins/agent-workflow/scripts/engine_bootstrap.py")
	if err != nil || !bytes.Equal(source, files["scripts/engine_bootstrap.py"]) {
		t.Fatal("bootstrap source changed", err)
	}
	var config map[string]any
	if err := json.Unmarshal(files["compatibility.json"], &config); err != nil {
		t.Fatal(err)
	}
	if len(config) != 4 || config["engine_version"] != "1.2.3" || config["schema_version"] != float64(1) || config["repository"] != "dk-code-factory/agent-workflow" || config["release_workflow"] != "dk-code-factory/agent-workflow/.github/workflows/release.yml" {
		t.Fatal(config)
	}
	files["scripts/engine_bootstrap.py"][0] = '!'
	fresh, err := BootstrapFiles("1.2.3")
	if err != nil || !bytes.Equal(source, fresh["scripts/engine_bootstrap.py"]) {
		t.Fatal("builder changed shared bootstrap", err)
	}
	for _, version := range []string{"latest", "v1.2.3", "1.2", "01.2.3", "1.2.3-rc.1"} {
		if _, err := BootstrapFiles(version); err == nil {
			t.Fatalf("accepted %q", version)
		}
	}
}
