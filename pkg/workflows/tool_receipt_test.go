package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func TestKnownToolDenialRequiresExactCommittedCurrentOwnedReceipt(t *testing.T) {
	ctx := context.Background()
	var effects atomic.Int32
	broker, binding, run := brokerFixture(t, func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error) {
		effects.Add(1)
		return json.RawMessage(`{}`), nil
	})
	in := binding.(*toolBinding).invocation
	if err := CheckToolBindingAuthority(ctx, binding, in.RunID, in.InvocationID); err != nil {
		t.Fatal(err)
	}
	if err := CheckToolBindingAuthority(ctx, binding, in.RunID, "foreign"); err == nil {
		t.Fatal("foreign invocation inherited authority")
	}
	if err := CheckToolBindingAuthority(ctx, nil, in.RunID, in.InvocationID); err == nil {
		t.Fatal("absent engine broker inherited authority")
	}
	request := ToolRequest{CallID: "denied-original", Server: "external", Tool: "lookup", Arguments: json.RawMessage(`{"query":7}`)}
	_, failure := binding.Call(ctx, request)
	if failure == nil || !KnownToolDenial(ctx, binding, request, failure) || effects.Load() != 0 {
		t.Fatalf("failure=%v known=%v effects=%d", failure, KnownToolDenial(ctx, binding, request, failure), effects.Load())
	}
	wrapped := workerToolBinding{ToolBinding: binding, observe: func(error) {}}
	if err := CheckToolBindingAuthority(ctx, wrapped, in.RunID, in.InvocationID); err != nil {
		t.Fatal("actual worker wrapper lost original authority", err)
	}
	if !KnownToolDenial(ctx, wrapped, request, failure) {
		t.Fatal("actual worker wrapper lost committed denial")
	}
	canonical := request
	canonical.Arguments = json.RawMessage(`{ "query" : 7 }`)
	if !KnownToolDenial(ctx, binding, canonical, failure) {
		t.Fatal("same original semantic arguments lost receipt")
	}
	for _, altered := range []ToolRequest{
		{CallID: "foreign", Server: request.Server, Tool: request.Tool, Arguments: request.Arguments},
		{CallID: request.CallID, Server: "engine", Tool: request.Tool, Arguments: request.Arguments},
		{CallID: request.CallID, Server: request.Server, Tool: request.Tool, Arguments: json.RawMessage(`{"query":8}`)},
	} {
		if KnownToolDenial(ctx, binding, altered, failure) {
			t.Fatal("foreign call/content inherited a denial receipt")
		}
	}
	for _, unsafe := range []error{errors.New(failure.Error()), errors.Join(failure, ErrUncertainTool), errors.Join(failure, context.Canceled), errors.Join(failure, &runtimeexec.StorageError{Err: errors.New("storage failure")}), errors.Join(failure, ErrActionIntegrity)} {
		if KnownToolDenial(ctx, binding, request, unsafe) {
			t.Fatalf("unsafe/unclassified error became resumable: %v", unsafe)
		}
	}
	_, replay := binding.Call(ctx, request)
	if !KnownToolDenial(ctx, binding, request, replay) || effects.Load() != 0 {
		t.Fatal("original committed denial replay changed effects")
	}
	if _, err := broker.Store.db.ExecContext(ctx, `UPDATE tool_calls SET status='failed' WHERE run_id=? AND call_id=?`, run.ID, request.CallID); err != nil {
		t.Fatal(err)
	}
	if KnownToolDenial(ctx, binding, request, failure) {
		t.Fatal("terminal failed status alone authorized model continuation")
	}
	if _, err := broker.Store.db.ExecContext(ctx, `UPDATE tool_calls SET status='denied' WHERE run_id=? AND call_id=?`, run.ID, request.CallID); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Store.db.ExecContext(ctx, `UPDATE runs SET owner='new-owner' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if KnownToolDenial(ctx, binding, request, failure) {
		t.Fatal("stale owner inherited a previous receipt")
	}
	if err := CheckToolBindingAuthority(ctx, binding, in.RunID, in.InvocationID); err == nil {
		t.Fatal("stale owner retained dispatch authority")
	}
}

func TestUncertainDispatchIsNeverAResumableToolFailure(t *testing.T) {
	ctx := context.Background()
	broker, binding, _ := brokerFixture(t, func(context.Context, ToolDefinition, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("effect may have completed before lost response")
	})
	request := ToolRequest{CallID: "uncertain-original", Server: "external", Tool: "lookup", Arguments: json.RawMessage(`{"query":"x"}`)}
	_, failure := binding.Call(ctx, request)
	if !errors.Is(failure, ErrUncertainTool) || KnownToolDenial(ctx, binding, request, failure) {
		t.Fatalf("uncertain effect authorized continuation: %v", failure)
	}
	if err := broker.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if KnownToolDenial(ctx, binding, request, failure) {
		t.Fatal("storage failure authorized model continuation")
	}
}
