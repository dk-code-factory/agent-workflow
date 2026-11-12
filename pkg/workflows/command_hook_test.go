package workflows

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestShellHookAllowsScalarEventNamedInputs(t *testing.T) {
	e, _ := durableExecutor(t, `on:
  workflow_call:
    inputs:
      event_name: {type: string, default: release}
jobs:
  task:
    steps:
      - id: work
        argv: [true]
        finally:
          - shell: sh
            run: "printf '%s' '${{ inputs.event_name }}'"
`, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := e.Run(ctx, RunRequest{Ref: "workflows/main.yml"})
	if err != nil || run.Status != RunStatusSucceeded {
		t.Fatalf("scalar input prevented finalization: run=%+v err=%v", run, err)
	}
	finalizers := run.Jobs["task"].Steps["work"].Lifecycle.Handlers["finally"]
	if len(finalizers) != 1 || finalizers[0].Outputs["stdout"] != "release" {
		t.Fatalf("scalar finalizer did not execute: %+v", finalizers)
	}
}

func TestShellHookRejectsActualEventAccessAndCompositeInterpolation(t *testing.T) {
	for _, expression := range []string{"event.type", "event.type == 'finally'", "inputs.object", "inputs.array"} {
		t.Run(expression, func(t *testing.T) {
			e := &Executor{WorkspaceDir: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := e.executeCommand(ctx, Step{Shell: "sh", Run: "echo ${{ " + expression + " }}"}, expressionContext{
				Hook: true, Event: map[string]any{"type": "finally"}, Inputs: map[string]any{"object": map[string]any{"ok": true}, "array": []any{"a"}},
			}, make(chan struct{}, 1))
			if err == nil || !strings.Contains(err.Error(), "event JSON on stdin") {
				t.Fatalf("unsafe interpolation allowed: %v", err)
			}
		})
	}
}
