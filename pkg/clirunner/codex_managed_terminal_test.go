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

// Abort only the owned disposable fixture's final mailbox transaction. The
// source terminal and private engine checkpoint have already completed; this
// exercises native-free reconciliation of a lost completion acknowledgement.
func TestManagedCodexTerminalCheckpointReconcilesWithoutProvider(t *testing.T) {
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
	run := &workflows.Run{ID: "managed-terminal-checkpoint", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var body struct {
			Tools []any `json:"tools"`
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Tools) != 0 {
			t.Error("empty managed catalog exposed tools", string(raw))
		}
		if requests.Add(1) != 1 {
			t.Error("terminal recovery made another provider request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": "terminal-original"}}, {"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "id": "terminal-message", "content": []any{map[string]any{"type": "output_text", "text": "ORIGINAL_TERMINAL_RECEIPT"}}}}, {"type": "response.completed", "response": map[string]any{"id": "terminal-original", "usage": map[string]any{"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}}} {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	defer gateway.Close()
	t.Setenv("MANAGED_TERMINAL_FIXTURE_KEY", "synthetic-no-provider-key")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"empty": {Provider: "codex-app-server", Model: "gpt-5.4", Authentication: Authentication{Mode: "api-key", CredentialEnv: "MANAGED_TERMINAL_FIXTURE_KEY"}}}}}
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
	message, err := manager.Send(t.Context(), run.ID, worker.ID, "original-terminal", "format only the admitted data")
	if err != nil {
		t.Fatal(err)
	}
	req := workflows.SessionRequest{AgentRequest: workflows.AgentRequest{RunID: run.ID, ParentWorkerID: worker.ID, InvocationID: message.ID, AgentID: "empty"}}
	state, err := providerStateDir(root, "codex", req)
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", gateway.URL)
	if err = os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := sql.Open("sqlite", filepath.Join(stateRoot, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	trigger := "CREATE TRIGGER owned_managed_completion_cut BEFORE UPDATE OF status ON messages WHEN NEW.id='" + message.ID + "' AND NEW.status='succeeded' BEGIN SELECT RAISE(ABORT,'owned terminal completion cut'); END"
	if _, err = diagnostic.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	firstCtx, stopFirst := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	go func() { manager.Serve(firstCtx); close(firstDone) }()
	defer func() { stopFirst(); <-firstDone }()
	var before workflows.WorkerMessage
	for ctx.Err() == nil {
		var terminals int
		queryErr := diagnostic.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND json_extract(CAST(record AS TEXT),'$.managed_codex_recovery.state')='terminal'`, message.ID).Scan(&terminals)
		messages, err := manager.Messages(ctx, run.ID, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		if queryErr == nil && terminals == 1 && store.CheckInvocationCleanup(ctx, run.ID) == nil && len(messages) == 1 && messages[0].Status == workflows.RunStatusRunning {
			before = messages[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if before.ID != message.ID || before.StartedAt.IsZero() || requests.Load() != 1 {
		t.Fatal("cut was not durable terminal before mailbox acknowledgement", before, requests.Load())
	}
	stopFirst()
	<-firstDone
	var finalRows int
	if err = diagnostic.QueryRow(`SELECT count(*) FROM usage_observations WHERE invocation_id=? AND observation_key='final'`, message.ID).Scan(&finalRows); err != nil || finalRows != 0 {
		t.Fatal("failed completion transaction partially committed accounting", finalRows, err)
	}
	if _, err = diagnostic.Exec(`DROP TRIGGER owned_managed_completion_cut`); err != nil {
		t.Fatal(err)
	}
	diagnostic.Close()
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
	resumed, err := store.QueueResume(ctx, run.ID, "terminal-recovery", true, 1)
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
	secondCtx, stopSecond := context.WithCancel(ctx)
	secondDone := make(chan struct{})
	go func() { manager.Serve(secondCtx); close(secondDone) }()
	defer func() { stopSecond(); <-secondDone }()
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
	if after.ID != before.ID || !after.StartedAt.Equal(before.StartedAt) || after.Generation != resumed.Generation || after.Result.Outputs["text"] != "ORIGINAL_TERMINAL_RECEIPT" || requests.Load() != 1 {
		messages, _ := manager.Messages(context.Background(), run.ID, worker.ID)
		t.Fatal("terminal acknowledgement recovery lost original receipt", messages, requests.Load())
	}
	current, err := manager.Get(ctx, run.ID, worker.ID)
	if err != nil || current.Remaining != 0 {
		t.Fatal("terminal replay changed consumed allowance", current, err)
	}
	runtimeRows, err := store.Invocations(ctx, run.ID, false)
	if err != nil || len(runtimeRows) != 1 || runtimeRows[0].State != "cleaned" {
		t.Fatal("terminal readback launched a provider", runtimeRows, err)
	}
	report, err := store.Usage(ctx, run.ID)
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 11 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 3 || !report.Summary.Complete {
		t.Fatal(report, err)
	}
	if !strings.Contains(after.Result.ProviderVersion, identity.BuildID) {
		t.Fatal("terminal checkpoint changed original build identity")
	}
	t.Logf("binary=%s original=%s owner=%s/%d→%s/%d original-start=%s Remaining=0 APIs=1 runtime-cleaned=1 usage=11/3 completion-cut=owned-SQLite-abort native-free=true", identity.BinarySHA256, message.ID, run.Owner, run.Generation, resumed.Owner, resumed.Generation, before.StartedAt.Format(time.RFC3339Nano))
}
