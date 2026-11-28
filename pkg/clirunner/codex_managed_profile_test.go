package clirunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
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
	for _, version := range []int{0, 1, 2, 3} {
		r := &Runner{Config: Config{AgentRuntime: "worker", NativeTaskAdmission: version, Agents: map[string]Agent{"worker": {Provider: "codex-app-server", ManagedBuild: &ManagedBuild{Manifest: "must-not-open"}}}}}
		if err := r.ValidateNativeModel("gpt-5.4"); err == nil {
			t.Fatalf("policy %d admitted disabled/mismatched managed authority", version)
		}
		if err := r.ValidateNativeOutput(nil); err == nil {
			t.Fatalf("policy %d admitted disabled/mismatched managed output authority", version)
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

func TestSDKRolePinningPreservesNativePolicyNarrowing(t *testing.T) {
	for _, variant := range []struct {
		name  string
		tools []string
		want  []string
	}{
		{"read-intersection", []string{"Read", "Write", "WebFetch"}, []string{"Read"}},
		{"empty-ceiling", []string{"Write", "WebFetch"}, []string{}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			root := t.TempDir()
			metadata := filepath.Join(root, "node_modules/@anthropic-ai/claude-agent-sdk/package.json")
			if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metadata, []byte(`{"version":"0.3.322"}`), 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "bridge-launched")
			encodedMarker, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			bridge := filepath.Join(root, "bridge.mjs")
			if err := os.WriteFile(bridge, []byte("import {writeFileSync} from 'node:fs'; writeFileSync("+string(encodedMarker)+", 'launched');\n"), 0600); err != nil {
				t.Fatal(err)
			}
			role := Agent{Provider: "claude-sdk", Bridge: bridge, Tools: variant.tools, MCPGrants: []workflows.ToolGrant{{Server: "engine", Tool: "worker_status"}}}
			runner := &Runner{Workspace: root, Config: Config{AgentRuntime: "worker", Agents: map[string]Agent{"worker": role}}}
			original, err := json.Marshal(runner.Config)
			if err != nil {
				t.Fatal(err)
			}
			roles, err := runner.PinRoles(t.Context())
			if err != nil {
				t.Fatal("broad configured role rejected before task narrowing", err)
			}
			retained, err := runner.RunnerForRoles(roles)
			if err != nil {
				t.Fatal(err)
			}
			pinned := retained.(*Runner)
			if !reflect.DeepEqual(pinned.Config.Agents["worker"], role) {
				t.Fatal("role pinning changed configured capabilities", pinned.Config.Agents["worker"])
			}
			definition := &workflows.AgentDefinition{Name: "validator", ToolPolicy: "read-only"}
			admitted, err := pinned.AdmitNativeTask(definition)
			if err != nil || admitted.NativePolicy == nil || !reflect.DeepEqual(admitted.NativePolicy.NativeTools, variant.want) || definition.NativePolicy != nil {
				t.Fatal("task did not retain its native intersection without mutating input", admitted, err)
			}
			if _, err := pinned.AdmitNativeTask(&workflows.AgentDefinition{Name: "ordinary"}); err != nil {
				t.Fatal("native task without an active broker binding was rejected", err)
			}
			probe := &nativePolicyBrokerProbe{}
			_, err = pinned.RunTurn(t.Context(), workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "worker", Tools: &workflows.InvocationTools{Binding: probe}}}, nil)
			if err == nil || !strings.Contains(err.Error(), "direct native tools with managed broker") || probe.calls.Load() != 0 {
				t.Fatal("actual mixed invocation escaped rejection before effects", err, probe.calls.Load())
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("mixed invocation started its bridge", err)
			}
			current, err := json.Marshal(runner.Config)
			if err != nil || string(current) != string(original) || !reflect.DeepEqual(pinned.Config.Agents["worker"], role) {
				t.Fatal("task admission or mixed rejection changed configured role authority", err)
			}
		})
	}
	role := Agent{Provider: "claude-sdk", Tools: []string{"Read"}}
	runner := &Runner{Config: Config{AgentRuntime: "worker", Agents: map[string]Agent{"worker": role}}}
	if err := runner.ValidateConversationRole("worker"); err == nil {
		t.Fatal("native conversation admitted broker release")
	}
	if _, err := runner.nativeTaskRuntime(); err != nil {
		t.Fatal("native-only route changed", err)
	}
	role.Tools = nil
	role.MCPGrants = []workflows.ToolGrant{{Server: "engine", Tool: "worker_status"}}
	runner.Config.Agents["worker"] = role
	if err := runner.ValidateConversationRole("worker"); err != nil {
		t.Fatal("broker-only conversation rejected", err)
	}
}
