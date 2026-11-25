package plugins

import (
	"encoding/json"
	"fmt"
	"strings"

	agentworkflow "github.com/dk-code-factory/agent-workflow"
)

// BootstrapFiles returns fresh package-relative assets for the shared Python
// bootstrap. Builders must preserve the script bytes and hash their final output
// independently of a source capture. The package's host version is independent
// of this exact engine version. No provider or credential configuration is added.
func BootstrapFiles(engineVersion string) (map[string][]byte, error) {
	if !exactVersion(engineVersion) || strings.ContainsAny(engineVersion, "-+") {
		return nil, fmt.Errorf("bootstrap requires an exact engine version")
	}
	script, err := agentworkflow.PluginBootstrap.ReadFile("plugins/agent-workflow/scripts/engine_bootstrap.py")
	if err != nil {
		return nil, err
	}
	const repository = "dk-code-factory/agent-workflow"
	config, err := json.MarshalIndent(struct {
		SchemaVersion int    `json:"schema_version"`
		EngineVersion string `json:"engine_version"`
		Repository    string `json:"repository"`
		Workflow      string `json:"release_workflow"`
	}{1, engineVersion, repository, repository + "/.github/workflows/release.yml"}, "", "  ")
	if err != nil {
		return nil, err
	}
	return map[string][]byte{"scripts/engine_bootstrap.py": script, "compatibility.json": append(config, '\n')}, nil
}
