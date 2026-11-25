package main

import (
	"bytes"
	"testing"
)

func TestPluginDispatcherOffline(t *testing.T) {
	t.Setenv("PATH", "")
	var out, errout bytes.Buffer
	if err := command(t.Context(), []string{"plugin", "--help"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"plugin", "unknown"}, {"plugin", "init"}, {"plugin", "init", "name", "--workflow", "x"}, {"plugin", "add-skill", "name"}} {
		if err := command(t.Context(), args, &out, &errout); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestPluginCLIInitAndImport(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PATH", "")
	var out, errout bytes.Buffer
	if err := command(t.Context(), []string{"plugin", "init", "demo"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
	if err := command(t.Context(), []string{"plugin", "add-skill", "--plugin", "demo", "--workflow", "demo/workflows/main.yml", "second"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
	if err := command(t.Context(), []string{"plugin", "init", "--workflow", "demo/workflows/main.yml", "third"}, &out, &errout); err != nil {
		t.Fatal(err)
	}
}
