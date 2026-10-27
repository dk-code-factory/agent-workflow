package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestRegisteredMutationControllerRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workflows", "main.yml"), []byte("jobs: {publish: {steps: [{uses: action/publish}]}}"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "state")
	store, err := workflows.OpenSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	type input struct{}
	type output struct {
		Receipt string `json:"receipt"`
	}
	started := make(chan struct{})
	var calls atomic.Int32
	var original workflows.ActionContext
	registry := &workflows.ActionRegistry{}
	err = workflows.RegisterAction(registry, "publish", workflows.ActionSpec[input, output]{Implementation: "fixture-v1", Effect: workflows.ActionExternalMutation,
		Bindings: func(context.Context, workflows.ActionContext, input) (json.RawMessage, error) {
			return json.RawMessage(`{"repository":"disposable-fixture"}`), nil
		},
		Handler: func(ctx context.Context, op workflows.ActionContext, _ input) (workflows.ActionResult[output], error) {
			calls.Add(1)
			original = op
			close(started)
			<-ctx.Done()
			return workflows.ActionResult[output]{Receipt: json.RawMessage(`{"publication":"fixture-receipt"}`)}, ctx.Err()
		},
		ValidateCheckpoint: func(_ context.Context, _ workflows.ActionContext, cp workflows.ActionCheckpoint[input, output]) error {
			if cp.Result.Output.Receipt != "fixture-receipt" {
				t.Error("recovered receipt lost")
			}
			return nil
		},
		Reconcile: func(_ context.Context, op workflows.ActionContext, cp workflows.ActionCheckpoint[input, output]) (workflows.ActionReconciliation[output], error) {
			if op != original || len(cp.Result.Receipt) == 0 {
				t.Error("original operation/receipt lost")
			}
			return workflows.ActionReconciliation[output]{Outcome: "completed", Proof: json.RawMessage(`{"verified":true}`), Result: workflows.ActionResult[output]{Output: output{"fixture-receipt"}, Receipt: cp.Result.Receipt}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	start := func() (*Client, func()) {
		executor := &workflows.Executor{WorkspaceDir: root, Store: store, ApplicationActions: registry}
		server := &Server{Dir: dir, Store: store, Executor: executor}
		ownerCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- server.Serve(ownerCtx) }()
		t.Cleanup(cancel)
		client := NewClient(dir)
		deadline := time.Now().Add(5 * time.Second)
		for client.Call(ctx, "GET", "/health", nil, nil) != nil {
			if time.Now().After(deadline) {
				t.Fatal("controller did not start")
			}
			time.Sleep(time.Millisecond)
		}
		return client, func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("controller did not join canceled action")
			}
		}
	}
	client, stop := start()
	var health struct {
		Capabilities []string `json:"capabilities"`
	}
	if err = client.Call(ctx, "GET", "/health", nil, &health); err != nil || !slices.Contains(health.Capabilities, workflows.RegisteredGoActionsV1) {
		t.Fatal("missing capability", health, err)
	}
	var handle workflows.Run
	if err = client.Call(ctx, "POST", "/runs", StartRequest{Ref: "workflows/main.yml"}, &handle); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("action did not start")
	}
	stop()
	saved, _, err := store.Load(ctx, handle.ID)
	if err != nil || saved.Status != workflows.RunStatusNeedsAttention {
		t.Fatal("canceled mutation uncertainty lost", saved, err)
	}
	client, stop = start()
	defer stop()
	if err = client.Call(ctx, "POST", "/runs/"+handle.ID+"/resume?request_id=receipt-recovery", nil, &handle); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, client, handle.ID, workflows.RunStatusSucceeded)
	if calls.Load() != 1 {
		t.Fatal("mutation repeated after restart", calls.Load())
	}
}
