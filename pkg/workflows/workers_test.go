package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeSession struct {
	mu      sync.Mutex
	prompts []string
	release chan struct{}
	started chan string
}

func (f *fakeSession) Capabilities(string) (ProviderCapabilities, error) {
	return ProviderCapabilities{Sessions: true, Resume: true, Interrupt: true, ManagedPolicy: true}, nil
}
func (f *fakeSession) RunTurn(ctx context.Context, req SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, req.Prompt)
	f.mu.Unlock()
	id := req.SessionID
	if id == "" {
		id = "provider-session"
	}
	if emit != nil {
		emit(ProviderEvent{Type: "session", SessionID: id})
	}
	if f.started != nil {
		f.started <- req.Prompt
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return SessionResult{SessionID: id, Status: "interrupted"}, ctx.Err()
		}
	}
	return SessionResult{SessionID: id, Status: RunStatusSucceeded, Outputs: map[string]any{"text": req.Prompt}}, nil
}
func workerFixture(t *testing.T, f *fakeSession) (*WorkerManager, string) {
	t.Helper()
	e, s := durableExecutor(t, "jobs: {x: {steps: [{argv: [true]}]}}", nil)
	run, err := e.Prepare(context.Background(), RunRequest{Ref: "workflows/main.yml"})
	if err != nil {
		t.Fatal(err)
	}
	run.Status = RunStatusRunning
	if err = s.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	m := &WorkerManager{Store: s, Slots: make(chan struct{}, 1), Factory: func(_ context.Context, _, _ string, tools []string) (SessionRunner, []string, map[string]string, error) {
		if tools == nil {
			tools = []string{"Read", "Bash"}
		}
		return f, tools, nil, nil
	}}
	if err = m.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m, run.ID
}
func serveWorkers(t *testing.T, m *WorkerManager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not stop")
		}
	})
	return cancel
}
func awaitMessages(t *testing.T, m *WorkerManager, run, id string, count int) []WorkerMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		messages, err := m.Messages(context.Background(), run, id)
		if err != nil {
			t.Fatal(err)
		}
		completed := 0
		for _, msg := range messages {
			if msg.Status == RunStatusSucceeded || msg.Status == "interrupted" {
				completed++
			}
		}
		if completed == count {
			return messages
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailbox did not finish: %+v", messages)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestDurableOrderedMailboxDeduplicatesRequestIDs(t *testing.T) {
	f := &fakeSession{}
	m, run := workerFixture(t, f)
	w, err := m.Spawn(context.Background(), WorkerSpec{RunID: run, Role: "a", Turns: 8})
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Send(context.Background(), run, w.ID, "req-1", "one")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := m.Send(context.Background(), run, w.ID, "req-1", "one")
	if err != nil || duplicate.ID != first.ID {
		t.Fatal("duplicate request did not return same durable acknowledgement")
	}
	if _, err = m.Send(context.Background(), run, w.ID, "req-1", "different"); err == nil {
		t.Fatal("request ID conflict accepted")
	}
	if _, err = m.Send(context.Background(), run, w.ID, "req-2", "two"); err != nil {
		t.Fatal(err)
	}
	serveWorkers(t, m)
	messages := awaitMessages(t, m, run, w.ID, 2)
	if len(messages) != 2 || messages[0].Sequence != 1 || messages[1].Sequence != 2 {
		t.Fatal(messages)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.prompts) != "[one two]" {
		t.Fatal(f.prompts)
	}
	saved, err := m.Get(context.Background(), run, w.ID)
	if err != nil || saved.SessionID != "provider-session" || saved.Remaining != 6 {
		t.Fatalf("session/budget lost %+v %v", saved, err)
	}
}
func TestChildBudgetPermissionsAndCrossRunDenial(t *testing.T) {
	m, run := workerFixture(t, &fakeSession{})
	ctx := context.Background()
	root, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "a", Tools: []string{"Read"}, Turns: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Spawn(ctx, WorkerSpec{RunID: run, ParentID: root.ID, Role: "a", Tools: []string{"Read", "Bash"}, Turns: 1}); err == nil {
		t.Fatal("child elevated permissions")
	}
	child, err := m.Spawn(ctx, WorkerSpec{RunID: run, ParentID: root.ID, Role: "a", Tools: []string{"Read"}, Turns: 3})
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := m.Get(ctx, run, root.ID)
	if parent.Remaining != 5 || child.Remaining != 3 {
		t.Fatal("child budget was not reserved")
	}
	if _, err = m.Get(ctx, "other-run", child.ID); err == nil {
		t.Fatal("cross-run get allowed")
	}
	if _, err = m.Send(ctx, "other-run", child.ID, "x", "hello"); err == nil {
		t.Fatal("cross-run send allowed")
	}
	if err = m.Interrupt(ctx, "other-run", child.ID, true); err == nil {
		t.Fatal("cross-run close allowed")
	}
	if err = m.Interrupt(ctx, run, child.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Send(ctx, run, child.ID, "x", "hello"); err == nil {
		t.Fatal("closed worker accepted a message")
	}
}
func TestInterruptPreservesSessionAndNextMessage(t *testing.T) {
	f := &fakeSession{release: make(chan struct{}), started: make(chan string, 2)}
	m, run := workerFixture(t, f)
	ctx := context.Background()
	w, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "a", Turns: 4})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = m.Send(ctx, run, w.ID, "one", "one")
	serveWorkers(t, m)
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	if err = m.Interrupt(ctx, run, w.ID, false); err != nil {
		t.Fatal(err)
	}
	awaitMessages(t, m, run, w.ID, 1)
	saved, _ := m.Get(ctx, run, w.ID)
	if saved.SessionID == "" || saved.Status != "idle" {
		t.Fatalf("interruption lost session: %+v", saved)
	}
	close(f.release)
	_, _ = m.Send(ctx, run, w.ID, "two", "two")
	messages := awaitMessages(t, m, run, w.ID, 2)
	if messages[0].Status != "interrupted" || messages[1].Status != RunStatusSucceeded {
		t.Fatal(messages)
	}
}
func TestWorkerRecoveryNeverReplaysAnUncertainTurn(t *testing.T) {
	f := &fakeSession{}
	m, run := workerFixture(t, f)
	ctx := context.Background()
	w, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "a", Turns: 4})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Send(ctx, run, w.ID, "one", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	w.Status = RunStatusRunning
	w.SessionID = "saved-session"
	_ = m.saveWorker(ctx, w)
	msg.Status = RunStatusRunning
	b, _ := json.Marshal(msg)
	_, _ = m.Store.db.Exec("UPDATE messages SET status=?,record=? WHERE id=?", msg.Status, b, msg.ID)
	if err = m.Init(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ := m.Get(ctx, run, w.ID)
	if saved.Status != RunStatusNeedsAttention {
		t.Fatal(saved)
	}
	if err = m.Resume(ctx, run, w.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Send(ctx, run, w.ID, "two", "next")
	serveWorkers(t, m)
	messages := awaitMessages(t, m, run, w.ID, 2)
	if messages[0].Status != "interrupted" {
		t.Fatal("uncertain turn replayed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.prompts) != "[next]" {
		t.Fatal(f.prompts)
	}
}

type accessSession struct {
	*fakeSession
	access WorkerAccess
}

func (s accessSession) WorkerAccess(string) (WorkerAccess, error) { return s.access, nil }
func TestChildCannotEscapeContainerBoundary(t *testing.T) {
	f := &fakeSession{}
	m, run := workerFixture(t, f)
	m.Factory = func(_ context.Context, _, role string, tools []string) (SessionRunner, []string, map[string]string, error) {
		access := WorkerAccess{Container: true, Writable: false, Workspace: "/repo"}
		switch role {
		case "local":
			access.Container = false
			access.Writable = true
		case "writable":
			access.Writable = true
		case "outside":
			access.Workspace = "/other"
		}
		return accessSession{fakeSession: f, access: access}, []string{"Read"}, nil, nil
	}
	ctx := context.Background()
	parent, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "parent", Turns: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"local", "writable", "outside"} {
		if _, err = m.Spawn(ctx, WorkerSpec{RunID: run, ParentID: parent.ID, Role: role, Turns: 1}); err == nil {
			t.Fatalf("child %s exceeded container boundary", role)
		}
	}
	if _, err = m.Spawn(ctx, WorkerSpec{RunID: run, ParentID: parent.ID, Role: "readonly", Turns: 1}); err != nil {
		t.Fatal(err)
	}
}
