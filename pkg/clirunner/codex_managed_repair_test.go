//go:build linux

package clirunner

import (
	"context"
	"database/sql"
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

func TestManagedCodexFreshRepairAndOriginalContinuation(t *testing.T) {
	for _, forcedTool := range []bool{false, true} {
		t.Run(fmt.Sprint("forced-native-tool-", forcedTool), func(t *testing.T) { managedCodexRepairFixture(t, forcedTool) })
	}
}
func managedCodexRepairFixture(t *testing.T, forcedTool bool) {
	identity := managedNativeFixtureIdentity(t)
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	store, err := workflows.OpenSQLite(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &workflows.Run{ID: "managed-repair-isolation", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	const originalPrompt = "ORIGINAL_PROMPT_NONCE_134"
	const originalInput = "ORIGINAL_INPUT_NONCE_134"
	const originalInstructions = "ORIGINAL_INSTRUCTIONS_NONCE_134"
	const repairData = "REPAIR_DATA_NONCE_134"
	const ambient = "UNADMITTED_AGENTS_NONCE_134"
	if err = os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(ambient), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "repair-must-not-write")
	var requests, callbacks atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			t.Error("unexpected fake API path", r.URL.Path)
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
			Tools []any `json:"tools"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(raw), ambient) {
			t.Error("ambient project instructions entered managed turn")
		}
		if number == 2 {
			if len(body.Tools) != 0 || !strings.Contains(string(raw), repairData) {
				t.Error("repair catalog/data differs", string(raw))
			}
			for _, nonce := range []string{originalPrompt, originalInput, originalInstructions} {
				if strings.Contains(string(raw), nonce) {
					t.Error("repair inherited task context", nonce)
				}
			}
		} else {
			if number > 3 || forcedTool && number > 2 {
				t.Error("unexpected replacement model request", number)
			}
			if len(body.Tools) != 2 || !strings.Contains(string(raw), originalPrompt) || !strings.Contains(string(raw), originalInput) || !strings.Contains(string(raw), originalInstructions) {
				t.Error("original task context/catalog lost", string(raw))
			}
			if strings.Contains(string(raw), repairData) {
				t.Error("original continuation imported isolated repair")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(value map[string]any) {
			data, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value["type"], data)
		}
		response := fmt.Sprint("repair-fixture-response-", number)
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": response}})
		if number == 2 && forcedTool {
			args, _ := json.Marshal(map[string]any{"cmd": "touch " + marker, "yield_time_ms": 1000})
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "function_call", "id": "fc-denied", "call_id": "repair-denied-native", "name": "exec_command", "arguments": string(args)}})
		} else {
			text := []string{"original-non-JSON-output", `{"fixed":true}`, "ORIGINAL_SESSION_CONTINUED"}[number-1]
			send(map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": fmt.Sprint("repair-message-", number), "content": []any{map[string]any{"type": "output_text", "text": text}}}})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": response, "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
	}))
	defer gateway.Close()
	t.Setenv("MANAGED_REPAIR_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"effects": {Provider: "codex-app-server", Model: "gpt-5.4", InstructionText: originalInstructions, Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_REPAIR_FIXTURE_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture := &managedEngineFixture{runner: runner, identity: identity, pool: pool, gatewayURL: gateway.URL}
	manager := &workflows.WorkerManager{Store: store, Capacity: pool, Factory: func(context.Context, string, string, []string) (workflows.SessionRunner, []string, map[string]string, error) {
		return fixture, []string{}, nil, nil
	}}
	broker := &workflows.Broker{Store: store, Workers: manager, External: func(context.Context, workflows.ToolDefinition, json.RawMessage) (json.RawMessage, error) {
		callbacks.Add(1)
		return nil, fmt.Errorf("repair must not dispatch an effect")
	}}
	manager.BindTools = func(ctx context.Context, in workflows.ToolInvocation) (workflows.ToolBinding, error) {
		in.Catalog = managedEffectDefinitions()
		return broker.Bind(ctx, in)
	}
	if err = manager.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	turns := 3
	if forcedTool {
		turns = 2
	}
	worker, err := manager.Spawn(t.Context(), workflows.WorkerSpec{RunID: run.ID, Role: "effects", Turns: turns})
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := sql.Open("sqlite", filepath.Join(stateRoot, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	// The private engine dispatcher writes these existing message fields. This
	// owned queued-message fixture supplies that same durable repair boundary;
	// it adds no production request field or runtime admission bypass.
	enqueue := func(requestID, prompt string, repair bool) workflows.WorkerMessage {
		message, err := manager.Send(t.Context(), run.ID, worker.ID, requestID, prompt)
		if err != nil {
			t.Fatal(err)
		}
		message.RepairOnly = repair
		message.Input = map[string]any{"original": originalInput}
		if repair {
			message.Output = &workflows.AgentOutputContract{Schema: map[string]any{"type": "object"}, RepairAttempts: 1}
		}
		raw, _ := json.Marshal(message)
		result, err := diagnostic.Exec(`UPDATE messages SET record=? WHERE id=? AND status='queued'`, raw, message.ID)
		if err != nil {
			t.Fatal(err)
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			t.Fatal("queued fixture was claimed before setup", count, err)
		}
		return message
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	execute := func(message workflows.WorkerMessage) workflows.WorkerMessage {
		turnCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { manager.Serve(turnCtx); close(done) }()
		defer func() { stop(); <-done }()
		for ctx.Err() == nil {
			messages, err := manager.Messages(ctx, run.ID, worker.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, saved := range messages {
				if saved.ID == message.ID && saved.Status != workflows.RunStatusQueued && saved.Status != workflows.RunStatusRunning {
					return saved
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("owned repair fixture deadline", ctx.Err())
		return workflows.WorkerMessage{}
	}
	first := execute(enqueue("original-task", originalPrompt, false))
	if first.Status != workflows.RunStatusSucceeded || first.Result.SessionID == "" {
		t.Fatal(first)
	}
	repair := execute(enqueue("fresh-repair", "Format only this repair data: "+repairData, true))
	current, err := manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.SessionID != first.Result.SessionID {
		t.Fatal("repair replaced original continuation session", current, err)
	}
	if repair.Result.SessionID == "" || repair.Result.SessionID == first.Result.SessionID || callbacks.Load() != 0 {
		t.Fatal("repair did not use a fresh tool-free context", repair, callbacks.Load())
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("forced native repair tool produced a filesystem effect", err)
	}
	if forcedTool {
		if repair.Status != workflows.RunStatusFailed || requests.Load() != 2 || current.Remaining != 0 {
			t.Fatal("forced omitted native tool escaped fresh repair ceiling", repair, requests.Load(), current.Remaining)
		}
		if err = manager.Resume(ctx, run.ID, worker.ID); err == nil {
			t.Fatal("failed/unknown repair was acknowledged as fresh authority")
		}
	} else {
		if repair.Status != workflows.RunStatusSucceeded || repair.Result.Outputs["text"] != `{"fixed":true}` {
			t.Fatal(repair)
		}
		continuation := execute(enqueue("original-continuation", "Continue the original session", false))
		if continuation.Status != workflows.RunStatusSucceeded || continuation.Result.SessionID != first.Result.SessionID || continuation.Result.Outputs["text"] != "ORIGINAL_SESSION_CONTINUED" || requests.Load() != 3 {
			t.Fatal("original continuation lost its history/binding", continuation, requests.Load())
		}
		current, err = manager.Get(ctx, run.ID, worker.ID)
		if err != nil || current.Remaining != 0 {
			t.Fatal("repair reset original allowance", current, err)
		}
		report, err := store.Usage(ctx, run.ID)
		if err != nil || !report.Summary.Complete || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 33 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 9 {
			t.Fatal("fresh repair accounting skipped/doubled a response", report, err)
		}
	}
	if err = store.CheckInvocationCleanup(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("binary=%s forced-native=%v original-session=%s repair-session=%s requests=%d callbacks=0 marker-absent=true original-allowance-remaining=0", identity.BinarySHA256, forcedTool, first.Result.SessionID, repair.Result.SessionID, requests.Load())
}
