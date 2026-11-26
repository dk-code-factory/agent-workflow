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

func TestManagedCodexUnknownFirstAttemptHoldsAdmissionAndMailbox(t *testing.T) {
	identity := managedNativeFixtureIdentity(t)
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	store, err := workflows.OpenSQLite(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if store != nil {
			store.Close()
		}
	}()
	run := &workflows.Run{ID: "managed-unknown-first", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
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
		if requests.Add(1) != 1 {
			t.Error("unknown native attempt automatically retried")
		}
		// The request is consumed, but neither a Completed identity nor usage is
		// supplied. Ending this owned fake stream must not mint a replacement call.
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"unknown-original\"}}\n\n")
	}))
	defer gateway.Close()
	t.Setenv("MANAGED_UNKNOWN_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"empty": {Provider: "codex-app-server", Model: "gpt-5.4", Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_UNKNOWN_FIXTURE_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture := &managedEngineFixture{runner: runner, identity: identity, pool: pool}
	factory := func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return fixture, []string{}, nil, nil
	}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: factory}
	if err = manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker, err := manager.Spawn(t.Context(), workflows.WorkerSpec{RunID: run.ID, Role: "empty", Turns: 1})
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.Send(t.Context(), run.ID, worker.ID, "unknown-original-request", "perform the admitted task")
	if err != nil {
		t.Fatal(err)
	}
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: run.ID, ParentWorkerID: worker.ID, InvocationID: message.ID, AgentID: "empty"}}
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = \"fixture\"\n[features]\nunbounded_connection_retries = true\nresponses_websockets = true\nresponses_websockets_v2 = true\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nstream_max_retries = 9\nrequest_max_retries = 9\nsupports_websockets = true\n", gateway.URL)
	if err = os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	firstCtx, stopFirst := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	go func() { manager.Serve(firstCtx); close(firstDone) }()
	defer func() { stopFirst(); <-firstDone }()
	var failed workflows.WorkerMessage
	for ctx.Err() == nil {
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Status != workflows.RunStatusQueued && messages[0].Status != workflows.RunStatusRunning {
			failed = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed.Status != workflows.RunStatusFailed || requests.Load() != 1 {
		t.Fatalf("unknown first attempt did not hold: %+v APIs=%d", failed, requests.Load())
	}
	current, err := manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.Remaining != 0 || current.Status != workflows.RunStatusNeedsAttention {
		t.Fatal(current, err)
	}
	if err = manager.Resume(ctx, run.ID, worker.ID); err == nil {
		t.Fatal("public mailbox acknowledgement discarded unknown first attempt")
	}
	if _, err = manager.Send(ctx, run.ID, worker.ID, "queued-later", "must remain queued without another allowance"); err != nil {
		t.Fatal(err)
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil || report.Summary.Complete || report.Summary.InputTokens != nil || report.Summary.OutputTokens != nil {
		t.Fatal("unknown usage became synthetic zero/completed", report, err)
	}
	if len(report.Invocations) != 1 || len(report.Invocations[0].Observations) != 2 {
		t.Fatal("missing exact admission bookkeeping and incomplete final record", report)
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	stopFirst()
	<-firstDone
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = workflows.OpenSQLite(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.QueueResume(ctx, run.ID, "unknown-owner-recovery", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	resumed.Status = workflows.RunStatusRunning
	if err = store.Claim(ctx, resumed, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	pool = &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture = &managedEngineFixture{runner: runner, identity: identity, pool: pool}
	manager = &workflows.WorkerManager{Store: store, Capacity: pool, Factory: factory}
	if err = manager.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err = manager.Resume(ctx, run.ID, worker.ID); err == nil {
		t.Fatal("restart acknowledged unknown first attempt")
	}
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan struct{})
	go func() { manager.Serve(secondCtx); close(secondDone) }()
	defer func() { stopSecond(); <-secondDone }()
	// Two ordinary scheduler sweeps exercise the later queued message without
	// assuming that merely inspecting an env flag proves native retry behavior.
	select {
	case <-time.After(2200 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	messages, err := manager.Messages(ctx, run.ID, worker.ID)
	if err != nil || len(messages) != 2 || messages[0].ID != message.ID || messages[1].Status != workflows.RunStatusQueued || requests.Load() != 1 {
		t.Fatal("unknown attempt gained fresh work", messages, requests.Load(), err)
	}
	current, err = manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal("unknown retry reset original allowance", current, err)
	}
	report, err = store.Usage(ctx, run.ID)
	if err != nil || report.Summary.Complete || report.Summary.InputTokens != nil {
		t.Fatal(report, err)
	}
	runtimeRows, err := store.Invocations(ctx, run.ID, false)
	if err != nil || len(runtimeRows) != 1 || runtimeRows[0].State != "cleaned" || runtimeRows[0].LogicalID != message.ID {
		t.Fatal("held unknown attempt created another native invocation", runtimeRows, err)
	}
	raw, _ := json.Marshal(report.Invocations[0].Observations)
	t.Logf("binary=%s original=%s owner=%s/%d→%s/%d APIs=1 Completed=0 callbacks=0 Remaining=0 subsequent=queued bookkeeping-only-known-cost=false observations=%s", identity.BinarySHA256, message.ID, run.Owner, run.Generation, resumed.Owner, resumed.Generation, raw)
}
