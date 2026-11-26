package clirunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func managedIdentityFixture(t *testing.T) (*ManagedCodexBuildIdentity, managedCodexDescriptor, string) {
	t.Helper()
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

func TestManagedCodexCanonicalProfileIsClosedAndNilRoleCompatible(t *testing.T) {
	if _, err := managedSourceDescriptor(); err == nil {
		t.Fatal("unproven canonical source profile admitted")
	}
	raw, err := json.Marshal(Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ManagedBuild") {
		t.Fatal("nil optional profile changes historical role serialization", string(raw))
	}
	if err := validateManagedBuildRole(Agent{ManagedBuild: &ManagedBuild{}, Provider: "codex-app-server"}); err == nil {
		t.Fatal("workspace can bypass disabled profile")
	}
	runner := &Runner{Config: Config{Agents: map[string]Agent{"worker": {Provider: "codex-app-server", ManagedBuild: &ManagedBuild{Manifest: "must-not-open"}}}}}
	if roles, err := runner.PinRoles(context.Background()); err == nil || len(roles) != 0 {
		t.Fatal("programmatic admission bypassed disabled profile")
	}
}
