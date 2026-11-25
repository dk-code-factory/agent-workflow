package plugins

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func capturedFixture(t *testing.T) (string, Descriptor, map[string]string) {
	t.Helper()
	root := t.TempDir()
	d := Descriptor{SchemaVersion: 1, Name: "demo", Version: "1.0.0", EngineVersion: "0.2.2", Hosts: []string{"codex", "claude"}, Skills: []Skill{{Name: "demo", Path: "./skills/demo/SKILL.md", Workflow: " \t./workflows/main.yml \n"}}, Resources: []string{"assets"}}
	encoded, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		DescriptorFile:         string(encoded),
		"skills/demo/SKILL.md": "---\nname: demo\ndescription: Captured workflow\n---\nRun the workflow.\n",
		"workflows/main.yml":   "jobs: {nested: {uses: ./workflows/nested/child.yml}}\n",
		"workflows/nested/child.yml": `on: {workflow_call: {}}
jobs:
  task:
    steps:
      - uses: agent/run
        with: {agent: agents, prompt: hello}
      - uses: docker://alpine:3
        files: [helpers]
        with: {entrypoint: sh, args: helpers/run.sh}
`,
		"workflows/nested/agents/task.yaml": exampleAgent,
		"workflows/nested/helpers/run.sh":   "#!/bin/sh\nprintf '%s' captured\n",
		"assets/raw.bin":                    "\x00\xffcaptured\n",
	}
	for name, contents := range files {
		put(t, root, name, contents)
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(name)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for name, mode := range map[string]fs.FileMode{
		"assets":                          0750,
		"assets/empty":                    0710,
		"assets/raw.bin":                  0640,
		"workflows/nested/agents":         0750,
		"workflows/nested/helpers":        0750,
		"workflows/nested/helpers/empty":  0710,
		"workflows/nested/helpers/run.sh": 0751,
	} {
		name = filepath.Join(root, filepath.FromSlash(name))
		if filepath.Ext(name) == "" {
			if err := os.MkdirAll(name, 0755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	put(t, root, "undeclared.txt", "private workspace data")
	put(t, root, "workflows/undeclared.yml", "invalid workflow")
	return root, d, files
}

func TestCaptureRetainsValidatedClosure(t *testing.T) {
	root, descriptor, files := capturedFixture(t)
	p, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Descriptor(), descriptor) {
		t.Fatalf("changed descriptor: %+v", p.Descriptor())
	}
	binding, err := p.Binding("demo")
	if err != nil || binding != (Skill{Name: "demo", Path: "skills/demo/SKILL.md", Workflow: "workflows/main.yml"}) {
		t.Fatalf("canonical binding: %+v, %v", binding, err)
	}
	manifest := p.Manifest()
	if manifest.Version != 1 {
		t.Fatalf("manifest version: %d", manifest.Version)
	}
	wantPaths := []string{"assets", "assets/empty", "assets/raw.bin", "plugin.yaml", "skills/demo/SKILL.md", "workflows/main.yml", "workflows/nested/agents", "workflows/nested/agents/task.yaml", "workflows/nested/child.yml", "workflows/nested/helpers", "workflows/nested/helpers/empty", "workflows/nested/helpers/run.sh"}
	var paths []string
	for _, entry := range manifest.Entries {
		paths = append(paths, entry.Path)
		if contents, ok := files[entry.Path]; ok {
			hash := sha256.Sum256([]byte(contents))
			if entry.Kind != "file" || entry.Size != int64(len(contents)) || entry.SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatalf("changed file identity: %+v", entry)
			}
		} else if entry.Kind != "directory" || entry.Size != 0 || entry.SHA256 != "" {
			t.Fatalf("lost directory identity: %+v", entry)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil || info.Mode().Perm() != fs.FileMode(entry.Mode) {
			t.Fatalf("changed original mode for %s: %+v, %v", entry.Path, entry, err)
		}
	}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("declared closure paths:\n%v\nwant %v", paths, wantPaths)
	}
	// A retained package must be usable after the entire installation disappears.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		got, err := p.ReadFile(name)
		if err != nil || !bytes.Equal(got, []byte(contents)) {
			t.Fatalf("changed captured %s: %q, %v", name, got, err)
		}
	}
	if !reflect.DeepEqual(manifest, p.Manifest()) || !reflect.DeepEqual(descriptor, p.Descriptor()) {
		t.Fatal("source removal changed captured metadata")
	}
}

func TestCapturedPackageAccessorsAreIndependent(t *testing.T) {
	root, descriptor, files := capturedFixture(t)
	p, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	digest, manifest := p.Digest(), p.Manifest()
	gotDescriptor := p.Descriptor()
	gotDescriptor.Name = "other"
	gotDescriptor.Hosts[0] = "other"
	gotDescriptor.Skills[0].Workflow = "workflows/other.yml"
	gotDescriptor.Resources[0] = "other"
	gotManifest := p.Manifest()
	gotManifest.Version = 999
	gotManifest.Entries[0].Path = "other"
	gotBytes, err := p.ReadFile(DescriptorFile)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes[0] ^= 1
	if !reflect.DeepEqual(descriptor, p.Descriptor()) || !reflect.DeepEqual(manifest, p.Manifest()) || digest != p.Digest() {
		t.Fatal("accessor mutation changed package identity")
	}
	gotBytes, err = p.ReadFile(DescriptorFile)
	if err != nil || string(gotBytes) != files[DescriptorFile] {
		t.Fatalf("accessor mutation changed source bytes: %q, %v", gotBytes, err)
	}
	for _, name := range []string{"../plugin.yaml", "/plugin.yaml", "assets/../plugin.yaml", ".", "assets/empty"} {
		if _, err := p.ReadFile(name); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("accepted unsafe path or directory %q: %v", name, err)
		}
	}
	if _, err := p.ReadFile("undeclared.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read undeclared data: %v", err)
	}
	if _, err := p.Binding("unknown"); err == nil {
		t.Fatal("selected undeclared skill")
	}
}

func TestCaptureDigestSurvivesRelocation(t *testing.T) {
	root, _, _ := capturedFixture(t)
	original, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	relocated := filepath.Join(t.TempDir(), "relocated")
	if err := os.Rename(root, relocated); err != nil {
		t.Fatal(err)
	}
	put(t, relocated, "undeclared.txt", "changed private data")
	again, err := Capture(relocated)
	if err != nil {
		t.Fatal(err)
	}
	if original.Digest() != again.Digest() || !reflect.DeepEqual(original.Manifest(), again.Manifest()) {
		t.Fatal("installation location or undeclared content changed package identity")
	}
}

func TestCaptureDigestTracksDeclaredChanges(t *testing.T) {
	for _, change := range []string{"descriptor bytes", "skill bytes", "workflow bytes", "agent bytes", "resource bytes", "file mode", "directory mode", "add empty directory", "remove empty directory"} {
		t.Run(change, func(t *testing.T) {
			if runtime.GOOS == "windows" && (change == "file mode" || change == "directory mode") {
				t.Skip("Windows does not retain distinct POSIX permission bits for these mode changes")
			}
			root, _, files := capturedFixture(t)
			original, err := Capture(root)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "descriptor bytes":
				put(t, root, DescriptorFile, files[DescriptorFile]+"# changed descriptor\n")
			case "skill bytes":
				put(t, root, "skills/demo/SKILL.md", files["skills/demo/SKILL.md"]+"Changed instructions.\n")
			case "workflow bytes":
				put(t, root, "workflows/main.yml", files["workflows/main.yml"]+"# changed workflow\n")
			case "agent bytes":
				put(t, root, "workflows/nested/agents/task.yaml", files["workflows/nested/agents/task.yaml"]+"# changed agent\n")
			case "resource bytes":
				put(t, root, "assets/raw.bin", "changed resource")
			case "file mode", "directory mode":
				name := "assets/raw.bin"
				if change == "directory mode" {
					name = "assets/empty"
				}
				if err := os.Chmod(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
			case "add empty directory":
				if err := os.Mkdir(filepath.Join(root, "assets/added"), 0755); err != nil {
					t.Fatal(err)
				}
			case "remove empty directory":
				if err := os.Remove(filepath.Join(root, "assets/empty")); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := Capture(root)
			if err != nil {
				t.Fatal(err)
			}
			if original.Digest() == changed.Digest() {
				t.Fatal("declared package change reused its original digest")
			}
		})
	}
}

func TestCaptureBuildsFromOneValidatedSnapshot(t *testing.T) {
	root, _, files := capturedFixture(t)
	descriptor, collected, err := loadCaptured(root)
	if err != nil {
		t.Fatal(err)
	}
	// Construction must use only validated bytes, even if source paths disappear
	// before the public immutable representation is built.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	p, err := newCapturedPackage(descriptor, collected)
	if err != nil {
		t.Fatal(err)
	}
	// Ownership of the collector and descriptor must not escape into the API.
	descriptor.Skills[0].Name = "changed"
	collected.files[DescriptorFile].data[0] ^= 1
	delete(collected.files, "assets/raw.bin")
	for name, contents := range files {
		got, err := p.ReadFile(name)
		if err != nil || string(got) != contents {
			t.Fatalf("did not retain validated %s: %q, %v", name, got, err)
		}
	}
	if _, err := p.Binding("demo"); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRejectsInvalidClosure(t *testing.T) {
	root, _, _ := capturedFixture(t)
	put(t, root, "workflows/nested/agents/task.yaml", "name: missing-required-fields\n")
	if p, err := Capture(root); err == nil || p != nil {
		t.Fatalf("exported an invalid package: %+v, %v", p, err)
	}
}

func TestCaptureDigestEncodingV1(t *testing.T) {
	// Use explicit captured modes so the encoding fixture is independent of
	// filesystem permission semantics. Other tests exercise public Capture.
	collected := &collector{files: map[string]file{}}
	for name, text := range map[string]string{
		DescriptorFile:         "schema-version: 1\nname: demo\nversion: 1.0.0\nengine-version: 0.2.2\nhosts: [codex]\nskills: [{name: demo, path: skills/demo/SKILL.md, workflow: workflows/main.yml}]\n",
		"skills/demo/SKILL.md": "---\nname: demo\ndescription: Captured workflow\n---\nRun the workflow.\n",
		"workflows/main.yml":   "jobs: {main: {steps: [{argv: [echo, captured]}]}}\n",
	} {
		collected.files[name] = file{data: []byte(text), mode: 0644}
	}
	descriptor, err := Parse(collected.files[DescriptorFile].data)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newCapturedPackage(descriptor, collected)
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the version-1 encoding so future manifest changes require an
	// explicit version decision rather than silently invalidating identities.
	const want = "070068190e8c237ff545c6b6867a39be8ebf2d847fedf2728ba3b2851ce56485"
	if p.Digest() != want {
		t.Fatalf("changed version-1 package digest: %s", p.Digest())
	}
}

func TestCaptureRejectsNonUTF8Paths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires a filesystem accepting non-UTF-8 path bytes")
	}
	root, _, _ := capturedFixture(t)
	put(t, root, "assets/\xff", "ambiguous JSON path")
	if p, err := Capture(root); err == nil || p != nil {
		t.Fatalf("exported a non-UTF-8 manifest path: %+v, %v", p, err)
	}
}
