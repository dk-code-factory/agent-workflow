package plugins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const PackageManifestVersion = 1

// PackageEntry describes one captured package-relative path. Kind is "file" or
// "directory"; Mode retains the original permission bits, without file-type bits.
// Directories have zero Size and an empty SHA256, including declared empty dirs.
type PackageEntry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

// PackageManifest is the versioned identity of the captured dependency closure.
// Entries are sorted by their canonical slash-separated paths. Parent directories
// that were not collected as dependencies are not inferred into the manifest.
type PackageManifest struct {
	Version int            `json:"version"`
	Entries []PackageEntry `json:"entries"`
}

// CapturedPackage owns validated descriptor, skill, workflow, agent and resource
// bytes independently of their installation. Accessors never reread source files
// and return copies so callers cannot change the captured identity or content.
type CapturedPackage struct {
	descriptor Descriptor
	files      map[string]file
	manifest   PackageManifest
	digest     string
}

// Capture validates and retains the complete declared package closure in one
// load. Undeclared files and the absolute source location are not part of it.
func Capture(root string) (*CapturedPackage, error) {
	descriptor, captured, err := loadCaptured(root)
	if err != nil {
		return nil, err
	}
	return newCapturedPackage(descriptor, captured)
}

func newCapturedPackage(descriptor *Descriptor, captured *collector) (*CapturedPackage, error) {
	p := &CapturedPackage{
		descriptor: cloneDescriptor(*descriptor),
		files:      make(map[string]file, len(captured.files)),
		manifest:   PackageManifest{Version: PackageManifestVersion, Entries: make([]PackageEntry, 0, len(captured.files))},
	}
	paths := make([]string, 0, len(captured.files))
	for name := range captured.files {
		canonical, err := localPath(name)
		if err != nil || canonical != name || !utf8.ValidString(name) {
			return nil, fmt.Errorf("%q: package paths must be canonical UTF-8 relative paths", name)
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		source := captured.files[name]
		p.files[name] = file{data: bytes.Clone(source.data), mode: source.mode}
		entry := PackageEntry{Path: name, Kind: "directory", Mode: uint32(source.mode.Perm())}
		if !source.mode.IsDir() {
			entry.Kind, entry.Size = "file", int64(len(source.data))
			hash := sha256.Sum256(source.data)
			entry.SHA256 = hex.EncodeToString(hash[:])
		}
		p.manifest.Entries = append(p.manifest.Entries, entry)
	}
	// This struct contains only integers, strings and slices, so encoding cannot
	// fail. Its field order and JSON tags define the version-1 digest encoding.
	encoded, _ := json.Marshal(p.manifest)
	entries := make([]workflows.AssetEntry, 0, len(p.manifest.Entries))
	for _, entry := range p.manifest.Entries {
		entries = append(entries, workflows.AssetEntry{Path: entry.Path, Kind: entry.Kind, Mode: entry.Mode, Size: entry.Size, SHA256: entry.SHA256})
	}
	if err := workflows.ValidateAssetEntries(entries); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	p.digest = hex.EncodeToString(hash[:])
	return p, nil
}

func cloneDescriptor(d Descriptor) Descriptor {
	d.Hosts = slices.Clone(d.Hosts)
	d.Skills = slices.Clone(d.Skills)
	d.Resources = slices.Clone(d.Resources)
	return d
}

// Descriptor returns the validated metadata with its original path spelling.
// ReadFile(DescriptorFile) returns the exact descriptor source bytes.
func (p *CapturedPackage) Descriptor() Descriptor { return cloneDescriptor(p.descriptor) }

// Binding returns a selected skill with canonical package-relative paths.
func (p *CapturedPackage) Binding(name string) (Skill, error) {
	for _, skill := range p.descriptor.Skills {
		if skill.Name == name {
			skill.Path, _ = localPath(skill.Path)
			skill.Workflow, _ = workflows.CanonicalLocalRef(skill.Workflow)
			return skill, nil
		}
	}
	return Skill{}, fmt.Errorf("unknown plugin skill %q", name)
}

// Manifest returns an independent copy of the captured path and content identity.
func (p *CapturedPackage) Manifest() PackageManifest {
	return PackageManifest{Version: p.manifest.Version, Entries: slices.Clone(p.manifest.Entries)}
}

// Digest is the lowercase SHA-256 of json.Marshal(Manifest()). Version 1 uses
// the declared struct field order, numeric permission bits and sorted entries.
// Raw descriptor and skill bytes participate through their file digests, so
// metadata edits change identity even when their parsed meaning is unchanged.
func (p *CapturedPackage) Digest() string { return p.digest }

// Admission adapts this exact validated capture to provider-neutral engine data.
// It never rewalks the installation or reuses a digest for transformed files.
func (p *CapturedPackage) Admission(name string) (*workflows.PackageSource, error) {
	skill, err := p.Binding(name)
	if err != nil {
		return nil, err
	}
	d := p.descriptor
	out := &workflows.PackageSource{Binding: workflows.PackageBinding{Version: 1, Name: d.Name, PackageVersion: d.Version, EngineVersion: d.EngineVersion, Skill: skill.Name, Workflow: skill.Workflow, Digest: p.digest}}
	for _, entry := range p.manifest.Entries {
		source := p.files[entry.Path]
		out.Entries = append(out.Entries, workflows.AssetInput{Path: entry.Path, Mode: source.mode, Data: bytes.Clone(source.data)})
	}
	return out, nil
}

// ReadFile returns a copy of a declared file's captured bytes. It never reads the
// installation; directories and undeclared or escaping paths are rejected.
func (p *CapturedPackage) ReadFile(name string) ([]byte, error) {
	canonical, err := localPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fmt.Errorf("%w: %s", fs.ErrInvalid, err)}
	}
	source, ok := p.files[canonical]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	if source.mode.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	return bytes.Clone(source.data), nil
}
