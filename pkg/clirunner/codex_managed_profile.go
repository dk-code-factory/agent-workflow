package clirunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"runtime"

	codexsource "github.com/dk-code-factory/agent-workflow/patches/codex"
)

// ManagedBuild is workspace-owned. Identity is captured during fresh admission
// and retained in the original role snapshot; providers never select it.
type ManagedBuild struct {
	Manifest string                     `yaml:"manifest" json:"manifest"`
	Identity *ManagedCodexBuildIdentity `yaml:"-" json:"identity,omitempty"`
}

type ManagedCodexBuildInputs struct {
	UpstreamSHA          string `json:"upstream_sha"`
	PatchSHA256          string `json:"patch_sha256"`
	RecipeSHA256         string `json:"recipe_sha256"`
	Protocol             string `json:"protocol"`
	Target               string `json:"target"`
	RustToolchain        string `json:"rust_toolchain"`
	OfflineWrapperSHA256 string `json:"offline_wrapper_sha256"`
}

type managedCodexBinaryManifest struct {
	BuildID                string                  `json:"build_id"`
	Inputs                 ManagedCodexBuildInputs `json:"inputs"`
	SourceSHA256           string                  `json:"source_sha256"`
	BinaryPath             string                  `json:"binary_path"`
	BinarySHA256           string                  `json:"binary_sha256"`
	BinaryBytes            int64                   `json:"binary_bytes"`
	StartupKind            string                  `json:"startup_kind"`
	StartupArgv            []string                `json:"startup_argv"`
	BuildReceipt           string                  `json:"build_receipt"`
	VerifiedExecutedBinary bool                    `json:"verified_executed_binary"`
	ConformancePassed      bool                    `json:"conformance_passed"`
	CopiedFrom             string                  `json:"copied_from,omitempty"`
	CopyVerifiedNoHardlink bool                    `json:"copy_verified_no_hardlink,omitempty"`
}

// These bytes survive manifest removal/replacement. A restart must still open
// the original binary and verify the actual executed image before task data.
type ManagedCodexBuildIdentity struct {
	ManifestContents json.RawMessage         `json:"manifest_contents"`
	ManifestSHA256   string                  `json:"manifest_sha256"`
	BuildID          string                  `json:"build_id"`
	Inputs           ManagedCodexBuildInputs `json:"inputs"`
	SourceSHA256     string                  `json:"source_sha256"`
	BinaryPath       string                  `json:"binary_path"`
	BinarySHA256     string                  `json:"binary_sha256"`
	BinaryBytes      int64                   `json:"binary_bytes"`
	StartupKind      string                  `json:"startup_kind"`
	StartupArgv      []string                `json:"startup_argv"`
}

type managedCodexDescriptor struct {
	Schema               int    `json:"schema"`
	Enabled              bool   `json:"enabled"`
	Status               string `json:"status"`
	Protocol             string `json:"protocol"`
	UpstreamTag          string `json:"upstream_tag"`
	UpstreamSHA          string `json:"upstream_sha"`
	RustToolchain        string `json:"rust_toolchain"`
	Patch                string `json:"patch"`
	PatchSHA256          string `json:"patch_sha256"`
	RecipeSHA256         string `json:"recipe_sha256"`
	OfflineWrapper       string `json:"offline_wrapper"`
	OfflineWrapperSHA256 string `json:"offline_wrapper_sha256"`
	UpstreamLockSHA256   string `json:"upstream_lock_sha256"`
	SourceLockSHA256     string `json:"source_lock_sha256"`
	SourceSHA256         string `json:"source_sha256,omitempty"`
}

func managedSourceDescriptor() (managedCodexDescriptor, error) {
	var value managedCodexDescriptor
	if err := strictCodexDecode(codexsource.Descriptor(), &value); err != nil {
		return value, err
	}
	if value.Schema != 1 || !value.Enabled {
		return value, fmt.Errorf("managed Codex source profile remains disabled pending complete native/engine conformance")
	}
	return value, nil
}
func (d managedCodexDescriptor) inputs() ManagedCodexBuildInputs {
	return ManagedCodexBuildInputs{UpstreamSHA: d.UpstreamSHA, PatchSHA256: d.PatchSHA256, RecipeSHA256: d.RecipeSHA256, Protocol: d.Protocol, Target: "x86_64-unknown-linux-gnu", RustToolchain: d.RustToolchain, OfflineWrapperSHA256: d.OfflineWrapperSHA256}
}
func managedBuildID(value ManagedCodexBuildInputs) string {
	encoded, _ := json.Marshal(map[string]string{"upstream_sha": value.UpstreamSHA, "patch_sha256": value.PatchSHA256, "recipe_sha256": value.RecipeSHA256, "protocol": value.Protocol, "target": value.Target, "rust_toolchain": value.RustToolchain, "offline_wrapper_sha256": value.OfflineWrapperSHA256})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
func managedHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func managedStartupArgv() []string {
	return []string{"--config", "project_doc_max_bytes=0", "--config", `cli_auth_credentials_store="file"`}
}

func validateManagedIdentity(value *ManagedCodexBuildIdentity, source managedCodexDescriptor) error {
	if value == nil || value.Inputs != source.inputs() || value.BuildID != managedBuildID(value.Inputs) || !managedHex(source.SourceSHA256, 64) || value.SourceSHA256 != source.SourceSHA256 || !managedHex(value.BinarySHA256, 64) || !filepath.IsAbs(value.BinaryPath) || value.BinaryBytes <= 0 || value.BinaryBytes > 1<<30 || value.StartupKind != "standalone-app-server" || !reflect.DeepEqual(value.StartupArgv, managedStartupArgv()) {
		return fmt.Errorf("managed Codex retained source/build/binary identity differs")
	}
	if !managedHex(value.Inputs.UpstreamSHA, 40) || !managedHex(value.Inputs.PatchSHA256, 64) || !managedHex(value.Inputs.RecipeSHA256, 64) || !managedHex(value.Inputs.OfflineWrapperSHA256, 64) || value.Inputs.Protocol != "agent-workflow-codex-managed/1" {
		return fmt.Errorf("managed Codex source protocol or digest is invalid")
	}
	sum := sha256.Sum256(value.ManifestContents)
	if hex.EncodeToString(sum[:]) != value.ManifestSHA256 {
		return fmt.Errorf("managed Codex retained manifest bytes changed")
	}
	var original managedCodexBinaryManifest
	if err := strictCodexDecode(value.ManifestContents, &original); err != nil {
		return err
	}
	if original.BuildID != value.BuildID || original.Inputs != value.Inputs || original.SourceSHA256 != value.SourceSHA256 || original.BinaryPath != value.BinaryPath || original.BinarySHA256 != value.BinarySHA256 || original.BinaryBytes != value.BinaryBytes || original.StartupKind != value.StartupKind || !reflect.DeepEqual(original.StartupArgv, value.StartupArgv) {
		return fmt.Errorf("managed Codex captured manifest does not match retained identity")
	}
	return nil
}

func captureManagedBuild(path string, source managedCodexDescriptor) (*ManagedCodexBuildIdentity, error) {
	file, err := openManagedManifest(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, fmt.Errorf("managed build manifest is not a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, fmt.Errorf("managed build manifest exceeds bound")
	}
	var manifest managedCodexBinaryManifest
	if err := strictCodexDecode(raw, &manifest); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	identity := &ManagedCodexBuildIdentity{ManifestContents: append(json.RawMessage(nil), raw...), ManifestSHA256: hex.EncodeToString(sum[:]), BuildID: manifest.BuildID, Inputs: manifest.Inputs, SourceSHA256: manifest.SourceSHA256, BinaryPath: manifest.BinaryPath, BinarySHA256: manifest.BinarySHA256, BinaryBytes: manifest.BinaryBytes, StartupKind: manifest.StartupKind, StartupArgv: append([]string(nil), manifest.StartupArgv...)}
	if err := validateManagedIdentity(identity, source); err != nil {
		return nil, err
	}
	return identity, nil
}

func validateManagedBuildRole(agent Agent) error {
	if agent.ManagedBuild == nil {
		return nil
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || agent.Provider != "codex-app-server" || agent.Execution.Mode == "container" || agent.Execution.Network != nil || agent.Execution.NetworkRuntime != "" {
		return fmt.Errorf("managed Codex build requires its proven local Linux amd64 runtime; container/network transports remain unverified")
	}
	if len(agent.Args) > 0 || len(agent.Tools) > 0 {
		return fmt.Errorf("managed broker-only Codex profile cannot admit raw arguments or direct native tool grants")
	}
	source, err := managedSourceDescriptor()
	if err != nil {
		return err
	}
	if err := validateManagedIdentity(agent.ManagedBuild.Identity, source); err != nil {
		return err
	}
	if agent.Command != "" && agent.Command != agent.ManagedBuild.Identity.BinaryPath {
		return fmt.Errorf("managed Codex command differs from its original pinned binary")
	}
	return nil
}
