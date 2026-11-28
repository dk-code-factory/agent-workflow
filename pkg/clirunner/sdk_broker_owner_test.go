package clirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestSDKManagedReacquireRevalidatesOriginalOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1)}
	lease, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	definition := workflows.ToolDefinition{Server: "fixture", Name: "lookup"}
	blockerReady := make(chan *workflows.CapacityLease, 1)
	binding := &managedBatchBinding{catalog: []workflows.ToolDefinition{definition}, call: func(ctx context.Context, _ workflows.ToolRequest) (json.RawMessage, error) {
		blocker, err := pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		blockerReady <- blocker
		return json.RawMessage(`{"original":true}`), nil
	}}
	var stale atomic.Bool
	ownerErr := errors.New("original execution owner replaced")
	batch := newSDKBrokerBatch(&workflows.InvocationTools{Binding: binding, Lease: lease}, nil, func() error {
		if stale.Load() {
			return ownerErr
		}
		return nil
	})
	data := map[string]any{"response_id": "original-response", "model": "fixture", "stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}, "calls": []map[string]any{{"id": "original-call", "name": "mcp__broker__" + workflows.ToolWireName(definition), "arguments": json.RawMessage(`{}`)}}}
	if err = batch.start(data); err != nil {
		t.Fatal(err)
	}
	if err = batch.seal(ctx, data, "fixture", "session"); err != nil {
		t.Fatal(err)
	}
	if err = batch.queue(workflows.ToolRequest{CallID: "original-call", Server: definition.Server, Tool: definition.Name, Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var callbacks sync.WaitGroup
	failure := make(chan error, 1)
	if err = batch.blocked(ctx, bridgeFrame{Data: data, CallIDs: []string{"original-call"}}, &bridgeWriter{writer: &output}, &callbacks, func(err error) { failure <- err }); err != nil {
		t.Fatal(err)
	}
	var blocker *workflows.CapacityLease
	select {
	case blocker = <-blockerReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer blocker.Release()
	// The callback has its semantic result, but capacity belongs to another
	// worker. Replacing durable authority does not cancel the original context.
	stale.Store(true)
	blocker.Release()
	select {
	case err = <-failure:
		if !errors.Is(err, ownerErr) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("stale owner resumed instead of failing", ctx.Err())
	}
	joined := make(chan struct{})
	go func() { callbacks.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("callback did not join", ctx.Err())
	}
	if output.Len() != 0 || binding.calls.Load() != 1 || ctx.Err() != nil {
		t.Fatal("stale owner delivered semantic result", output.String(), binding.calls.Load(), ctx.Err())
	}
}
