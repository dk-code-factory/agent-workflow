//go:build linux

package clirunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"golang.org/x/sys/unix"
)

func managedEffectDefinitions() []workflows.ToolDefinition {
	return []workflows.ToolDefinition{{Server: "fixture", Name: "a", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"nonce":{"type":"string"}},"required":["nonce"]}`)}, {Server: "fixture", Name: "b", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"nonce":{"type":"string"}},"required":["nonce"]}`)}}
}
func killOwnedManagedFixture(t *testing.T, identity *ManagedCodexBuildIdentity, state string) int {
	t.Helper()
	expected, err := os.Stat(identity.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	guardians, err := managedGuardianChildren(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range guardians {
		guardian, err := strconv.Atoi(text)
		if err != nil {
			t.Fatal(err)
		}
		children, err := managedGuardianChildren(guardian)
		if err != nil {
			continue
		}
		for _, child := range children {
			pid, err := strconv.Atoi(child)
			if err != nil {
				continue
			}
			path := fmt.Sprintf("/proc/%d/exe", pid)
			actual, err := os.Stat(path)
			if err != nil || !os.SameFile(expected, actual) {
				continue
			}
			env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
			if err != nil || !strings.Contains(string(env), "CODEX_HOME="+state+"\x00") {
				continue
			}
			fd, err := unix.PidfdOpen(pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := managedExecutableDigest(file)
			file.Close()
			if err != nil || digest != identity.BinarySHA256 {
				t.Fatal("owned native image mismatch", digest, err)
			}
			if err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatal("verified current owned managed native process not found")
	return 0
}

func TestManagedCodexOriginalMessageSQLiteAndNativeRestart(t *testing.T) {
	managedCodexRestartFixture(t, "restart")
}
func TestManagedCodexCancelledBatchCannotMintAnotherTurn(t *testing.T) {
	managedCodexRestartFixture(t, "cancel")
}
func TestManagedCodexForeignRawCallHoldsOriginalRecovery(t *testing.T) {
	managedCodexRestartFixture(t, "foreign-raw")
}
func managedCodexRestartFixture(t *testing.T, mode string) {
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
	run := &workflows.Run{ID: "managed-original-restart", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	originalOwner, originalGeneration := run.Owner, run.Generation
	defs := managedEffectDefinitions()
	var requests, effects atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Error("unexpected native API path", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			return
		}
		number := requests.Add(1)
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(value map[string]any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value["type"], data)
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
		}
		response := []string{"restart-original", "restart-final"}
		if number > 2 {
			t.Error("replacement model attempt", number)
			return
		}
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": response[number-1]}})
		if number == 1 {
			for _, d := range defs {
				args, _ := json.Marshal(map[string]any{"nonce": d.Name})
				send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc-" + d.Name, "call_id": "original-" + d.Name, "name": workflows.ToolWireName(d), "arguments": string(args)}})
			}
		} else {
			counts := map[string]int{}
			for _, item := range body.Input {
				if item["type"] == "function_call_output" {
					id, _ := item["call_id"].(string)
					counts[id]++
					text, _ := item["output"].(string)
					if !strings.Contains(text, "original-receipt") || strings.Contains(text, "aborted") {
						t.Error("recovery input lost real semantic result", item)
					}
				}
			}
			if len(counts) != 2 || counts["original-a"] != 1 || counts["original-b"] != 1 {
				t.Error("original output list differs", counts)
			}
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "recovered-final", "content": []any{map[string]any{"type": "output_text", "text": "ORIGINAL_EFFECTS_RECOVERED"}}}})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": response[number-1], "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
	}))
	defer gateway.Close()
	t.Setenv("MANAGED_RESTART_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"effects": {Provider: "codex-app-server", Model: "gpt-5.4", Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_RESTART_FIXTURE_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture := &managedEngineFixture{runner: runner, identity: identity, pool: pool}
	var blockOnce sync.Once
	blocked := make(chan *workflows.CapacityLease, 1)
	var effectMu sync.Mutex
	effectFile := filepath.Join(root, "physical-effects.log")
	factory := func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return fixture, []string{}, nil, nil
	}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: factory}
	makeBroker := func(replay bool) *workflows.Broker {
		return &workflows.Broker{Store: store, Workers: manager, External: func(ctx context.Context, d workflows.ToolDefinition, _ json.RawMessage) (json.RawMessage, error) {
			if replay {
				return nil, fmt.Errorf("completed original effect unexpectedly redispatched")
			}
			blockOnce.Do(func() {
				lease, err := pool.Acquire(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				blocked <- lease
			})
			effectMu.Lock()
			defer effectMu.Unlock()
			file, err := os.OpenFile(effectFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			_, writeErr := file.WriteString(d.Name + "\n")
			syncErr := file.Sync()
			closeErr := file.Close()
			if writeErr != nil {
				return nil, writeErr
			}
			if syncErr != nil {
				return nil, syncErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			effects.Add(1)
			return json.RawMessage(`{"original-receipt":"` + d.Name + `"}`), nil
		}}
	}
	broker := makeBroker(false)
	manager.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		in.Catalog = defs
		return broker.Bind(ctx, in)
	}
	if err = manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	turns := 1
	if mode == "cancel" {
		turns = 3
	}
	worker, err := manager.Spawn(t.Context(), workflows.WorkerSpec{RunID: run.ID, Role: "effects", Turns: turns})
	if err != nil {
		t.Fatal(err)
	}
	message, err := manager.Send(t.Context(), run.ID, worker.ID, "same-original-request", "execute exactly the admitted effects")
	if err != nil {
		t.Fatal(err)
	}
	var queued workflows.WorkerMessage
	expectedMessages := 1
	if mode == "cancel" {
		expectedMessages = 2
		// Retained mailbox work admitted before the original hold must stay
		// queued; fresh work after the hold must be rejected before insertion.
		queued, err = manager.Send(t.Context(), run.ID, worker.ID, "queued-before-cancel", "must retain original allowance without dispatch")
		if err != nil {
			t.Fatal(err)
		}
	}
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: run.ID, ParentWorkerID: worker.ID, InvocationID: message.ID, AgentID: "effects"}}
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", gateway.URL)
	if err = os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	firstCtx, stopFirst := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() { manager.Serve(firstCtx); close(firstDone) }()
	defer func() { stopFirst(); awaitManagedServe(t, firstDone) }()
	var lease *workflows.CapacityLease
	select {
	case lease = <-blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer lease.Release()
	for {
		calls, err := store.ToolCalls(ctx, run.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		complete := 0
		for _, call := range calls {
			if call.Status == "completed" {
				complete++
			}
		}
		if complete == 2 {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("original receipts not committed", calls)
		}
	}
	if requests.Load() != 1 || effects.Load() != 2 || pool.Suspended() != 1 {
		t.Fatal("crash cut not sealed original pending batch", requests.Load(), effects.Load(), pool.Suspended())
	}
	killedPID := 0
	if mode == "cancel" {
		if err = manager.Interrupt(ctx, run.ID, worker.ID, false); err != nil {
			t.Fatal(err)
		}
	} else {
		killedPID = killOwnedManagedFixture(t, identity, state)
	}
	lease.Release()
	var before workflows.WorkerMessage
	for ctx.Err() == nil {
		current, err := manager.Get(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == workflows.RunStatusNeedsAttention && len(messages) == expectedMessages {
			before = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	expectedStatus := workflows.RunStatusRunning
	if mode == "cancel" {
		expectedStatus = "interrupted"
	}
	if before.Status != expectedStatus || before.ID != message.ID || before.StartedAt.IsZero() {
		t.Fatalf("original invocation was finalized or replaced %+v", before)
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal("owned runtime cleanup not durably complete", err)
	}
	observationErr := ctx.Err()
	cancel()
	stopFirst()
	awaitManagedServe(t, firstDone)
	if observationErr != nil {
		t.Fatal("original restart cut observation deadline", observationErr)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if mode == "cancel" {
		if _, err = manager.Send(ctx, run.ID, worker.ID, "rejected-after-cancel", "must not gain new mailbox authority"); !errors.Is(err, workflows.ErrActionIntegrity) {
			t.Fatal("cancelled original accepted new mailbox work", err)
		}
	}
	if mode == "foreign-raw" {
		insertForeignManagedRawCall(t, state)
	}
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
	resumed, err := store.QueueResume(ctx, run.ID, "original-owner-recovery", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	resumed.Status = workflows.RunStatusRunning
	if err = store.Claim(ctx, resumed, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	if resumed.Owner == originalOwner || resumed.Generation != originalGeneration+1 {
		t.Fatal("execution fence was not replaced", resumed)
	}
	stale := runtimeexec.WithBinding(ctx, runtimeexec.Binding{Journal: store, RunID: run.ID, LogicalID: message.ID, Owner: originalOwner, Generation: originalGeneration, Kind: "worker"})
	if err = workflows.CheckManagedCodexAuthority(stale, run.ID, message.ID); err == nil {
		t.Fatal("old owner retained original authority")
	}
	pool = &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture = &managedEngineFixture{runner: runner, identity: identity, pool: pool}
	manager = &workflows.WorkerManager{Store: store, Capacity: pool, Factory: factory}
	broker = makeBroker(true)
	manager.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		in.Catalog = defs
		return broker.Bind(ctx, in)
	}
	if err = manager.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err = manager.Resume(ctx, run.ID, worker.ID); !errors.Is(err, workflows.ErrActionIntegrity) {
		t.Fatal("public mailbox ACK discarded original invocation", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("restart recovery setup deadline", err)
	}
	recoveryBudget := 45 * time.Second
	if mode == "cancel" {
		recoveryBudget = 15 * time.Second // No second native invocation is admitted.
	}
	cancel()
	ctx, cancel = context.WithTimeout(t.Context(), recoveryBudget)
	defer cancel()
	secondCtx, stopSecond := context.WithCancel(t.Context())
	secondDone := make(chan struct{})
	go func() { manager.Serve(secondCtx); close(secondDone) }()
	defer func() { stopSecond(); awaitManagedServe(t, secondDone) }()
	if mode != "restart" {
		var outcome workflows.WorkerMessage
		if mode == "cancel" {
			select {
			case <-time.After(2200 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		} else {
			for ctx.Err() == nil {
				messages, err := manager.Messages(ctx, run.ID, worker.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(messages) == 1 && messages[0].Status == workflows.RunStatusFailed {
					outcome = messages[0]
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		observationErr := ctx.Err()
		cancel()
		stopSecond()
		awaitManagedServe(t, secondDone)
		if observationErr != nil {
			t.Fatal("restart hold observation deadline", observationErr)
		}
		ctx, cancel = context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "cancel" {
			if len(messages) != 2 || messages[0].ID != message.ID || messages[0].Status != "interrupted" || !messages[0].StartedAt.Equal(before.StartedAt) || messages[1].ID != queued.ID || messages[1].Status != workflows.RunStatusQueued {
				t.Fatal("cancelled original turn admitted later work", messages)
			}
		} else if outcome.ID != message.ID || !outcome.StartedAt.Equal(before.StartedAt) || outcome.Generation != resumed.Generation {
			t.Fatal("foreign raw history replaced original invocation", messages)
		} else if outcome.FailureClass != "action_integrity" || outcome.UnsafeFailureClass != "integrity" {
			t.Fatal("source uncertainty became acknowledgeable provider failure", outcome)
		}
		expectedRemaining := 0
		expectedRuntime := 2
		if mode == "cancel" {
			expectedRemaining = 2
			expectedRuntime = 1
		}
		current, err := manager.Get(ctx, run.ID, worker.ID)
		if err != nil || current.Remaining != expectedRemaining || current.Status != workflows.RunStatusNeedsAttention {
			t.Fatal("held original authority changed", current, err)
		}
		if err = manager.Resume(ctx, run.ID, worker.ID); !errors.Is(err, workflows.ErrActionIntegrity) {
			t.Fatal("held original turn acknowledged into fresh authority", err)
		}
		if requests.Load() != 1 || effects.Load() != 2 {
			t.Fatal("held source allowed model/effect replay", requests.Load(), effects.Load())
		}
		report, err := store.Usage(ctx, run.ID)
		if err != nil || report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 3 {
			t.Fatal("hold lost original known charge", report, err)
		}
		if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		rows, err := store.Invocations(ctx, run.ID, false)
		if err != nil || len(rows) != expectedRuntime {
			t.Fatal(rows, err)
		}
		for _, row := range rows {
			if row.State != "cleaned" {
				t.Fatal("source work not joined", row)
			}
		}
		t.Logf("binary=%s mode=%s original=%s old-owner=%s/%d new-owner=%s/%d APIs=1 physical-effects=2 Remaining=%d known-usage=11/3 incomplete=true cleaned-runtime=%d", identity.BinarySHA256, mode, message.ID, originalOwner, originalGeneration, resumed.Owner, resumed.Generation, expectedRemaining, expectedRuntime)
		return
	}
	var after workflows.WorkerMessage
	for ctx.Err() == nil {
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Status == workflows.RunStatusSucceeded {
			after = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	recoveryObservationErr := ctx.Err()
	cancel()
	stopSecond()
	awaitManagedServe(t, secondDone)
	if recoveryObservationErr != nil {
		t.Fatal("original restart recovery observation deadline", recoveryObservationErr)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if after.Status != workflows.RunStatusSucceeded || after.ID != before.ID || !after.StartedAt.Equal(before.StartedAt) || after.Generation != resumed.Generation || after.Result.SessionID != before.Result.SessionID && before.Result.SessionID != "" {
		messages, _ := manager.Messages(context.Background(), run.ID, worker.ID)
		current, _ := manager.Get(context.Background(), run.ID, worker.ID)
		t.Fatalf("original recovery failed msg=%+v worker=%+v APIs=%d", messages, current, requests.Load())
	}
	if after.Result.Outputs["text"] != "ORIGINAL_EFFECTS_RECOVERED" || requests.Load() != 2 || effects.Load() != 2 {
		t.Fatal(after, requests.Load(), effects.Load())
	}
	current, err := manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal("recovery reset/decremented original allowance", current, err)
	}
	physical, err := os.ReadFile(effectFile)
	if err != nil || strings.Count(string(physical), "\n") != 2 {
		t.Fatal("physical effects duplicated", string(physical), err)
	}
	calls, err := store.ToolCalls(ctx, run.ID, false)
	if err != nil || len(calls) != 2 {
		t.Fatal(calls, err)
	}
	for _, call := range calls {
		if call.Generation != originalGeneration || call.InvocationID != message.ID || call.Status != "completed" {
			t.Fatal("original effect receipt rewritten", call)
		}
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	runtimeRows, err := store.Invocations(ctx, run.ID, false)
	if err != nil || len(runtimeRows) != 2 {
		t.Fatal(runtimeRows, err)
	}
	for _, row := range runtimeRows {
		if row.State != "cleaned" || row.LogicalID != message.ID {
			t.Fatal("runtime ownership/join differs", row)
		}
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 22 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 6 || !report.Summary.Complete {
		t.Fatal("original usage lost/reset/recharged", report, err)
	}
	t.Logf("binary=%s killed-owned-native=%d owner=%s/%d→%s/%d original=%s started=%s Remaining=0 physical-effects=2 APIs=2 usage=22/6 cleaned-runtime-rows=2", identity.BinarySHA256, killedPID, originalOwner, originalGeneration, resumed.Owner, resumed.Generation, message.ID, before.StartedAt.Format(time.RFC3339Nano))
}

// Alter only the owned native raw history, leaving the sealed journal and
// private engine witness unchanged. An extra original call in that sealed
// segment must hold at the real recovery boundary before another API request.
func insertForeignManagedRawCall(t *testing.T, state string) {
	t.Helper()
	var rollouts []string
	if err := filepath.WalkDir(state, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			rollouts = append(rollouts, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(rollouts) != 1 {
		t.Fatal("exact owned native rollout required", rollouts)
	}
	path := rollouts[0]
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 64<<20 {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	inserted := false
	for index, line := range lines {
		var envelope map[string]any
		if json.Unmarshal([]byte(line), &envelope) != nil {
			continue
		}
		payload, _ := envelope["payload"].(map[string]any)
		if envelope["type"] != "response_item" || payload["type"] != "function_call" {
			continue
		}
		payload["call_id"] = "foreign-extra-original"
		payload["id"] = "foreign-extra-original"
		payload["arguments"] = `{"nonce":"foreign"}`
		extra, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines[:index], append([]string{string(extra)}, lines[index:]...)...)
		inserted = true
		break
	}
	if !inserted {
		t.Fatal("owned original raw call not found")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}
