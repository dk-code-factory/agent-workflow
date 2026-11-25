package plugins

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

// BuildResult describes a local distribution. Digests identify different trees:
// SourceDigest is provenance; PackageDigest is Capture(installed plugin).Digest().
type BuildResult struct {
	SchemaVersion int      `json:"schema_version"`
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	EngineVersion string   `json:"engine_version"`
	Targets       []string `json:"targets"`
	Platforms     []string `json:"engine_platforms"`
	PythonMinimum string   `json:"python_minimum"`
	SourceDigest  string   `json:"source_digest"`
	PackageDigest string   `json:"package_digest"`
	Marketplace   string   `json:"marketplace"`
	Archive       string   `json:"archive"`
}

var archiveEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// ValidateBuild checks the same generated distribution plan as Build, entirely
// from captured bytes. It writes nothing and never executes the bootstrap.
func ValidateBuild(captured *CapturedPackage, targets []string) error {
	if captured == nil {
		return fmt.Errorf("plugin build requires a captured project")
	}
	_, _, err := distribution(captured, targets)
	return err
}

// Build captures a declared project once and produces a marketplace directory,
// its ZIP, build.json and SHA256SUMS. The output must not exist. No installation,
// provider, fixture execution or publication is performed.
func Build(source, output string, targets []string) (*BuildResult, error) {
	captured, err := Capture(source)
	if err != nil {
		return nil, err
	}
	files, result, err := distribution(captured, targets)
	if err != nil {
		return nil, err
	}
	if err := writeDistribution(output, files); err != nil {
		return nil, err
	}
	return result, nil
}

func distribution(captured *CapturedPackage, targets []string) (map[string]file, *BuildResult, error) {
	d := captured.Descriptor()
	targets = append([]string(nil), targets...)
	sort.Strings(targets)
	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("plugin build requires --target codex,claude (or one supported host)")
	}
	for i, target := range targets {
		if i > 0 && targets[i-1] == target {
			return nil, nil, fmt.Errorf("duplicate build target %q", target)
		}
		found := false
		for _, host := range d.Hosts {
			found = found || host == target
		}
		if !found || target != "codex" && target != "claude" {
			return nil, nil, fmt.Errorf("target %q is not supported by plugin.yaml", target)
		}
	}
	payload := map[string]file{}
	for _, entry := range captured.Manifest().Entries {
		if strings.ContainsAny(entry.Path, "\r\n") {
			return nil, nil, fmt.Errorf("package checksum paths cannot contain line breaks: %q", entry.Path)
		}
		mode := fs.FileMode(0644)
		var data []byte
		if entry.Kind == "directory" {
			mode = fs.ModeDir | 0755
		} else {
			var err error
			data, err = captured.ReadFile(entry.Path)
			if err != nil {
				return nil, nil, err
			}
			if entry.Mode&0111 != 0 {
				mode = 0755
			}
		}
		payload[entry.Path] = file{data: data, mode: mode}
	}
	for _, reserved := range []string{"plugin.json", ".codex-plugin/plugin.json", ".claude-plugin/plugin.json", "compatibility.json", "scripts/agent-workflow", "scripts/engine_bootstrap.py"} {
		if _, exists := payload[reserved]; exists {
			return nil, nil, fmt.Errorf("generated package path collides with declared content: %s", reserved)
		}
	}
	// Host-discovered sources are transformed deliberately. Keep other sources
	// at their original paths because workflows may also declare them as helpers.
	// All other generated paths are exclusive, including declared resources.
	delete(payload, DescriptorFile)
	for _, skill := range d.Skills {
		binding, _ := captured.Binding(skill.Name)
		if strings.HasPrefix(binding.Path, "skills/") {
			delete(payload, binding.Path)
		}
	}
	for i, sourceSkill := range d.Skills {
		binding, _ := captured.Binding(sourceSkill.Name)
		instructions, _ := captured.ReadFile(binding.Path)
		canonical := path.Join("skills", binding.Name, "SKILL.md")
		if canonical != binding.Path {
			// Keep author instructions beside their declared resources, but outside
			// host SKILL discovery. The entrypoint reads this file before invocation.
			original := binding.Path
			if strings.HasPrefix(original, "skills/") {
				original = path.Join(path.Dir(binding.Path), "workflow-instructions.md")
				if err := addBuildFile(payload, original, instructions, 0644); err != nil {
					return nil, nil, err
				}
			}
			metadata, _ := ReadMetadata(instructions)
			header, _ := yaml.Marshal(metadata)
			instructions = []byte("---\n" + string(header) + "---\n\nRead the developer-owned instructions at package-relative `" + original + "` before running this skill. Resolve their relative resource references from that file's directory.\n")
		}
		instructions = append(bytes.TrimRight(instructions, "\n"), []byte("\n\n"+invocationGuidance(binding.Name))...)
		if err := addBuildFile(payload, canonical, instructions, 0644); err != nil {
			return nil, nil, err
		}
		d.Skills[i].Path, d.Skills[i].Workflow = canonical, binding.Workflow
	}
	for name := range payload {
		if strings.HasPrefix(name, "skills/") && path.Base(name) == "SKILL.md" {
			bound := false
			for _, skill := range d.Skills {
				bound = bound || skill.Path == name
			}
			if !bound {
				return nil, nil, fmt.Errorf("undeclared host skill entrypoint: %s", name)
			}
		}
	}
	bootstrap, err := BootstrapFiles(d.EngineVersion)
	if err != nil {
		return nil, nil, err
	}
	for name, data := range bootstrap {
		if err := addBuildFile(payload, name, data, 0644); err != nil {
			return nil, nil, err
		}
	}
	if err := addBuildFile(payload, "scripts/agent-workflow", []byte(bootstrapEntrypoint), 0755); err != nil {
		return nil, nil, err
	}
	manifest := map[string]any{"name": d.Name, "version": d.Version, "description": "Engine-backed workflow skills for " + d.Name + "."}
	portable := map[string]any{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": d.Name, "version": d.Version, "description": manifest["description"]}
	if err := addBuildJSON(payload, "plugin.json", portable); err != nil {
		return nil, nil, err
	}
	manifest["skills"] = "./skills/"
	for _, target := range targets {
		if err := addBuildJSON(payload, "."+target+"-plugin/plugin.json", manifest); err != nil {
			return nil, nil, err
		}
	}
	// Include parents explicitly so installed Capture and the in-memory final
	// identity cover exactly the same normalized directories and generated files.
	if err := completeBuildDirectories(payload); err != nil {
		return nil, nil, err
	}
	d.Hosts = targets
	d.Resources = buildPaths(payload)
	encoded, err := yaml.Marshal(d)
	if err != nil {
		return nil, nil, err
	}
	if len(encoded) > 1<<20 {
		return nil, nil, fmt.Errorf("generated plugin.yaml exceeds 1 MiB")
	}
	payload[DescriptorFile] = file{data: encoded, mode: 0644}
	final, err := newCapturedPackage(&d, &collector{files: payload})
	if err != nil {
		return nil, nil, err
	}
	files := map[string]file{}
	prefix := path.Join(d.Name, "plugins", d.Name)
	for name, entry := range payload {
		files[path.Join(prefix, name)] = entry
	}
	for _, target := range targets {
		var catalog any
		if target == "codex" {
			catalog = map[string]any{"name": d.Name, "plugins": []any{map[string]any{"name": d.Name, "source": map[string]string{"source": "local", "path": "./plugins/" + d.Name}, "policy": map[string]string{"installation": "AVAILABLE", "authentication": "ON_INSTALL"}, "category": "Productivity"}}}
		} else {
			catalog = map[string]any{"name": d.Name, "owner": map[string]string{"name": d.Name}, "plugins": []any{map[string]string{"name": d.Name, "source": "./plugins/" + d.Name}}}
		}
		catalogPath := path.Join(d.Name, ".claude-plugin/marketplace.json")
		if target == "codex" {
			catalogPath = path.Join(d.Name, ".agents/plugins/marketplace.json")
		}
		if err := addBuildJSON(files, catalogPath, catalog); err != nil {
			return nil, nil, err
		}
	}
	if err := completeBuildDirectories(files); err != nil {
		return nil, nil, err
	}
	archive, err := buildZIP(files)
	if err != nil {
		return nil, nil, err
	}
	result := &BuildResult{SchemaVersion: 1, Name: d.Name, Version: d.Version, EngineVersion: d.EngineVersion, Targets: targets, Platforms: []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"}, PythonMinimum: "3.11", SourceDigest: captured.Digest(), PackageDigest: final.Digest(), Marketplace: d.Name, Archive: d.Name + "-" + d.Version + ".zip"}
	files[result.Archive] = file{data: archive, mode: 0644}
	if err := addBuildJSON(files, "build.json", result); err != nil {
		return nil, nil, err
	}
	var checksums strings.Builder
	for _, name := range buildPaths(files) {
		entry := files[name]
		if !entry.mode.IsDir() {
			sum := sha256.Sum256(entry.data)
			fmt.Fprintf(&checksums, "%x  %s\n", sum, name)
		}
	}
	files["SHA256SUMS"] = file{data: []byte(checksums.String()), mode: 0644}
	// The marketplace adds paths outside the runtime payload (including the
	// checksum/report names). Check their portable graph too. Its ZIP duplicates
	// payload bytes, so the runtime byte bound applies to final above, not here.
	entries := make([]workflows.AssetEntry, 0, len(files))
	for _, name := range buildPaths(files) {
		entry := workflows.AssetEntry{Path: name, Kind: "file", Mode: uint32(files[name].mode.Perm()), SHA256: strings.Repeat("0", 64)}
		if files[name].mode.IsDir() {
			entry.Kind, entry.SHA256 = "directory", ""
		}
		entries = append(entries, entry)
	}
	if err := workflows.ValidateAssetEntries(entries); err != nil {
		return nil, nil, err
	}
	return files, result, nil
}

func addBuildFile(files map[string]file, name string, data []byte, mode fs.FileMode) error {
	if _, exists := files[name]; exists {
		return fmt.Errorf("generated package path collides with declared content: %s", name)
	}
	files[name] = file{data: bytes.Clone(data), mode: mode}
	return nil
}

func addBuildJSON(files map[string]file, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return addBuildFile(files, name, append(data, '\n'), 0644)
}

func buildPaths(files map[string]file) []string {
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

func completeBuildDirectories(files map[string]file) error {
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if entry, exists := files[parent]; exists {
				if !entry.mode.IsDir() {
					return fmt.Errorf("package file/directory collision: %s", parent)
				}
				break
			}
			files[parent] = file{mode: fs.ModeDir | 0755}
		}
	}
	return nil
}

func buildZIP(files map[string]file) ([]byte, error) {
	var buffer bytes.Buffer
	bundle := zip.NewWriter(&buffer)
	paths := buildPaths(files)
	archiveName := func(name string) string {
		if files[name].mode.IsDir() {
			return name + "/"
		}
		return name
	}
	sort.Slice(paths, func(i, j int) bool { return archiveName(paths[i]) < archiveName(paths[j]) })
	for _, name := range paths {
		entry := files[name]
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if entry.mode.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
		}
		header.SetMode(entry.mode)
		// Set DOS fields directly: Modified would add extended host timestamps.
		header.ModifiedDate = 1<<5 | 1
		writer, err := bundle.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write(entry.data); err != nil {
			return nil, err
		}
	}
	if err := bundle.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeDistribution(output string, files map[string]file) (returned error) {
	return writeDistributionUsing(output, files, authoringIO())
}

func writeDistributionUsing(output string, files map[string]file, disk authorIO) (returned error) {
	if output == "" {
		return fmt.Errorf("plugin build requires an output directory")
	}
	var created []createdPath
	undo := func() error { return nil }
	defer func() {
		if returned != nil {
			returned = errors.Join(returned, undo(), rollbackPaths(created))
		}
	}()
	if err := reserveBuildDirectory(output, &created); err != nil {
		return err
	}
	paths := buildPaths(files)
	regular := make(map[string]file)
	// Precreate every owned directory with usable owner permissions, independent
	// of umask, before opening children. The file writer retains exclusive-create
	// and owned-inode rollback checks; it never needs to create masked parents.
	for _, name := range paths {
		if !files[name].mode.IsDir() {
			regular[name] = files[name]
			continue
		}
		if err := checkNewPath(output, name); err != nil {
			return err
		}
		if err := reserveBuildDirectory(filepath.Join(output, filepath.FromSlash(name)), &created); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || !os.SameFile(created[0].info, info) {
		return errors.Join(err, fmt.Errorf("output directory changed identity"))
	}
	// Normalize through the handle that exclusively created each file. A path
	// substituted after writing must never redirect permission changes elsewhere.
	create := disk.create
	ownedFiles := map[string]fs.FileInfo{}
	disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
		f, err := create(name, mode)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(output, name)
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		info, err := f.Stat()
		if err != nil {
			return nil, errors.Join(err, f.Close())
		}
		relative = filepath.ToSlash(relative)
		ownedFiles[relative] = info
		return &buildOutputFile{authorFile: f, mode: mode.Perm(), root: root, name: relative}, nil
	}
	undo, err = disk.writeFiles(output, regular)
	if err != nil {
		return err
	}
	// Anchor directory finalization to the reserved output inode. Reopening a
	// substituted member is safe only after comparing it with our creation receipt.
	currentRoot, err := os.Lstat(output)
	if err != nil || !os.SameFile(created[0].info, currentRoot) {
		return errors.Join(err, fmt.Errorf("output directory changed identity"))
	}
	directories := map[string]createdPath{}
	for _, entry := range created[1:] {
		name, err := filepath.Rel(output, entry.name)
		if err != nil {
			return err
		}
		directories[filepath.ToSlash(name)] = entry
	}
	for i := len(paths) - 1; i >= 0; i-- {
		name := paths[i]
		if !files[name].mode.IsDir() {
			info, err := root.Lstat(name)
			if err != nil || !info.Mode().IsRegular() || !os.SameFile(ownedFiles[name], info) {
				return errors.Join(err, fmt.Errorf("%s changed identity; output retained for inspection", name))
			}
			continue
		}
		if err := finalizeBuildDirectory(root, name, directories[name].info, files[name].mode.Perm()); err != nil {
			return err
		}
	}
	return finalizeBuildDirectory(root, ".", created[0].info, 0755)
}

type buildOutputFile struct {
	authorFile
	mode fs.FileMode
	root *os.Root
	name string
}

func (f *buildOutputFile) Close() error {
	owned, err := f.Stat()
	current, currentErr := f.root.Lstat(f.name)
	if err != nil || currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(owned, current) {
		return errors.Join(err, currentErr, fmt.Errorf("%s changed identity; file permissions retained", f.name), f.authorFile.Close())
	}
	permission, ok := f.authorFile.(interface{ Chmod(fs.FileMode) error })
	if !ok {
		return errors.Join(fmt.Errorf("created output file cannot set permissions through its handle"), f.authorFile.Close())
	}
	return errors.Join(permission.Chmod(f.mode), f.authorFile.Close())
}

func finalizeBuildDirectory(root *os.Root, name string, expected fs.FileInfo, mode fs.FileMode) error {
	directory, err := openSourceFile(root, name)
	if err != nil {
		return err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || !os.SameFile(expected, info) {
		return errors.Join(err, fmt.Errorf("%s changed identity; directory permissions retained", name))
	}
	return directory.Chmod(mode)
}

func reserveBuildDirectory(name string, created *[]createdPath) error {
	if err := os.Mkdir(name, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	*created = append(*created, createdPath{name, info})
	// Chmod the newly reserved directory before traversal: even 0700 passed to
	// Mkdir can lose owner read/write/search bits under an owner-restricting mask.
	return permitBuildDirectory(name, info)
}

const bootstrapEntrypoint = `#!/usr/bin/env python3
"""Use only the bootstrap shipped beside this entrypoint."""
import sys
sys.dont_write_bytecode = True
from pathlib import Path
from engine_bootstrap import main
sys.exit(main(Path(__file__).resolve().parents[1]))
`

func invocationGuidance(skill string) string {
	return fmt.Sprintf(`## Shared workflow invocation and recovery

Resolve PACKAGE_ROOT two directories above this SKILL.md. Use the absolute
PACKAGE_ROOT/scripts/agent-workflow launcher, with each argument passed separately
and paths quoted for the shell. Never import launchers from the target workspace.
Use check to inspect prerequisites/cache; use ensure only when setup is authorized.
The exact engine release is pinned in compatibility.json. No latest or PATH-engine
fallback is allowed. Python 3.11+ and GitHub CLI are prerequisites; host selection
does not select an execution provider. Missing prerequisites need explicit setup,
never implicit login or credential refresh.

Read the bound workflow's on.workflow_call.inputs for names, types, defaults and
requiredness. Obtain the target workspace, requested scope and a stable request ID.
Invoke the launcher with this argument structure (all flags precede the skill):

    exec -- plugin run --plugin-root PACKAGE_ROOT --workspace REPO --request-id ID [--input NAME=VALUE] [--secret-env NAME=ENVIRONMENT_VARIABLE] %s

Repeat --input for each supplied input. A complete JSON value retains its type;
other text is a string. Encode numeric/boolean-looking strings as JSON strings,
such as --input 'label="001"'. Pass secrets only through named environment
references. Permissions, credentials and execution-provider settings belong to
the target workspace's agents.yml; the plugin cannot grant execution authority.

Retain the durable run ID. After a lost acknowledgement, use the same launcher
with exec -- status --workspace REPO --request-id ID to locate the original run.
Do not create a replacement request or retry changed package content. Inspect
status, inputs, logs, attempts and usage before acting. Answer only the current
gate with its pinned evidence and an explicit user decision. Resume the same run
with its retained definitions, runtime settings, completed work and allowances.
Use interrupt for a recoverable pause and cancel only when requested. Repeating
uncertain effects requires explicit authorization; never replenish budgets or
replay completed work. Recovery uses retained assets after upgrades/removal.
`, skill)
}
