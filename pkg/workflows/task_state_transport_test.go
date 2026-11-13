package workflows

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func taskStateEndpoint(t *testing.T) (*taskStateService, *SQLiteStore, *Run, context.Context) {
	t.Helper()
	store, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	run := &Run{ID: randomID(), Owner: randomID(), Generation: 1, Status: RunStatusRunning, Version: 1, Jobs: map[string]JobExecution{}}
	if err = store.Admit(t.Context(), run, Snapshot{Version: 1, Metadata: map[string]any{"task_group": "transport-tests"}}); err != nil {
		t.Fatal(err)
	}
	ctx := fenceContext(t.Context(), run)
	service, err := (&Executor{Store: store}).newTaskStateService(ctx)
	if err != nil || service == nil {
		t.Fatalf("endpoint: %v", err)
	}
	t.Cleanup(func() {
		if err := service.close(); err != nil {
			t.Error(err)
		}
	})
	return service, store, run, ctx
}

func taskStateRPC(t *testing.T, service *taskStateService, token, body string) (int, []byte) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(service.dir, "rpc.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), "POST", "http://localhost/v1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}

func TestTaskStateTransportCapabilitiesFencingAndPhaseRestrictions(t *testing.T) {
	s, store, run, ctx := taskStateEndpoint(t)
	descriptor, revoke, err := s.capability(ctx, "execute")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revoke)
	reserve := `{"operation":"reserve","name":"reviews","operation_id":"review-1","limit":2,"amount":1}`
	if code, _ := taskStateRPC(t, s, "wrong", reserve); code != http.StatusUnauthorized {
		t.Fatalf("unscoped request accepted: %d", code)
	}
	for _, malformed := range []string{
		`{"operation":"get_budget","name":"reviews","group":"other"}`,
		`{"operation":"get_budget","name":"reviews"} {}`,
		`{"operation":"get_budget","name":"reviews","owner":"other"}`,
	} {
		if code, _ := taskStateRPC(t, s, descriptor.Token, malformed); code != http.StatusBadRequest {
			t.Fatalf("override or trailing JSON accepted (%d): %s", code, malformed)
		}
	}
	if code, body := taskStateRPC(t, s, descriptor.Token, reserve); code != http.StatusOK || !bytes.Contains(body, []byte(`"used":1`)) {
		t.Fatalf("reservation: %d %s", code, body)
	}
	for _, phase := range []string{"bindings", "validate_checkpoint", "reconcile"} {
		read, finish, err := s.capability(ctx, phase)
		if err != nil {
			t.Fatal(err)
		}
		if code, body := taskStateRPC(t, s, read.Token, `{"operation":"get_budget","name":"reviews"}`); code != http.StatusOK {
			t.Fatalf("%s read: %d %s", phase, code, body)
		}
		if code, _ := taskStateRPC(t, s, read.Token, reserve); code != http.StatusForbidden {
			t.Fatalf("%s write accepted: %d", phase, code)
		}
		finish()
		if code, _ := taskStateRPC(t, s, read.Token, `{"operation":"get_budget","name":"reviews"}`); code != http.StatusUnauthorized {
			t.Fatalf("revoked capability accepted: %d", code)
		}
	}
	if _, err = store.db.Exec("UPDATE runs SET generation=generation+1 WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := taskStateRPC(t, s, descriptor.Token, reserve); code != http.StatusConflict || !bytes.Contains(body, []byte("ownership changed")) {
		t.Fatalf("stale owner accepted: %d %s", code, body)
	}
}

func TestTaskStateTransportBoundsAndCancellation(t *testing.T) {
	s, _, _, ctx := taskStateEndpoint(t)
	owned, cancel := context.WithCancel(ctx)
	descriptor, revoke, err := s.capability(owned, "execute")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revoke)
	body := `{"operation":"get_claim","task":"` + strings.Repeat("x", 1<<20) + `"}`
	if code, _ := taskStateRPC(t, s, descriptor.Token, body); code != http.StatusBadRequest {
		t.Fatalf("oversized request accepted: %d", code)
	}
	cancel()
	if code, _ := taskStateRPC(t, s, descriptor.Token, `{"operation":"get_budget","name":"reviews"}`); code != http.StatusUnauthorized {
		t.Fatalf("canceled hook accepted: %d", code)
	}
}

func TestTaskStateCapabilityRevocationCancelsAndJoinsPendingStoreRPC(t *testing.T) {
	s, store, _, ctx := taskStateEndpoint(t)
	descriptor, revoke, err := s.capability(ctx, "execute")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revoke)
	// Occupy the sole SQLite connection so the authenticated RPC is observably
	// pending inside the store rather than merely queued in a network reader.
	connection, err := store.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	initialWaits := store.db.Stats().WaitCount
	result := make(chan int, 1)
	go func() {
		code, _ := taskStateRPC(t, s, descriptor.Token, `{"operation":"reserve","name":"reviews","operation_id":"blocked","limit":1,"amount":1}`)
		result <- code
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for store.db.Stats().WaitCount == initialWaits {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("authenticated RPC did not enter the bounded SQLite wait")
		}
	}
	joined := make(chan struct{})
	go func() { revoke(); close(joined) }()
	select {
	case <-joined:
	case <-deadline.C:
		t.Fatal("capability revocation did not join the pending store operation")
	}
	select {
	case code := <-result:
		if code != http.StatusConflict {
			t.Fatalf("pending write survived hook revocation: %d", code)
		}
	case <-deadline.C:
		t.Fatal("canceled RPC did not finish")
	}
}

type taskStateGatedBody struct {
	io.Reader
	reading chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (b *taskStateGatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	<-b.resume
	return b.Reader.Read(p)
}

func TestTaskStateCapabilityRevocationRejectsPendingRequestBody(t *testing.T) {
	s, store, _, ctx := taskStateEndpoint(t)
	descriptor, revoke, err := s.capability(ctx, "execute")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revoke)
	body := &taskStateGatedBody{
		Reader:  strings.NewReader(`{"operation":"reserve","name":"reviews","operation_id":"partial-body","limit":1,"amount":1}`),
		reading: make(chan struct{}),
		resume:  make(chan struct{}),
	}
	resume := sync.OnceFunc(func() { close(body.resume) })
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1", body)
	request.Header.Set("Authorization", "Bearer "+descriptor.Token)
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { s.ServeHTTP(response, request); close(finished) }()
	t.Cleanup(func() {
		resume()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("pending request handler did not finish")
		}
	})
	select {
	case <-body.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated request did not begin reading its body")
	}
	revoked := make(chan struct{})
	go func() { revoke(); close(revoked) }()
	select {
	case <-revoked:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation waited for an incomplete request body")
	}
	resume()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after its body was released")
	}
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("pending body retained revoked capability: %d", response.Code)
	}
	if _, err := store.TaskState(ctx, TaskStateRequest{Operation: "get_budget", Name: "reviews"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("pending request accessed the store after revocation: %v", err)
	}
}

func TestTaskStateTransportUngroupedRunHasNoEndpoint(t *testing.T) {
	store, err := OpenSQLite(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := &Run{ID: randomID(), Owner: randomID(), Status: RunStatusRunning, Jobs: map[string]JobExecution{}}
	if err = store.Admit(t.Context(), run, Snapshot{Version: 1}); err != nil {
		t.Fatal(err)
	}
	service, err := (&Executor{Store: store}).newTaskStateService(fenceContext(t.Context(), run))
	if err != nil || service != nil {
		t.Fatalf("ungrouped run exposed endpoint: %v %v", service, err)
	}
}

func TestTaskStatePythonHelperThroughFakeContainers(t *testing.T) {
	for _, mode := range []string{"shared", "standalone", "shared-closed", "standalone-closed"} {
		t.Run(mode, func(t *testing.T) {
			closed := strings.HasSuffix(mode, "-closed")
			mode = strings.TrimSuffix(mode, "-closed")
			e, store, docker := containerExecutor(t)
			var wireMu sync.Mutex
			var tokens []string
			var validations int
			directories := map[string]bool{}
			docker.helper = func(_ []string, input []byte, _ []string) ([]byte, []byte, int, bool) {
				var request struct {
					TaskState *taskStateDescriptor `json:"task_state"`
					Phase     string               `json:"phase"`
				}
				if json.Unmarshal(input, &request) == nil && request.TaskState != nil {
					wireMu.Lock()
					tokens = append(tokens, request.TaskState.Token)
					if request.Phase == "validate_checkpoint" {
						validations++
					}
					docker.mu.Lock()
					for _, container := range docker.containers {
						if container.taskStateDir != "" {
							directories[container.taskStateDir] = true
						}
					}
					docker.mu.Unlock()
					wireMu.Unlock()
				}
				return nil, nil, 0, false
			}
			for _, name := range []string{"client.py", "helper.py"} {
				data, err := os.ReadFile(filepath.Join("..", "..", "examples", "task-state", "workflows", name))
				if err != nil {
					t.Fatal(err)
				}
				if closed && name == "helper.py" {
					data = []byte(strings.Replace(string(data), `    return {"result":`, "    state.call(\"close_group\", request_id=\"finish\", instruction=\"close completed capture\", source=\"test\")\n    return {\"result\":", 1))
				}
				if err = os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := os.ReadFile(filepath.Join("..", "..", "examples", "task-state", "workflows", "main.yml"))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			if mode == "shared" {
				// Start the resident container before any structured hook: its
				// endpoint mount must already be available when the helper runs.
				source = strings.Replace(source, "    steps:\n", "    steps:\n      - id: prepare\n        argv: [printf, ready]\n", 1)
			}
			if mode == "standalone" {
				source = strings.Replace(source, "    container:\n      image: python:3.13-slim\n      files: [client.py, helper.py]\n", "", 1)
				source = strings.Replace(source, "        argv: [python3, -B, /actions/helper.py]", "        uses: docker://python:3.13-slim\n        files: [client.py, helper.py]\n        with: {entrypoint: python3, args: '-B /actions/helper.py'}", 1)
			}
			if err = os.WriteFile(filepath.Join(e.WorkspaceDir, "workflows", "test.yml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			run, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", TaskGroup: "python-capture", Inputs: map[string]any{"value": "ready", "operation_id": "capture-1"}})
			if err != nil {
				t.Fatal(err)
			}
			if run.Jobs["capture"].Steps["capture"].Outputs["value"] != "ready" {
				t.Fatalf("business output lost: %+v", run)
			}
			observed, err := store.InspectTaskState(t.Context(), run.ID)
			if err != nil || len(observed.Claims) != 1 || observed.Claims[0].State != "completed" || len(observed.Budgets) != 1 || observed.Budgets[0].Used != 1 {
				t.Fatalf("missing logical state: %+v %v", observed, err)
			}
			if _, err = e.Resume(t.Context(), run.ID, nil, false); err != nil {
				t.Fatalf("owned read-only validation failed: %v", err)
			}
			wireMu.Lock()
			validated := validations > 0
			wireMu.Unlock()
			if !validated {
				t.Fatal("successful resume skipped the checkpoint validation hook")
			}
			second, err := e.Run(t.Context(), RunRequest{Ref: "workflows/test.yml", TaskGroup: "python-capture", Inputs: map[string]any{"value": "ready", "operation_id": "capture-1"}})
			if closed {
				if err == nil || !strings.Contains(err.Error(), "permanently closed") {
					t.Fatalf("closed group admitted replacement work: %v", err)
				}
				second = run
			} else if err != nil {
				t.Fatalf("replacement run did not reuse claim: %v", err)
			}
			observed, err = store.InspectTaskState(t.Context(), second.ID)
			if err != nil || observed.Closed != closed || len(observed.Claims) != 1 || len(observed.Budgets) != 1 || observed.Budgets[0].Used != 1 {
				t.Fatalf("replacement reset logical state: %+v %v", observed, err)
			}
			wireMu.Lock()
			defer wireMu.Unlock()
			if len(tokens) < 3 || len(directories) == 0 {
				t.Fatalf("missing real hook capabilities/mounts: tokens=%d directories=%d", len(tokens), len(directories))
			}
			seen := map[string]bool{}
			for _, token := range tokens {
				if seen[token] {
					t.Fatal("a capability survived into another hook")
				}
				seen[token] = true
			}
			for directory := range directories {
				if _, err := os.Stat(directory); !os.IsNotExist(err) {
					t.Fatalf("endpoint directory survived owned teardown: %s (%v)", directory, err)
				}
			}
			for _, id := range []string{run.ID, second.ID} {
				attempts, err := store.ActionAttempts(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				_, snapshot, err := store.Load(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(struct {
					Attempts []ActionAttempt
					Snapshot Snapshot
				}{attempts, snapshot})
				if err != nil {
					t.Fatal(err)
				}
				for _, token := range tokens {
					if bytes.Contains(raw, []byte(token)) {
						t.Fatal("ephemeral task-state capability persisted in action checkpoint or admitted snapshot")
					}
				}
			}
		})
	}
}
