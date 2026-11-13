package workflows

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverUsesWorkspaceDefinitionsDirectory(t *testing.T) {
	for _, test := range []struct{ definitions, directory string }{
		{"", "workflows"},
		{"automation", "automation"},
		{"nested/automation", "nested/automation"},
		{" automation ", "automation"},
		{"  ", "workflows"},
	} {
		t.Run(test.definitions, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(t.TempDir())
			for _, base := range []string{root, "."} {
				path := filepath.Join(base, test.directory, "nested")
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				name := "workspace"
				if base == "." {
					name = "decoy"
				}
				source := "name: " + name + "\njobs: {main: {steps: [{argv: [echo, ok]}]}}\n"
				if err := os.WriteFile(filepath.Join(path, name+".yaml"), []byte(source), 0600); err != nil {
					t.Fatal(err)
				}
			}
			e := nativeTestExecutor(t, &Executor{WorkspaceDir: " " + root + " ", DefinitionsDir: test.definitions})
			const ref = "workflows/nested/workspace.yaml"
			if _, err := e.Load(ref); err != nil {
				t.Fatalf("load workspace definition: %v", err)
			}
			got, err := e.Discover()
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Ref != ref || got[0].Name != "workspace" || got[0].Error != "" {
				t.Fatalf("discovery disagrees with workspace load: %+v", got)
			}
		})
	}
}

func TestDiscoverRejectsInvalidDefinitionsDirectory(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".", "..", "../outside", filepath.Join(root, "absolute")} {
		t.Run(dir, func(t *testing.T) {
			e := nativeTestExecutor(t, &Executor{WorkspaceDir: root, DefinitionsDir: dir})
			if _, err := e.Discover(); err == nil {
				t.Fatal("discovery accepted an invalid definitions directory")
			}
			if _, err := e.Load("workflows/main.yml"); err == nil {
				t.Fatal("loading accepted an invalid definitions directory")
			}
		})
	}
}

func TestDiscoverUsesCurrentDirectoryForEmptyWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	writeWorkflow(t, ".", "main.yml", "jobs: {main: {steps: [{argv: [echo, ok]}]}}")
	for _, workspace := range []string{"", "  ", "."} {
		got, err := (nativeTestExecutor(t, &Executor{WorkspaceDir: workspace})).Discover()
		if err != nil || len(got) != 1 || got[0].Ref != "workflows/main.yml" || got[0].Error != "" {
			t.Fatalf("workspace %q: definitions=%+v error=%v", workspace, got, err)
		}
	}
}

func TestDiscoverRetainsDefinitionErrorsAndSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	writeWorkflow(t, root, "invalid.yml", "jobs: {}")
	writeWorkflow(t, root, "valid.yml", "name: valid\njobs: {main: {steps: [{argv: [echo, ok]}]}}")
	if err := os.Symlink(filepath.Join(root, "workflows", "valid.yml"), filepath.Join(root, "workflows", "link.yml")); err != nil {
		t.Fatal(err)
	}
	got, err := (nativeTestExecutor(t, &Executor{WorkspaceDir: root})).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Ref != "workflows/invalid.yml" || got[0].Error == "" || got[1].Name != "valid" || got[1].Error != "" {
		t.Fatalf("unexpected discovered definitions: %+v", got)
	}
}
