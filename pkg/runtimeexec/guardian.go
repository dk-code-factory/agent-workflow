package runtimeexec

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
)

const MountedGuardian = "/engine-guardian/guardian"

type GuardianAsset struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Platform string `json:"platform"`
}

// GuardianFor selects the Linux workload binary even when the host is macOS.
// The manifest is part of the release; no download or build occurs at runtime.
func GuardianFor(directory string, image containerapi.Image) (GuardianAsset, error) {
	if err := containerapi.ValidateImage(image); err != nil {
		return GuardianAsset{}, err
	}
	var entries map[string]string
	b, err := os.ReadFile(filepath.Join(directory, "checksums.json"))
	if err != nil {
		return GuardianAsset{}, fmt.Errorf("packaged Linux guardian unavailable: %w", err)
	}
	if json.Unmarshal(b, &entries) != nil {
		return GuardianAsset{}, fmt.Errorf("invalid guardian checksum manifest")
	}
	name := "linux-" + image.Architecture
	g := GuardianAsset{Path: filepath.Join(directory, name), Platform: image.OS + "/" + image.Architecture, SHA256: entries[name]}
	if err = g.Verify(); err != nil {
		return GuardianAsset{}, err
	}
	return g, nil
}
func DefaultGuardianDirectory() string {
	binary, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(binary), "guardians")
}
func (g GuardianAsset) Verify() error {
	info, err := os.Lstat(g.Path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return fmt.Errorf("guardian must be an executable regular file")
	}
	b, err := os.ReadFile(g.Path)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(b)
	if len(g.SHA256) != 64 || hex.EncodeToString(hash[:]) != g.SHA256 {
		return fmt.Errorf("guardian checksum mismatch")
	}
	e, err := elf.Open(g.Path)
	if err != nil {
		return fmt.Errorf("guardian is not a Linux ELF binary: %w", err)
	}
	defer e.Close()
	expected := map[string]elf.Machine{"linux/amd64": elf.EM_X86_64, "linux/arm64": elf.EM_AARCH64}[g.Platform]
	if expected == elf.EM_NONE || e.Machine != expected {
		return fmt.Errorf("guardian platform mismatch")
	}
	for _, p := range e.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("guardian must be statically linked")
		}
	}
	return nil
}
