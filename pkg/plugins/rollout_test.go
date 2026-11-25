package plugins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
)

func TestMaintainedPluginAdmitsGeneratedPayloadAndDeclaredResources(t *testing.T) {
	t.Setenv("PATH", "")
	source, err := filepath.Abs(filepath.Join("..", "..", "examples", "workflow-plugin"))
	if err != nil {
		t.Fatal(err)
	}
	checked := Check(source)
	if !checked.Passed {
		t.Fatal(checked.Diagnostics)
	}
	tested := Test(t.Context(), source)
	if !tested.Passed {
		t.Fatal(tested.Diagnostics)
	}
	output := filepath.Join(t.TempDir(), "distribution")
	built, err := Build(source, output, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(output, built.Marketplace, "plugins", built.Name)
	captured, err := Capture(installed)
	if err != nil {
		t.Fatal(err)
	}
	if captured.Digest() != built.PackageDigest {
		t.Fatal("installed payload digest changed")
	}
	admission, err := captured.Admission("brief")
	if err != nil {
		t.Fatal(err)
	}
	if admission.Binding.Workflow != "workflows/brief.yml" {
		t.Fatal(admission.Binding)
	}
	got, err := captured.ReadFile("references/scope.md")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(source, "references", "scope.md"))
	if err != nil || string(got) != string(want) {
		t.Fatal("declared resource changed", err)
	}
	for _, entry := range captured.Manifest().Entries {
		if entry.Path == "fixtures" || entry.Path == "config" {
			t.Fatal("undeclared development sources shipped")
		}
	}
}

func TestMaintainedProviderExamplesSelectWorkspaceRuntimeWithoutHostDiscovery(t *testing.T) {
	t.Setenv("PATH", "")
	for _, tc := range []struct{ file, role, provider, credential string }{
		{"agents.codex.example.yml", "codex", "codex-app-server", "OPENAI_API_KEY"},
		{"agents.claude.example.yml", "task", "claude-sdk", "CLAUDE_CODE_OAUTH_TOKEN"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			config, err := clirunner.LoadConfig(filepath.Join("..", "..", "examples", "workflow-plugin", "config", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			role := config.Agents[config.NativeRole()]
			if config.NativeRole() != tc.role || role.Provider != tc.provider || role.Authentication.CredentialEnv != tc.credential {
				t.Fatalf("wrong workspace provider/credential binding: %+v", config)
			}
		})
	}
}
