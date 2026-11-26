//go:build linux

package clirunner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestManagedCodexKnownDenialVersusUncertainEffect(t *testing.T) {
	for _, knownDenied := range []bool{true, false} {
		t.Run(fmt.Sprint("known-denied-", knownDenied), func(t *testing.T) { managedCodexReceiptFixture(t, knownDenied) })
	}
}
func managedCodexReceiptFixture(t *testing.T, knownDenied bool) {
	identity := managedNativeFixtureIdentity(t)
	root := t.TempDir()
	store, err := workflows.OpenSQLite(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: "managed-receipt-classification", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	var requests, effects atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Error("unexpected fake API path", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if _, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20)); err != nil {
			t.Error(err)
			return
		}
		number := requests.Add(1)
		if number > 2 || number > 1 && !knownDenied {
			t.Error("uncertain effect permitted another model request", number)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(value map[string]any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value["type"], data)
		}
		response := fmt.Sprint("receipt-response-", number)
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": response}})
		if number == 1 {
			var nonce any = "a"
			if knownDenied {
				nonce = 7
			}
			args, _ := json.Marshal(map[string]any{"nonce": nonce})
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc-original", "call_id": "original-effect", "name": workflows.ToolWireName(managedEffectDefinitions()[0]), "arguments": string(args)}})
		} else {
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "denied-final", "content": []any{map[string]any{"type": "output_text", "text": "KNOWN_DENIAL_SETTLED"}}}})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": response, "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
	}))
	defer gateway.Close()
	t.Setenv("MANAGED_RECEIPT_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"effects": {Provider: "codex-app-server", Model: "gpt-5.4", Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_RECEIPT_FIXTURE_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture := &managedEngineFixture{runner: runner, identity: identity, pool: pool, gatewayURL: gateway.URL}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return fixture, []string{}, nil, nil
	}}
	marker := filepath.Join(root, "owned-effect")
	broker := &workflows.Broker{Store: store, Workers: manager, External: func(ctx context.Context, _ workflows.ToolDefinition, _ json.RawMessage) (json.RawMessage, error) {
		report, err := store.Usage(ctx, run.ID)
		if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens < 11 {
			return nil, fmt.Errorf("effect preceded known durable usage: %v", err)
		}
		if err = os.WriteFile(marker, []byte("effect happened"), 0600); err != nil {
			return nil, err
		}
		effects.Add(1)
		return nil, fmt.Errorf("response lost after owned external effect")
	}}
	manager.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		in.Catalog = managedEffectDefinitions()
		return broker.Bind(ctx, in)
	}
	if err = manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker, err := manager.Spawn(t.Context(), workflows.WorkerSpec{RunID: run.ID, Role: "effects", Turns: 2})
	if err != nil {
		t.Fatal(err)
	}
	original, err := manager.Send(t.Context(), run.ID, worker.ID, "same-original-effect", "perform exactly the admitted operation")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { manager.Serve(serveCtx); close(done) }()
	defer func() { stop(); <-done }()
	var message workflows.WorkerMessage
	for ctx.Err() == nil {
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Status != workflows.RunStatusQueued && messages[0].Status != workflows.RunStatusRunning {
			message = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	records, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(records) != 1 || records[0].CallID != "original-effect" || records[0].InvocationID != original.ID {
		t.Fatal(records, err)
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if knownDenied {
		if message.Status != workflows.RunStatusSucceeded || message.Result.Outputs["text"] != "KNOWN_DENIAL_SETTLED" || requests.Load() != 2 || effects.Load() != 0 || records[0].Status != "denied" || !report.Summary.Complete || *report.Summary.InputTokens != 22 || *report.Summary.OutputTokens != 6 {
			t.Fatal("known committed pre-dispatch denial did not settle", message, requests.Load(), effects.Load(), records, report)
		}
		if _, err = os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("known denial performed an effect", err)
		}
	} else {
		if message.Status != workflows.RunStatusFailed || requests.Load() != 1 || effects.Load() != 1 || records[0].Status != "uncertain" || report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 3 {
			t.Fatal("uncertainty became ordinary success:false or lost usage", message, requests.Load(), effects.Load(), records, report)
		}
		if err = manager.Resume(ctx, run.ID, worker.ID); err == nil {
			t.Fatal("uncertain effect was acknowledged into new authority")
		}
		queued, err := manager.Send(ctx, run.ID, worker.ID, "later-message", "must not consume another allowance")
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil || len(messages) != 2 || messages[1].ID != queued.ID || messages[1].Status != workflows.RunStatusQueued || requests.Load() != 1 || effects.Load() != 1 {
			t.Fatal("uncertainty admitted later work", messages, requests.Load(), effects.Load(), err)
		}
	}
	current, err := manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.Remaining != 1 {
		t.Fatal("receipt outcome granted or consumed another allowance", current, err)
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("binary=%s known-denied=%v original=%s receipt=%s effects=%d APIs=%d Remaining=1 known-input=%v complete=%v", identity.BinarySHA256, knownDenied, original.ID, records[0].Status, effects.Load(), requests.Load(), *report.Summary.InputTokens, report.Summary.Complete)
}
