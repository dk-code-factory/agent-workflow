package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/controller"
	"github.com/dk-code-factory/agent-workflow/pkg/plugins"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestPluginRunRejectsMissingAdmissionIdentity(t *testing.T) {
	for _, args := range [][]string{
		{"plugin", "run", "task"},
		{"plugin", "run", "--plugin-root", "unused", "task"},
		{"plugin", "run", "--plugin-root", "unused", "--request-id", "x", "--foreground", "task"},
		{"plugin", "run", "--plugin-root", "unused", "--request-id", "x", "task", "--input", "x=1"},
		{"status", "--request-id", "x", "a-run-id"},
	} {
		var out bytes.Buffer
		if err := command(t.Context(), args, &out, io.Discard); err == nil {
			t.Fatalf("accepted ambiguous package/inspection invocation: %v", args)
		}
	}
}

func TestPluginRunAndLostAcknowledgementRecovery(t *testing.T) {
	for _, configuration := range []string{"invalid", "changed", "deleted"} {
		t.Run(configuration, func(t *testing.T) { pluginRecoveryFixture(t, configuration) })
	}
}

func pluginRecoveryFixture(t *testing.T, configuration string) {
	workspace := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(workspace, ".agent-workflow/assets"), func(name string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				return os.Chmod(name, 0700)
			}
			return err
		})
	})
	root := filepath.Join(t.TempDir(), "installed")
	if err := plugins.Init(root, "task", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	source := `on:
  workflow_call:
    inputs:
      number: {type: number, required: true}
      text: {type: string, required: true}
jobs:
  task:
    steps:
      - {shell: sh, run: 'echo once >> effects'}
      - {id: gate, uses: input/wait, with: {prompt: continue, evidence-id: original}}
      - {argv: [sh, -c, 'printf "%s" "$1" > result', fixture, '${{ inputs.text }}']}
`
	if err := os.WriteFile(filepath.Join(root, "workflows/main.yml"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	start := func(admitted string) func() {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		var diagnostics bytes.Buffer
		controllerArgs := []string{"controller", "--workspace", workspace}
		if admitted != "" {
			controllerArgs = append(controllerArgs, "--admitted-run", admitted)
		}
		go func() { done <- command(runCtx, controllerArgs, &diagnostics, &diagnostics) }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("controller stop deadline")
				}
			})
		}
		t.Cleanup(stop)
		return stop
	}
	stop := start("")
	api := controller.NewClient(filepath.Join(workspace, ".agent-workflow"))
	deadline := time.Now().Add(5 * time.Second)
	for api.Call(ctx, "GET", "/health", nil, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("controller start deadline")
		}
		time.Sleep(time.Millisecond)
	}
	var out bytes.Buffer
	args := []string{"plugin", "run", "--workspace", workspace, "--plugin-root", root, "--request-id", "lost-ack", "--input", "number=9007199254740993", "--input", `text="001"`, "--detach", "task"}
	if err := command(ctx, args, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var run workflows.Run
	if err := json.Unmarshal(out.Bytes(), &run); err != nil {
		t.Fatal(err, out.String())
	}
	for {
		if err := api.Call(ctx, "GET", "/runs/"+run.ID, nil, &run); err != nil {
			t.Fatal(err)
		}
		if run.Status == workflows.RunStatusWaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not suspend", run)
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := command(ctx, []string{"status", "--workspace", workspace, "--request-id", "lost-ack"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var recovered workflows.Run
	if err := json.Unmarshal(out.Bytes(), &recovered); err != nil || recovered.ID != run.ID {
		t.Fatalf("lost admission: %+v, %v", recovered, err)
	}
	store, err := workflows.OpenSQLiteInspection(filepath.Join(workspace, ".agent-workflow"))
	if err != nil {
		t.Fatal(err)
	}
	_, snap, err := store.Load(ctx, run.ID)
	store.Close()
	if err != nil || snap.Inputs["number"] != json.Number("9007199254740993") || snap.Inputs["text"] != "001" {
		t.Fatalf("changed typed input: %+v, %v", snap.Inputs, err)
	}
	var gates []workflows.ExternalInputGate
	if err := api.Call(ctx, "GET", "/runs/"+run.ID+"/inputs", nil, &gates); err != nil {
		t.Fatal(err)
	}
	// Interrupt before answering so only the explicit retained resume advances
	// this continuation after its installation vanished.
	if err := api.Call(ctx, "POST", "/runs/"+run.ID+"/interrupt", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := api.Call(ctx, "POST", "/runs/"+run.ID+"/inputs/"+gates[0].ID+"/answer", workflows.ExternalInputAnswer{AnswerID: "approve", EvidenceID: "original", Value: json.RawMessage(`true`)}, nil); err != nil {
		t.Fatal(err)
	}
	stop()
	configPath := filepath.Join(workspace, "agents.yml")
	switch configuration {
	case "invalid":
		if err := os.WriteFile(configPath, []byte("invalid: ["), 0600); err != nil {
			t.Fatal(err)
		}
	case "changed":
		if err := os.WriteFile(configPath, []byte("controller: {max-queued: 17}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	case "deleted":
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	start(run.ID)
	deadline = time.Now().Add(5 * time.Second)
	for api.Call(ctx, "GET", "/health", nil, nil) != nil {
		if time.Now().After(deadline) {
			t.Fatal("recovery controller start deadline")
		}
		time.Sleep(time.Millisecond)
	}
	if err := api.Call(ctx, "POST", "/runs", controller.StartRequest{RequestID: "new", Ref: "workflows/main.yml"}, nil); err == nil || !strings.Contains(err.Error(), "recovery controller") {
		t.Fatal("cold recovery settings authorized fresh work", err)
	}
	out.Reset()
	if err := command(ctx, []string{"resume", "--workspace", workspace, run.ID}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"effects": "once\n", "result": "001"} {
		got, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(got) != want {
			t.Fatalf("recovered %s: %q, %v", name, got, err)
		}
	}
	out.Reset()
	if err := command(ctx, args, &out, io.Discard); err == nil {
		t.Fatal("fresh launch ignored missing installation")
	}
	out.Reset()
	if err := command(ctx, []string{"status", "--workspace", workspace, "--request-id", "missing"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("missing request lookup: %v", err)
	}
}
