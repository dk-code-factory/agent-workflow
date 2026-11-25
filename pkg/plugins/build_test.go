package plugins

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func extractBuild(t *testing.T, archive, destination string) {
	t.Helper()
	bundle, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	previous := ""
	for _, entry := range bundle.File {
		if entry.Name <= previous || entry.ModifiedDate != 33 || entry.ModifiedTime != 0 || len(entry.Extra) != 0 || entry.CreatorVersion>>8 != 3 {
			t.Fatalf("nondeterministic ZIP metadata: %+v", entry.FileHeader)
		}
		previous = entry.Name
		if path.IsAbs(entry.Name) || strings.Contains(entry.Name, "..") || strings.Contains(entry.Name, "\\") {
			t.Fatal("unsafe ZIP path", entry.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(entry.Name))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		stream, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, entry.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, entry.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildBothHostsReproducibleCapturedIdentity(t *testing.T) {
	source, d, originalFiles := capturedFixture(t)
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	before, err := Capture(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "") // Build cannot depend on host CLIs, Python or providers.
	result, err := Build(source, first, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceDigest != before.Digest() || result.SourceDigest == result.PackageDigest || result.Version == result.EngineVersion {
		t.Fatalf("incorrect provenance/version identity: %+v", result)
	}
	// Metadata unrelated to admitted content never enters artifacts.
	if err := filepath.WalkDir(source, func(name string, e fs.DirEntry, err error) error {
		if err == nil {
			err = os.Chtimes(name, time.Unix(1_000_000, 0), time.Unix(1_000_000, 0))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(source, second, []string{"claude", "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(first, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(first, name)
		a, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(second, rel))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("rebuild changed %s: %v", rel, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "installation with spaces")
	extractBuild(t, filepath.Join(first, result.Archive), extracted)
	root := filepath.Join(extracted, result.Marketplace, "plugins", d.Name)
	installed, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Digest() != result.PackageDigest {
		t.Fatalf("runtime identity differs: got %s want %s", installed.Digest(), result.PackageDigest)
	}
	for name, expected := range originalFiles {
		if name == DescriptorFile || strings.HasSuffix(name, "SKILL.md") {
			continue
		}
		data, err := installed.ReadFile(name)
		if err != nil || string(data) != expected {
			t.Fatalf("lost original dependency %s: %v", name, err)
		}
	}
	for _, name := range []string{"undeclared.txt", "workflows/undeclared.yml", "build.json", "SHA256SUMS"} {
		if _, err := installed.ReadFile(name); err == nil {
			t.Fatal("unexpected runtime payload", name)
		}
	}
	for _, host := range []string{"codex", "claude"} {
		manifest, err := installed.ReadFile("." + host + "-plugin/plugin.json")
		if err != nil || !bytes.Contains(manifest, []byte(`"name": "demo"`)) || !bytes.Contains(manifest, []byte(`"skills": "./skills/"`)) {
			t.Fatalf("bad %s manifest: %s %v", host, manifest, err)
		}
	}
	for _, name := range []string{".agents/plugins/marketplace.json", ".claude-plugin/marketplace.json"} {
		catalog, err := os.ReadFile(filepath.Join(extracted, result.Marketplace, name))
		if err != nil || !bytes.Contains(catalog, []byte(`"name": "demo"`)) || !bytes.Contains(catalog, []byte(`"path": "./plugins/demo"`)) && !bytes.Contains(catalog, []byte(`"source": "./plugins/demo"`)) {
			t.Fatalf("bad catalog %s: %s %v", name, catalog, err)
		}
	}
	bootstrap, _ := BootstrapFiles(d.EngineVersion)
	actualBootstrap, _ := installed.ReadFile("scripts/engine_bootstrap.py")
	if !bytes.Equal(actualBootstrap, bootstrap["scripts/engine_bootstrap.py"]) {
		t.Fatal("maintained bootstrap changed")
	}
	checksums, _ := os.ReadFile(filepath.Join(first, "SHA256SUMS"))
	for _, line := range strings.Split(strings.TrimSpace(string(checksums)), "\n") {
		digest, name, _ := strings.Cut(line, "  ")
		data, err := os.ReadFile(filepath.Join(first, name))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
			t.Fatal("incorrect checksum", line, err)
		}
	}
}

func TestBuildRemapsInstructionsWithoutExtraHostSkills(t *testing.T) {
	source, d, _ := capturedFixture(t)
	original := "authoring/skill/SKILL.md"
	text := "---\nname: demo\ndescription: Custom source\n---\n\nRead [notes](notes.md) and keep developer text.\n"
	put(t, source, original, text)
	put(t, source, "authoring/skill/notes.md", "declared context")
	d.Skills[0].Path = original
	d.Resources = append(d.Resources, "authoring/skill/notes.md")
	writeBuildDescriptor(t, source, d)
	captured, err := Capture(source)
	if err != nil {
		t.Fatal(err)
	}
	files, result, err := distribution(captured, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got := files["demo/plugins/demo/"+original].data; string(got) != text {
		t.Fatalf("changed original source: %s", got)
	}
	entry := string(files["demo/plugins/demo/skills/demo/SKILL.md"].data)
	if !strings.Contains(entry, "Read the developer-owned instructions") || !strings.Contains(entry, original) || !strings.Contains(entry, "exec -- plugin run") {
		t.Fatal("incomplete generated instructions", entry)
	}
	output := filepath.Join(t.TempDir(), "distribution")
	if err := writeDistribution(output, files); err != nil {
		t.Fatal(err)
	}
	installed, err := Capture(filepath.Join(output, "demo/plugins/demo"))
	if err != nil || installed.Digest() != result.PackageDigest {
		t.Fatal("remap did not preserve final runtime identity", err)
	}
	if _, err := os.Stat(filepath.Join(output, "demo/plugins/demo/.codex-plugin/plugin.json")); !os.IsNotExist(err) {
		t.Fatal("unrequested host generated", err)
	}
}

func writeBuildDescriptor(t *testing.T, root string, d Descriptor) {
	t.Helper()
	data, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, DescriptorFile, string(data))
}

func TestBuildPreservesInstructionSourcesUsedAsWorkflowHelpers(t *testing.T) {
	for _, helper := range []string{"authoring/SKILL.md", "authoring"} {
		t.Run(helper, func(t *testing.T) {
			source, d, _ := capturedFixture(t)
			original := "workflows/authoring/SKILL.md"
			instructions := string(skillText("demo")) + "\nRead [notes](notes.md).\n"
			put(t, source, original, instructions)
			put(t, source, "workflows/authoring/notes.md", "relative instruction context")
			put(t, source, "workflows/main.yml", "jobs:\n  task:\n    container:\n      image: ubuntu:24.04\n      files: ["+helper+"]\n    steps:\n      - run: cat authoring/SKILL.md\n")
			d.Skills[0].Path = original
			d.Resources = append(d.Resources, "workflows/authoring/notes.md")
			writeBuildDescriptor(t, source, d)
			output := filepath.Join(t.TempDir(), "built")
			result, err := Build(source, output, []string{"codex", "claude"})
			if err != nil {
				t.Fatal(err)
			}
			extracted := filepath.Join(t.TempDir(), "installed with spaces")
			extractBuild(t, filepath.Join(output, result.Archive), extracted)
			installed, err := Capture(filepath.Join(extracted, "demo/plugins/demo"))
			if err != nil || installed.Digest() != result.PackageDigest {
				t.Fatal("required helper lost during instruction remapping", err)
			}
			if data, err := installed.ReadFile(original); err != nil || string(data) != instructions {
				t.Fatalf("original helper bytes changed: %s %v", data, err)
			}
			entry, _ := installed.ReadFile("skills/demo/SKILL.md")
			if !bytes.Contains(entry, []byte("`"+original+"`")) {
				t.Fatal("entrypoint lost original instruction context")
			}
			count := 0
			for _, entry := range installed.Manifest().Entries {
				if strings.HasPrefix(entry.Path, "skills/") && path.Base(entry.Path) == "SKILL.md" {
					count++
				}
			}
			if count != 1 {
				t.Fatal("extra host-discovered skill", count)
			}
		})
	}
}

func TestBuildSkillRemappingDoesNotDependOnSourceOrder(t *testing.T) {
	source, d, _ := capturedFixture(t)
	d.Skills = []Skill{
		{Name: "second", Path: "skills/demo/SKILL.md", Workflow: "workflows/main.yml"},
		{Name: "demo", Path: "skills/second/SKILL.md", Workflow: "workflows/second.yml"},
	}
	put(t, source, "skills/demo/SKILL.md", string(skillText("second")))
	put(t, source, "skills/second/SKILL.md", string(skillText("demo")))
	put(t, source, "workflows/second.yml", "jobs: {main: {steps: [{run: echo second, shell: sh}]}}\n")
	writeBuildDescriptor(t, source, d)
	output := filepath.Join(t.TempDir(), "built")
	result, err := Build(source, output, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := Capture(filepath.Join(output, "demo/plugins/demo"))
	if err != nil || installed.Digest() != result.PackageDigest {
		t.Fatal("crossed source paths failed final admission", err)
	}
	for _, name := range []string{"demo", "second"} {
		binding, _ := installed.Binding(name)
		if binding.Path != "skills/"+name+"/SKILL.md" {
			t.Fatal("incorrect host identity", binding)
		}
	}
}

func TestBuildRejectsHostInstructionResourceOverlapBeforeOutput(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		for _, declaration := range []string{"helper file", "helper directory", "resource file", "resource directory"} {
			t.Run(fmt.Sprintf("canonical=%v/%s", canonical, declaration), func(t *testing.T) {
				source, d, _ := capturedFixture(t)
				name, directory := "demo", "skills/source/workflows"
				if canonical {
					name, directory = "workflows", "skills/workflows"
				}
				original := path.Join(directory, "SKILL.md")
				text := string(skillText(name))
				put(t, source, original, text)
				workflow := path.Join(directory, "main.yml")
				helper := ""
				switch declaration {
				case "helper file":
					helper = "    container: {image: ubuntu:24.04, files: [SKILL.md]}\n"
				case "helper directory":
					helper = "    container: {image: ubuntu:24.04, files: [.]}\n"
				case "resource file":
					d.Resources = append(d.Resources, original)
				case "resource directory":
					d.Resources = append(d.Resources, directory)
				}
				put(t, source, workflow, "jobs:\n  task:\n"+helper+"    steps:\n      - run: echo run\n        shell: sh\n")
				d.Skills = []Skill{{Name: name, Path: original, Workflow: workflow}}
				writeBuildDescriptor(t, source, d)
				captured, err := Capture(source)
				if err != nil {
					t.Fatal("valid runtime project was rejected", err)
				}
				output := filepath.Join(t.TempDir(), "must-not-exist")
				_, buildErr := Build(source, output, []string{"codex", "claude"})
				if buildErr == nil || !strings.Contains(buildErr.Error(), "move its source outside skills/") {
					t.Fatal("missing actionable generated-layout collision", buildErr)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("created incomplete distribution", err)
				}
				if data, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(original))); err != nil || string(data) != text {
					t.Fatal("changed source helper bytes", err)
				}
				if err := os.RemoveAll(source); err != nil {
					t.Fatal(err)
				}
				if checkErr := ValidateBuild(captured, []string{"codex", "claude"}); checkErr == nil || checkErr.Error() != buildErr.Error() {
					t.Fatal("captured provenance did not retain resource roles", checkErr)
				}
			})
		}
	}
}

func TestBuildImportedStandaloneRootHelpersRoundTrip(t *testing.T) {
	source, _ := rootHelperCaptureFixture(t)
	output := filepath.Join(t.TempDir(), "distribution")
	result, err := Build(source, output, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "installed with spaces")
	extractBuild(t, filepath.Join(output, result.Archive), extracted)
	root := filepath.Join(extracted, "demo/plugins/demo")
	installed, err := Capture(root)
	if err != nil || installed.Digest() != result.PackageDigest {
		t.Fatal("imported root helpers changed runtime identity after extraction", err)
	}
	prefix := "imports/demo/workflows"
	var helperPaths []string
	for _, entry := range installed.Manifest().Entries {
		if entry.Path == "." {
			t.Fatal("structural installation root entered payload")
		}
		if entry.Path == prefix || strings.HasPrefix(entry.Path, prefix+"/") {
			helperPaths = append(helperPaths, entry.Path)
			wantMode := uint32(0644)
			if entry.Kind == "directory" || strings.HasSuffix(entry.Path, "/run.sh") {
				wantMode = 0755
			}
			if entry.Mode != wantMode {
				t.Fatalf("wrong normalized imported mode: %+v", entry)
			}
		}
	}
	want := []string{prefix, prefix + "/helpers", prefix + "/helpers/empty", prefix + "/helpers/run.sh", prefix + "/main.yml"}
	if !reflect.DeepEqual(helperPaths, want) {
		t.Fatalf("root helper closure lost children or acquired transport metadata: %v", helperPaths)
	}
	before, _ := os.ReadFile(filepath.Join(source, prefix, "main.yml"))
	after, err := installed.ReadFile(prefix + "/main.yml")
	if err != nil || !bytes.Equal(before, after) || !bytes.Contains(after, []byte("files: [.]")) {
		t.Fatalf("rewrote imported helper declaration: %s %v", after, err)
	}
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0755) })
	readonly, err := Capture(root)
	if err != nil || readonly.Digest() != result.PackageDigest {
		t.Fatal("transport root permissions changed extracted payload identity", err)
	}
}

func TestBuildRejectsIncompleteOrCollidingOutput(t *testing.T) {
	for _, scenario := range []string{"missing", "native", "metadata", "manifest", "other host manifest", "bootstrap", "ancestor", "case", "discovery", "targets", "duplicate targets", "engine prerelease", "line break", "marketplace collision"} {
		t.Run(scenario, func(t *testing.T) {
			source, d, _ := capturedFixture(t)
			targets := []string{"codex"}
			switch scenario {
			case "missing":
				d.Resources = append(d.Resources, "missing")
			case "native":
				put(t, source, "workflows/nested/child.yml", "on: {workflow_call: {}}\njobs: {task: {steps: [{uses: agent/run, with: {agent: agents/task.toml, prompt: hello}}]}}\n")
				put(t, source, "workflows/nested/agents/task.toml", "model = 'codex'")
			case "metadata":
				put(t, source, "skills/demo/SKILL.md", "---\nname: different\ndescription: invalid\n---\nInstructions\n")
			case "manifest", "other host manifest", "bootstrap", "ancestor", "case", "discovery", "line break":
				name := map[string]string{"manifest": "plugin.json", "other host manifest": ".claude-plugin/plugin.json", "bootstrap": "scripts/engine_bootstrap.py", "ancestor": "scripts", "case": "Scripts/data", "discovery": "skills/unbound/SKILL.md", "line break": "assets/line\nbreak"}[scenario]
				put(t, source, name, "declared collision")
				d.Resources = append(d.Resources, name)
			case "targets":
				targets = []string{"unknown"}
			case "duplicate targets":
				targets = []string{"codex", "codex"}
			case "engine prerelease":
				d.EngineVersion = "1.2.3-pre"
			case "marketplace collision":
				d.Name = "sha256sums"
			}
			writeBuildDescriptor(t, source, d)
			output := filepath.Join(t.TempDir(), "must-not-exist")
			if _, err := Build(source, output, targets); err == nil {
				t.Fatal("accepted invalid package")
			} else if scenario == "native" && !strings.Contains(err.Error(), "agent convert") {
				t.Fatal("missing native conversion guidance", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("advertised incomplete output", err)
			}
		})
	}
}

func TestBuildDirectoryModesIgnoreUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("owner permission denial requires an unprivileged user")
	}
	if source := os.Getenv("WORKFLOW_BUILD_TEST_SOURCE"); source != "" {
		if _, err := Build(source, os.Getenv("WORKFLOW_BUILD_TEST_OUTPUT"), []string{"codex"}); err != nil {
			t.Fatal(err)
		}
		return
	}
	source, _, _ := capturedFixture(t)
	baseline := map[string]string{}
	for _, mask := range []string{"077", "0100", "0200", "0400", "0700", "0777"} {
		t.Run(mask, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "restricted")
			// Change umask only in a bounded disposable child, never this process.
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `umask "$1"; shift; exec "$@"`, "sh", mask, os.Args[0], "-test.run=^TestBuildDirectoryModesIgnoreUmask$", "-test.count=1")
			cmd.Env = append(os.Environ(), "WORKFLOW_BUILD_TEST_SOURCE="+source, "WORKFLOW_BUILD_TEST_OUTPUT="+output)
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("restricted umask build: %s %v", data, err)
			}
			if err := filepath.WalkDir(output, func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				want := fs.FileMode(0644)
				if entry.IsDir() || info.Mode()&0111 != 0 {
					want = 0755
				}
				if info.Mode().Perm() != want {
					t.Fatalf("not normalized: %s %v", name, info.Mode())
				}
				if !entry.IsDir() {
					data, err := os.ReadFile(name)
					if err != nil {
						return err
					}
					rel, _ := filepath.Rel(output, name)
					if mask == "077" {
						baseline[rel] = string(data)
					} else if baseline[rel] != string(data) {
						t.Fatalf("umask changed artifact bytes: %s", rel)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuildRollbackPreservesConcurrentOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "failed")
	files := map[string]file{"nested": {mode: fs.ModeDir | 0755}, "nested/payload": {data: []byte("owned"), mode: 0644}}
	disk := authoringIO()
	create := disk.create
	disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
		f, err := create(name, mode)
		if err != nil {
			return f, err
		}
		return &failedAuthorFile{File: f.(*os.File), write: true, onWrite: func() {
			put(t, output, "nested/keep", "concurrent unrelated file")
		}}, nil
	}
	err := writeDistributionUsing(output, files, disk)
	if err == nil || !strings.Contains(err.Error(), "rollback newly created paths") {
		t.Fatalf("missing retained-path diagnostic: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(output, "nested/keep")); err != nil || string(data) != "concurrent unrelated file" {
		t.Fatalf("removed unrelated output: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(output, "nested/payload")); !os.IsNotExist(err) {
		t.Fatalf("retained partially written owned file: %v", err)
	}
}

func TestBuildMetadataNeverFollowsReplacedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks and permissions")
	}
	for _, replacement := range []string{"file", "directory", "root"} {
		t.Run(replacement, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "built")
			outside := t.TempDir()
			put(t, outside, "payload", "unrelated private content")
			for name, mode := range map[string]fs.FileMode{outside: 0700, filepath.Join(outside, "payload"): 0600} {
				if err := os.Chmod(name, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(name, archiveEpoch, archiveEpoch); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]file{"nested": {mode: fs.ModeDir | 0755}, "nested/payload": {data: []byte("owned"), mode: 0644}}
			disk := authoringIO()
			create := disk.create
			disk.create = func(name string, mode fs.FileMode) (authorFile, error) {
				f, err := create(name, mode)
				if err != nil {
					return f, err
				}
				return &failedAuthorFile{File: f.(*os.File), onWrite: func() {
					target, link := name, filepath.Join(outside, "payload")
					if replacement == "directory" {
						target, link = filepath.Join(output, "nested"), outside
					} else if replacement == "root" {
						target, link = output, outside
					}
					if err := os.Rename(target, target+"-owned"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(link, target); err != nil {
						t.Fatal(err)
					}
				}}, nil
			}
			if err := writeDistributionUsing(output, files, disk); err == nil {
				t.Fatal("accepted replaced output")
			}
			for name, want := range map[string]fs.FileMode{outside: 0700, filepath.Join(outside, "payload"): 0600} {
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != want || !info.ModTime().Equal(archiveEpoch) {
					t.Fatalf("modified external metadata: %s %v %v", name, info, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(outside, "payload")); err != nil || string(data) != "unrelated private content" {
				t.Fatalf("modified external content: %s %v", data, err)
			}
		})
	}
}

func TestBuildReservationRejectsReplacedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlinks and permissions")
	}
	name, outside := filepath.Join(t.TempDir(), "owned"), t.TempDir()
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, name+"-owned"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, name); err != nil {
		t.Fatal(err)
	}
	if err := permitBuildDirectory(name, info); err == nil {
		t.Fatal("accepted replaced reservation")
	}
	if current, err := os.Stat(outside); err != nil || current.Mode().Perm() != 0755 {
		t.Fatalf("changed unrelated directory permissions: %v %v", current, err)
	}
}

func TestBuildCapturedSnapshotAndExistingDestination(t *testing.T) {
	source, _, _ := capturedFixture(t)
	captured, err := Capture(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBuild(captured, []string{"codex", "claude"}); err != nil {
		t.Fatal("build reread source", err)
	}
	output := t.TempDir()
	put(t, output, "unrelated", "preserve")
	files, _, err := distribution(captured, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDistribution(output, files); err == nil {
		t.Fatal("overwrote existing output")
	}
	entries, _ := os.ReadDir(output)
	if len(entries) != 1 || entries[0].Name() != "unrelated" {
		t.Fatal("changed existing output")
	}
}

func TestBuildRelocatedReadonlyLauncherUsesPackageBootstrap(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("bootstrap supports Linux and macOS")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3.11+ required for bootstrap checks")
	}
	source, _, _ := capturedFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	result, err := Build(source, output, []string{"codex", "claude"})
	if err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "relocated read only plugin")
	extractBuild(t, filepath.Join(output, result.Archive), extracted)
	root := filepath.Join(extracted, "demo/plugins/demo")
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0755)
			}
			return err
		})
	})
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := fs.FileMode(0444)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() || info.Mode()&0111 != 0 {
			mode = 0555
		}
		return os.Chmod(name, mode)
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := Capture(root)
	hostile := t.TempDir()
	put(t, hostile, "engine_bootstrap.py", "raise RuntimeError('imported target workspace')")
	put(t, hostile, "compatibility.json", "{}")
	tools := t.TempDir()
	put(t, tools, "gh", "#!"+python+"\nimport sys\nif sys.argv[1:] == ['attestation', 'verify', '--help']: print('--source-ref --source-digest --signer-digest --signer-workflow --bundle')\nelse: sys.exit('unexpected network operation')\n")
	if err := os.Chmod(filepath.Join(tools, "gh"), 0755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	cmd := exec.Command(python, filepath.Join(root, "scripts/agent-workflow"), "check")
	cmd.Dir = hostile
	cmd.Env = append(os.Environ(), "PATH="+tools, "PYTHONPATH="+hostile, "XDG_CACHE_HOME="+cache)
	data, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(data, []byte(`"prerequisites": "ok"`)) {
		t.Fatalf("relocated launcher required Codex or imported caller content: %s %v", data, err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("check wrote cache", err)
	}
	// A local fake candidate verifies setup/exec without contacting any provider,
	// host API or GitHub. A second exec must reuse the verified candidate cache.
	archive, digest := fakeBuildEngine(t, python)
	args := []string{filepath.Join(root, "scripts/agent-workflow"), "exec", "--candidate-archive", archive, "--sha256", digest, "--", "plugin", "run", "--plugin-root", root, "--workspace", hostile, "--request-id", "stable-request", "--input", `label="001"`, "demo"}
	for i := 0; i < 2; i++ {
		candidate := exec.Command(python, args...)
		candidate.Dir, candidate.Env = hostile, cmd.Env
		var out, errout bytes.Buffer
		candidate.Stdout, candidate.Stderr = &out, &errout
		err := candidate.Run()
		exit, ok := err.(*exec.ExitError)
		var received []string
		decodeErr := json.Unmarshal(out.Bytes(), &received)
		if !ok || exit.ExitCode() != 7 || decodeErr != nil || !reflect.DeepEqual(received, args[7:]) || !strings.Contains(errout.String(), "UNCERTIFIED CANDIDATE") || !strings.Contains(errout.String(), "fake engine stderr") {
			t.Fatalf("candidate exec changed arguments/streams/exit: %s %s %v %v", out.Bytes(), errout.Bytes(), err, decodeErr)
		}
	}
	// A still-available archive must not silently replace a corrupt retained
	// installation. Reinstalling every time would incorrectly pass the loop above.
	cachedEngine := filepath.Join(cache, "agent-workflow", "candidate", "0.2.2", runtime.GOOS+"-"+runtime.GOARCH+"-"+digest, "distribution", "agent-workflow")
	if err := os.WriteFile(cachedEngine, []byte("corrupt cached engine"), 0755); err != nil {
		t.Fatal(err)
	}
	poisoned := exec.Command(python, args...)
	poisoned.Dir, poisoned.Env = hostile, cmd.Env
	if data, err := poisoned.CombinedOutput(); err == nil || !bytes.Contains(data, []byte("Cached distribution is corrupt")) {
		t.Fatalf("launcher did not verify retained cache: %s %v", data, err)
	}
	after, err := Capture(root)
	if err != nil || !reflect.DeepEqual(before.Manifest(), after.Manifest()) {
		t.Fatal("launcher mutated package", err)
	}
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if strings.Contains(name, "__pycache__") {
			t.Fatal("launcher generated bytecode", name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func fakeBuildEngine(t *testing.T, python string) (string, string) {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("unsupported fake candidate platform")
	}
	build := map[string]string{"version": "0.2.2", "os": runtime.GOOS, "arch": runtime.GOARCH, "commit": strings.Repeat("a", 40), "source_fingerprint": strings.Repeat("b", 64), "dirty": "true"}
	metadata, _ := json.Marshal(build)
	code := []byte("#!" + python + "\nimport json,sys\nif sys.argv[1:] == ['version']: print(" + fmt.Sprintf("%q", string(metadata)) + ")\nelse:\n print(json.dumps(sys.argv[1:])); print('fake engine stderr',file=sys.stderr); sys.exit(7)\n")
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	prefix := fmt.Sprintf("agent-workflow-0.2.2-%s-%s/", runtime.GOOS, runtime.GOARCH)
	files := map[string]file{"build.json": {data: metadata, mode: 0644}}
	for _, helper := range []string{"agent-workflow", "agent-workflow-runtime", "agent-workflow-guardian", "workflow-conformance"} {
		files[helper] = file{data: code, mode: 0755}
	}
	for _, name := range buildPaths(files) {
		entry := files[name]
		if err := archive.WriteHeader(&tar.Header{Name: prefix + name, Size: int64(len(entry.data)), Mode: int64(entry.mode)}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "local fake candidate.tar.gz")
	if err := os.WriteFile(name, data.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return name, fmt.Sprintf("%x", sha256.Sum256(data.Bytes()))
}
