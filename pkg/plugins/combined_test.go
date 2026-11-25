package plugins

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func projectSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files[path] = info.Mode().String()
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] += fmt.Sprintf(" %x", sha256.Sum256(data))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestPluginAddSkillChecksCombinedProject(t *testing.T) {
	for _, boundary := range []string{"workflows", "bytes", "entries", "descriptor"} {
		t.Run(boundary, func(t *testing.T) {
			sources := []string{t.TempDir(), t.TempDir()}
			for index, source := range sources {
				workflow := "on: {workflow_call: {}}\njobs:\n"
				switch boundary {
				case "workflows":
					for child := 0; child < 100; child++ {
						name := fmt.Sprintf("workflows/child-%03d.yml", child)
						workflow += fmt.Sprintf("  child%d: {uses: %s}\n", child, name)
						put(t, source, name, "on: {workflow_call: {}}\njobs: {main: {steps: [{argv: [echo, hello]}]}}\n")
					}
				case "bytes":
					workflow += "  main: {steps: [{uses: docker://python:3, with: {entrypoint: python, args: helpers/asset}, files: [helpers]}]}\n"
					put(t, source, "workflows/helpers/asset", strings.Repeat("x", (17-index)<<20))
					if err := os.Chmod(filepath.Join(source, "workflows/helpers/asset"), 0555); err != nil {
						t.Fatal(err)
					}
				case "entries":
					workflow += "  main: {steps: [{uses: docker://python:3, with: {entrypoint: python, args: helpers/asset}, files: [helpers]}]}\n"
					for item := 0; item < 2050; item++ {
						put(t, source, fmt.Sprintf("workflows/helpers/item-%04d", item), "declared\n")
					}
				case "descriptor":
					workflow += "  main: {steps: [{argv: [echo, hello]}]}\n"
				}
				put(t, source, "workflows/main.yml", workflow)
				if _, _, err := importWorkflow("extra", filepath.Join(source, "workflows/main.yml")); err != nil {
					t.Fatalf("individual closure is invalid: %v", err)
				}
			}
			root := filepath.Join(t.TempDir(), "demo")
			if err := Init(root, "demo", "0.2.2", filepath.Join(sources[0], "workflows/main.yml")); err != nil {
				t.Fatal(err)
			}
			if boundary == "descriptor" {
				d, err := ReadDescriptor(root)
				if err != nil {
					t.Fatal(err)
				}
				data, err := yaml.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				// Keep the existing descriptor below its bound; another binding
				// makes the serialized descriptor exceed that same engine bound.
				d.Name = strings.Repeat("a", (1<<20)-len(data)+len(d.Name)-50)
				data, err = yaml.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				put(t, root, DescriptorFile, string(data))
			}
			if _, err := Load(root); err != nil {
				t.Fatalf("existing project is invalid: %v", err)
			}
			before := projectSnapshot(t, root)
			sourceBefore := projectSnapshot(t, sources[1])
			err := AddSkill(root, "extra", filepath.Join(sources[1], "workflows/main.yml"))
			if err == nil {
				_, loadErr := Load(root)
				t.Fatalf("add-skill accepted a combined project; subsequent Load: %v", loadErr)
			}
			want := map[string]string{
				"workflows":  "closure exceeds 201 workflows",
				"bytes":      "dependency closure exceeds 32 MiB",
				"entries":    "4096",
				"descriptor": "plugin.yaml: exceeds 1 MiB",
			}[boundary]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong combined admission error: %v; want %q", err, want)
			}
			if after := projectSnapshot(t, root); !reflect.DeepEqual(before, after) {
				t.Fatal("failed import changed project paths, contents or modes")
			}
			if after := projectSnapshot(t, sources[1]); !reflect.DeepEqual(sourceBefore, after) {
				t.Fatal("failed import changed source paths, contents or modes")
			}
			if _, err := Load(root); err != nil {
				t.Fatalf("failed import damaged existing project: %v", err)
			}
		})
	}
}

func TestPluginAddSkillRejectsExistingAgentDirectory(t *testing.T) {
	source := t.TempDir()
	put(t, source, "workflows/main.yml", strings.ReplaceAll(exampleWorkflow, "agents/task.yaml", "agents"))
	put(t, source, "workflows/agents/task.yaml", exampleAgent)
	root := filepath.Join(t.TempDir(), "demo")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	put(t, root, "imports/extra/workflows/agents/unrelated.yaml", exampleAgent)
	before := projectSnapshot(t, root)
	if err := AddSkill(root, "extra", filepath.Join(source, "workflows/main.yml")); err == nil {
		_, loadErr := Load(root)
		t.Fatalf("accepted a directory collision; subsequent Load: %v", loadErr)
	} else if !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("wrong collision diagnostic: %v", err)
	}
	if after := projectSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("directory collision changed existing project")
	}
}
