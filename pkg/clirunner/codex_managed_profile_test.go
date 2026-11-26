package clirunner

import (
	"context"
	"encoding/json"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
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

func TestManagedCodexCanonicalTupleAndNilRoleCompatible(t *testing.T) {
	if source, err := managedSourceDescriptor(); err != nil || source.SourceSHA256 == "" || source.Protocol != "agent-workflow-codex-managed/1" {
		t.Fatal("canonical managed source tuple unavailable", err)
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

func TestSDKMixedNativeBrokerAuthorityIsClosedBeforeAdmission(t *testing.T) {
	role := Agent{Provider: "claude-sdk", Tools: []string{"Read"}, MCPGrants: []workflows.ToolGrant{{Server: "engine", Tool: "worker_spawn"}}}
	runner := &Runner{Config: Config{AgentRuntime: "worker", Agents: map[string]Agent{"worker": role}}}
	if _, err := runner.PinRoles(context.Background()); err == nil {
		t.Fatal("mixed native/broker profile pinned")
	}
	if _, err := runner.nativeTaskRuntime(); err == nil {
		t.Fatal("mixed native/broker task admitted")
	}
	if err := runner.ValidateConversationRole("worker"); err == nil {
		t.Fatal("native conversation admitted broker release")
	}
	role.MCPGrants = nil
	runner.Config.Agents["worker"] = role
	if _, err := runner.nativeTaskRuntime(); err != nil {
		t.Fatal("native-only route changed", err)
	}
	role.Tools = nil
	runner.Config.Agents["worker"] = role
	if err := runner.ValidateConversationRole("worker"); err != nil {
		t.Fatal("broker-only conversation rejected", err)
	}
}
