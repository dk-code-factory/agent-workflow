package clirunner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
func TestSDKManagedTerminalCheckpointReconcilesWithoutProvider(t *testing.T) {
	if os.Getenv("AGENT_WORKFLOW_SDK_TEST") != "1" {
		t.Skip("locked SDK/local fake gateway only")
	}
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
	run := &workflows.Run{ID: "sdk-managed-terminal-checkpoint", Status: workflows.RunStatusQueued, Version: 1, Jobs: map[string]workflows.JobExecution{}}
	if err = store.Admit(t.Context(), run, workflows.Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	run.Status = workflows.RunStatusRunning
	if err = store.Claim(t.Context(), run, workflows.RunStatusQueued); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			fmt.Fprint(w, `{"input_tokens":16,"data":[],"has_more":false}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if tools, _ := body["tools"].([]any); len(tools) != 0 {
			t.Errorf("empty SDK catalog exposed tools %+v", tools)
		}
		if requests.Add(1) != 1 {
			t.Error("SDK terminal recovery made another provider request")
		}
		model, _ := body["model"].(string)
		writeSDKMessage(w, model, []map[string]any{{"type": "text", "text": "ORIGINAL_TERMINAL_RECEIPT"}}, "end_turn")
	}))
	defer gateway.Close()
	bridge, err := filepath.Abs("../../bridge/claude/bridge.mjs")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDK_TERMINAL_FIXTURE_KEY", "fake-only")
	runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"empty": {Provider: "claude-sdk", Bridge: bridge, Model: "claude-sonnet-4-6", Authentication: Authentication{Mode: "gateway", Endpoint: gateway.URL, CredentialEnv: "SDK_TERMINAL_FIXTURE_KEY"}}}}}
	pool := &workflows.Capacity{Slots: make(chan struct{}, 1), MaxSuspended: 4}
	fixture := &sdkManagedEngineFixture{runner: runner, pool: pool}
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
		queryErr := diagnostic.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations WHERE invocation_id=? AND json_extract(CAST(record AS TEXT),'$.managed_sdk_attempt.state')='terminal'`, message.ID).Scan(&terminals)
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
	fixture = &sdkManagedEngineFixture{runner: runner, pool: pool}
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
	if err != nil || report.Summary.InputTokens == nil || *report.Summary.InputTokens != 16 || report.Summary.OutputTokens == nil || *report.Summary.OutputTokens != 5 || !report.Summary.Complete {
		t.Fatal(report, err)
	}
	if after.Result.ProviderVersion != "claude-agent-sdk@0.3.322" {
		t.Fatal("SDK original provider identity changed")
	}
	t.Logf("SDK .322 original=%s owner=%s/%d->%s/%d sameStartedAt/Remaining0/API1/runtimecleaned1/usage16-5/completiontxabort/native-free", message.ID, run.Owner, run.Generation, resumed.Owner, resumed.Generation)

}
