package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

type fakeRuntimeObject struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Argv      []string
	Workspace string
}

// This subprocess implements the public runtime CLI contract, never Docker or
// Podman itself. It runs only disposable fixture commands on the host.
func TestInvocationRuntimeFixture(t *testing.T) {
	root := os.Getenv("AW_RUNTIME_FIXTURE")
	if root == "" {
		return
	}
	var args []string
	for index, arg := range os.Args {
		if arg == "--" {
			args = os.Args[index+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	if _, err := os.Stat(filepath.Join(root, "offline")); err == nil {
		os.Exit(19)
	}
	objectPath := filepath.Join(root, "object.json")
	load := func() fakeRuntimeObject {
		data, err := os.ReadFile(objectPath)
		if err != nil {
			os.Exit(17)
		}
		var object fakeRuntimeObject
		if json.Unmarshal(data, &object) != nil {
			os.Exit(18)
		}
		return object
	}
	encode := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	modeBytes, _ := os.ReadFile(filepath.Join(root, "mode"))
	mode := strings.TrimSpace(string(modeBytes))
	switch args[0] {
	case "info":
		identity := "fixture-daemon"
		if data, err := os.ReadFile(filepath.Join(root, "identity")); err == nil {
			identity = string(data)
		}
		encode(map[string]any{"ID": identity})
	case "context":
		encode([]any{map[string]any{"Endpoints": map[string]any{"docker": map[string]any{"Host": "unix:///fixture-runtime"}}}})
	case "image":
		encode([]any{map[string]any{"Id": "sha256:" + strings.Repeat("b", 64), "Config": map[string]any{"Labels": map[string]string{"agent-workflow.guardian-protocol": "1", "agent-workflow.bridge-protocol": "2", "agent-workflow.claude-sdk": "0.3.301"}}}})
	case "create":
		object := fakeRuntimeObject{ID: strings.Repeat("a", 64)}
		object.Config.Labels = map[string]string{}
		for index := 1; index < len(args); index++ {
			switch args[index] {
			case "--name":
				index++
				object.Name = args[index]
			case "--label":
				index++
				key, value, _ := strings.Cut(args[index], "=")
				object.Config.Labels[key] = value
			case "--mount":
				index++
				fields := strings.Split(args[index], ",")
				source, target := "", ""
				for _, field := range fields {
					key, value, _ := strings.Cut(field, "=")
					if key == "src" {
						source = value
					}
					if key == "dst" {
						target = value
					}
				}
				if target == "/workspace" {
					object.Workspace = source
				}
			case "--entrypoint":
				for next := index + 3; next < len(args); next++ {
					if args[next] == "--" {
						object.Argv = args[next+1:]
						break
					}
				}
			}
		}
		if mode == "wrong-label" {
			object.Config.Labels["agent-workflow.token"] = "foreign"
		}
		data, _ := json.Marshal(object)
		if os.WriteFile(objectPath, data, 0600) != nil {
			os.Exit(18)
		}
		if mode == "lost-create-response" {
			os.Exit(19)
		}
		fmt.Println(object.ID)
	case "container":
		encode([]fakeRuntimeObject{load()})
	case "ps":
		if _, err := os.Stat(objectPath); err == nil {
			object := load()
			fmt.Println(object.ID + " " + object.Name)
		}
	case "rm":
		object := load()
		if args[len(args)-1] != object.ID {
			os.Exit(21)
		}
		_ = os.WriteFile(filepath.Join(root, "removed-by-id"), []byte(args[len(args)-1]), 0600)
		_ = os.Remove(objectPath)
	case "start":
		object := load()
		_ = os.WriteFile(filepath.Join(root, "started"), []byte("yes"), 0600)
		if len(object.Argv) == 0 {
			os.Exit(22)
		}
		cmd := exec.Command(object.Argv[0], object.Argv[1:]...)
		cmd.Dir = object.Workspace
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if mode == "outage-after-start" {
			_ = os.WriteFile(filepath.Join(root, "offline"), []byte("yes"), 0600)
		}
		if err != nil {
			os.Exit(23)
		}
	default:
		os.Exit(24)
	}
	os.Exit(0)
}

func containerExecutorFixture(t *testing.T, mode string) (*Executor, *SQLiteStore, string) {
	t.Helper()
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport AW_RUNTIME_FIXTURE='" + strings.ReplaceAll(root, "'", "'\\''") + "'\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestInvocationRuntimeFixture$ -- \"$@\"\n"
	if err = os.WriteFile(filepath.Join(root, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err = os.WriteFile(filepath.Join(root, "mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	e, store := durableExecutor(t, "jobs: {x: {steps: [{argv: [sh, -c, 'printf fixture-container']} ]}}", nil)
	e.CommandExecution = runtimeexec.Policy{Mode: "container", Runtime: "docker", Image: "sha256:" + strings.Repeat("b", 64), Writable: true, Cleanup: "strict"}
	return e, store, root
}

func TestContainerCommandCreatesBeforeStartAndCleansVerifiedIdentity(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "")
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatalf("container command: %+v %v", run, err)
	}
	if run.Jobs["x"].Steps["step_1"].Outputs["stdout"] != "fixture-container" {
		t.Fatal(run)
	}
	records, err := store.Invocations(context.Background(), run.ID, false)
	if err != nil || len(records) != 1 || records[0].State != "cleaned" || records[0].Container.ID == "" {
		t.Fatalf("invocation was not durable: %+v %v", records, err)
	}
	if _, err = os.Stat(filepath.Join(runtime, "removed-by-id")); err != nil {
		t.Fatal("container was not reconciled by verified ID", err)
	}
}

func TestLostCreateResponseNeverStartsWork(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "lost-create-response")
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status != RunStatusNeedsAttention {
		t.Fatal(run, err)
	}
	if _, err = os.Stat(filepath.Join(runtime, "started")); !os.IsNotExist(err) {
		t.Fatal("uncertain create replayed a start")
	}
	if err = store.CheckInvocationCleanup(context.Background(), run.ID); err != nil {
		t.Fatal("inert owned object was not reconciled", err)
	}
}

func TestRuntimeOutageRetainsUncertaintyAndRecoversWhenAvailable(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "outage-after-start")
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status != RunStatusNeedsAttention {
		t.Fatal(run, err)
	}
	if err = store.CheckCleanupPrerequisites(context.Background()); err == nil {
		t.Fatal("runtime outage treated as absence")
	}
	if err = os.Remove(filepath.Join(runtime, "offline")); err != nil {
		t.Fatal(err)
	}
	if err = store.ReconcileInvocations(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.CheckCleanupPrerequisites(context.Background()); err != nil {
		t.Fatal("recovered runtime still blocks admission", err)
	}
	if err = store.ReconcileInvocations(context.Background(), run.ID); err != nil {
		t.Fatal("reconciliation is not idempotent", err)
	}
}

func TestForeignContainerLabelsAreNeverRemoved(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "wrong-label")
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err == nil || run.Status != RunStatusNeedsAttention {
		t.Fatal(run, err)
	}
	if _, err = os.Stat(filepath.Join(runtime, "removed-by-id")); !os.IsNotExist(err) {
		t.Fatal("foreign labeled object was touched")
	}
	if err = store.CheckCleanupPrerequisites(context.Background()); err == nil {
		t.Fatal("foreign ownership uncertainty erased")
	}
}

type failCreatedJournal struct{ *SQLiteStore }

func (j failCreatedJournal) UpdateInvocation(ctx context.Context, record runtimeexec.Record) error {
	if record.State == "created" {
		return errors.New("fixture terminal storage write failed")
	}
	return j.SQLiteStore.UpdateInvocation(ctx, record)
}

func TestContainerCannotStartBeforeIdentityCommit(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "")
	run := &Run{ID: strings.Repeat("3", 32), Status: RunStatusRunning}
	if err := store.Admit(context.Background(), run, Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	ctx := runtimeexec.WithBinding(context.Background(), runtimeexec.Binding{Journal: failCreatedJournal{store}, Dir: store.Dir, RunID: run.ID, Generation: run.Generation, LogicalID: "identity-commit"})
	state := t.TempDir()
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf must-not-start")
	cmd.Dir = e.WorkspaceDir
	if _, err := runtimeexec.PrepareContainer(ctx, cmd, e.CommandExecution, runtimeexec.ContainerOptions{Workspace: e.WorkspaceDir, State: state, Root: e.WorkspaceDir}); err == nil {
		t.Fatal("failed identity commit allowed attached start")
	}
	if _, err := os.Stat(filepath.Join(runtime, "started")); !os.IsNotExist(err) {
		t.Fatal("work started before container identity was durable")
	}
	if err := store.ReconcileInvocations(context.Background(), run.ID); err != nil {
		t.Fatal("incomplete create could not be reconciled", err)
	}
}

func TestChangedRuntimeIdentityCannotProveAbsence(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "outage-after-start")
	run, _ := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err := os.Remove(filepath.Join(runtime, "offline")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtime, "identity"), []byte("another-daemon"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.ReconcileInvocations(context.Background(), run.ID); err == nil {
		t.Fatal("another daemon was treated as the originating runtime")
	}
	if _, err := os.Stat(filepath.Join(runtime, "removed-by-id")); !os.IsNotExist(err) {
		t.Fatal("object touched through a changed runtime identity")
	}
}

func TestCommandExecutionPolicyIsPinnedAcrossResume(t *testing.T) {
	e, store, runtime := containerExecutorFixture(t, "")
	run, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := store.Load(context.Background(), run.ID)
	if err != nil || snapshot.Execution.Mode != "container" {
		t.Fatal("command policy was not admitted", snapshot.Execution, err)
	}
	e.CommandExecution = runtimeexec.Policy{} // changing current configuration cannot weaken admission
	result, err := e.Resume(context.Background(), run.ID, nil, false)
	if err != nil || result.Status != RunStatusSucceeded {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(runtime, "started")); err != nil {
		t.Fatal("resume silently switched command execution to native", err)
	}
}

func TestReconciliationFiltersLiveInvocationsBeforeBatchLimit(t *testing.T) {
	manager, run := workerFixture(t, &fakeSession{})
	ctx := context.Background()
	tx, err := manager.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index := 0; index < 110; index++ {
		// An invalid body also proves live records were not decoded by reconciliation.
		if _, err = tx.ExecContext(ctx, "INSERT INTO invocations(id,run_id,generation,state,record,updated) VALUES(?,?,1,'running',?,?)", fmt.Sprintf("live-%03d", index), run, []byte("invalid live record"), index); err != nil {
			t.Fatal(err)
		}
	}
	record := runtimeexec.Record{ID: "cleanup", RunID: run, Generation: 1, State: "uncertain"}
	data, _ := json.Marshal(record)
	if _, err = tx.ExecContext(ctx, "INSERT INTO invocations(id,run_id,generation,state,record,updated) VALUES(?,?,1,'uncertain',?,111)", record.ID, run, data); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	records, err := manager.Store.listInvocations(ctx, "", true, true)
	if err != nil || len(records) != 1 || records[0].ID != "cleanup" {
		t.Fatalf("live batch hid cleanup: %+v %v", records, err)
	}
}
