package clirunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func managedIdentityFixture(t *testing.T) (*ManagedCodexBuildIdentity, managedCodexDescriptor, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("managed manifest capture requires Linux")
	}
	source := managedCodexDescriptor{Schema: 1, Enabled: true, UpstreamSHA: strings.Repeat("a", 40), PatchSHA256: strings.Repeat("b", 64), RecipeSHA256: strings.Repeat("c", 64), OfflineWrapperSHA256: strings.Repeat("d", 64), Protocol: "agent-workflow-codex-managed/1", RustToolchain: "1.95.0", SourceSHA256: strings.Repeat("e", 64)}
	manifest := managedCodexBinaryManifest{BuildID: managedBuildID(source.inputs()), Inputs: source.inputs(), SourceSHA256: source.SourceSHA256, BinaryPath: filepath.Join(t.TempDir(), "original-binary"), BinarySHA256: strings.Repeat("f", 64), BinaryBytes: 100, StartupKind: "standalone-app-server", StartupArgv: managedStartupArgv()}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := captureManagedBuild(path, source)
	if err != nil {
		t.Fatal(err)
	}
	return identity, source, path
}

func TestManagedCodexRetainedManifestCannotRecaptureAuthority(t *testing.T) {
	identity, source, path := managedIdentityFixture(t)
	original := append([]byte(nil), identity.ManifestContents...)
	if err := os.WriteFile(path, []byte(`{"build_id":"replacement"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedIdentity(identity, source); err != nil {
		t.Fatal("retained bytes depended on replaced manifest", err)
	}
	if _, err := captureManagedBuild(path, source); err == nil {
		t.Fatal("replacement manifest granted a new identity")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := validateManagedIdentity(identity, source); err != nil {
		t.Fatal("retained bytes depended on removed manifest", err)
	}
	identity.ManifestContents[0] = '['
	if err := validateManagedIdentity(identity, source); err == nil {
		t.Fatal("changed captured bytes accepted")
	}
	identity.ManifestContents = original
	identity.Inputs.PatchSHA256 = strings.Repeat("0", 64)
	if err := validateManagedIdentity(identity, source); err == nil {
		t.Fatal("changed retained patch tuple accepted")
	}
}

func TestManagedCodexCanonicalTupleAndNilRoleCompatible(t *testing.T) {
	source, sourceErr := managedSourceDescriptor()
	if source.Schema != 1 || source.SourceSHA256 == "" || source.Protocol != "agent-workflow-codex-managed/1" {
		t.Fatal("canonical managed source tuple unavailable", sourceErr)
	}
	if source.Enabled && sourceErr != nil || !source.Enabled && sourceErr == nil {
		t.Fatal("canonical runtime admission does not follow its verified gate", source.Enabled, sourceErr)
	}
	raw, err := json.Marshal(Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ManagedBuild") {
		t.Fatal("nil optional profile changes historical role serialization", string(raw))
	}
	if err := validateManagedBuildRole(Agent{ManagedBuild: &ManagedBuild{}, Provider: "codex-app-server"}); err == nil {
		t.Fatal("workspace can bypass missing retained identity")
	}
	runner := &Runner{Config: Config{Agents: map[string]Agent{"worker": {Provider: "codex-app-server", ManagedBuild: &ManagedBuild{Manifest: "must-not-open"}}}}}
	if roles, err := runner.PinRoles(context.Background()); err == nil || len(roles) != 0 {
		t.Fatal("programmatic admission bypassed missing retained identity")
	}
}

func TestManagedAdmissionCannotReplaceHistoricalOrSelectedAuthority(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		r := &Runner{Config: Config{AgentRuntime: "worker", NativeTaskAdmission: version, Agents: map[string]Agent{"worker": {Provider: "codex-app-server", ManagedBuild: &ManagedBuild{Manifest: "must-not-open"}}}}}
		if err := r.ValidateNativeModel("gpt-5.4"); err == nil {
			t.Fatalf("policy %d admitted disabled/mismatched managed authority", version)
		}
		if _, err := r.AdmitSkillTask(nil); err == nil || !strings.Contains(err.Error(), "selected skill") {
			t.Fatalf("policy %d did not reject managed selected skill before discovery: %v", version, err)
		}
	}
	r := &Runner{Config: Config{AgentRuntime: "worker", NativeTaskAdmission: 2, Agents: map[string]Agent{"worker": {Provider: "codex-app-server"}}}}
	if err := r.ValidateNativeOutput(nil); err == nil {
		t.Fatal("managed admission accepted a stock retained role")
	}
}
