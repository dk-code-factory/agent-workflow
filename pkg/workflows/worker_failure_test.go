package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

type failedNativeSession struct {
	fakeSession
	failure error
}

func (s *failedNativeSession) RunTurn(ctx context.Context, req SessionRequest, _ func(ProviderEvent)) (SessionResult, error) {
	if s.started != nil {
		select {
		case s.started <- req.Prompt:
		case <-ctx.Done():
			return SessionResult{}, ctx.Err()
		}
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return SessionResult{}, ctx.Err()
		}
	}
	failure := s.failure
	if failure == nil {
		failure = errors.New("provider usage limit")
	}
	return SessionResult{Status: RunStatusFailed, SessionID: "original-session", Outputs: map[string]any{"original": "retained failure output"}}, failure
}

func failureFixture(t *testing.T) (*WorkerManager, WorkerFailureRequest, WorkerMessage) {
	t.Helper()
	return failureFixtureWithSession(t, &failedNativeSession{})
}

func failureFixtureWithSession(t *testing.T, session *failedNativeSession) (*WorkerManager, WorkerFailureRequest, WorkerMessage) {
	t.Helper()
	m, run := workerFixture(t, &fakeSession{})
	m.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return session, nil, nil, nil
	}
	w, err := m.Spawn(context.Background(), WorkerSpec{RunID: run, Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Send(context.Background(), run, w.ID, "failure", "bounded assignment")
	if err != nil {
		t.Fatal(err)
	}
	serveWorkers(t, m)
	deadline := time.Now().Add(5 * time.Second)
	for {
		messages, err := m.Messages(context.Background(), run, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) == 1 && messages[0].Status == RunStatusFailed {
			msg = messages[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native failure did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	r, _, err := m.Store.Load(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return m, WorkerFailureRequest{RunID: run, WorkerID: w.ID, MessageID: msg.ID, Generation: r.Generation, Owner: r.Owner, RequestID: "handled-1", Reason: "optional task", Evidence: "original-message:" + msg.ID}, msg
}

func TestFailureAcknowledgementRejectsMixedCancellation(t *testing.T) {
	for _, failure := range []error{ErrProviderFailure, ErrUsageExhausted, errors.New("independent command failed")} {
		t.Run(classifyFailure(failure), func(t *testing.T) {
			mixed := errors.Join(failure, context.Canceled)
			m, req, msg := failureFixtureWithSession(t, &failedNativeSession{failure: mixed})
			if msg.FailureClass != classifyFailure(failure) {
				t.Fatal("mixed cancellation replaced the independent diagnostic", msg.FailureClass)
			}
			if msg.UnsafeFailureClass != "canceled" {
				t.Fatal("mixed cancellation lost its admission identity", msg.UnsafeFailureClass)
			}
			if _, err := m.AcknowledgeFailure(context.Background(), req); err == nil {
				t.Fatal("mixed cancellation was acknowledged as an ordinary failure")
			}
			messages, err := m.Messages(context.Background(), req.RunID, req.WorkerID)
			if err != nil || len(messages) != 1 || !reflect.DeepEqual(messages[0], msg) {
				t.Fatal("rejected acknowledgement changed failure evidence", messages, err)
			}
			if err := m.StopRun(context.Background(), req.RunID); !errors.Is(err, process.ErrCleanup) {
				t.Fatal("mixed cancellation lost its finalization barrier", err)
			}
		})
	}
}

func TestAcknowledgeFailurePreservesEvidenceAndDeduplicates(t *testing.T) {
	m, req, original := failureFixture(t)
	ctx := context.Background()
	if err := m.StopRun(ctx, req.RunID); !errors.Is(err, process.ErrCleanup) {
		t.Fatal("unhandled failure lost its barrier", err)
	}
	ack, err := m.AcknowledgeFailure(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := m.AcknowledgeFailure(ctx, req)
	if err != nil || !reflect.DeepEqual(ack, repeated) {
		t.Fatal("acknowledgement was not stable", repeated, err)
	}
	conflict := req
	conflict.Reason = "different disposition"
	if _, err = m.AcknowledgeFailure(ctx, conflict); err == nil {
		t.Fatal("conflicting disposition accepted")
	}
	messages, err := m.Messages(ctx, req.RunID, req.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].FailureAcknowledgement == nil {
		t.Fatal("missing native acknowledgement receipt")
	}
	messages[0].FailureAcknowledgement = nil
	if !reflect.DeepEqual(messages[0], original) {
		t.Fatal("native failure evidence was rewritten", messages[0], original)
	}
	var events int
	if err = m.Store.db.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type='worker.failure_acknowledged'", req.RunID).Scan(&events); err != nil || events != 1 {
		t.Fatal(events, err)
	}
	if _, err = m.Send(ctx, req.RunID, req.WorkerID, "later", "must not dispatch"); err == nil {
		t.Fatal("acknowledgement reopened worker")
	}
	if err = m.StopRun(ctx, req.RunID); err != nil {
		t.Fatal("settled acknowledged failure blocked finalization", err)
	}
}

func TestFailureAcknowledgementRejectsUnsafeAndStaleRequests(t *testing.T) {
	for _, mode := range []string{"owner", "generation", "message", "empty_evidence", "finalizing", "queued", "needs_attention", "storage", "authentication", "action_integrity", "canceled", "timeout", "prerequisite"} {
		t.Run(mode, func(t *testing.T) {
			m, req, msg := failureFixture(t)
			switch mode {
			case "owner":
				req.Owner += "stale"
			case "generation":
				req.Generation++
			case "message":
				req.MessageID = "other-message"
			case "empty_evidence":
				req.Evidence = ""
			case "finalizing":
				r, _, err := m.Store.Load(context.Background(), req.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if err = m.Store.BeginFinalization(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			case "queued":
				pending := WorkerMessage{ID: "pending", WorkerID: req.WorkerID, RequestID: "pending", Sequence: 2, Status: RunStatusQueued}
				raw, _ := json.Marshal(pending)
				if _, err := m.Store.db.Exec("INSERT INTO messages VALUES(?,?,?,?,?,?)", pending.ID, req.WorkerID, pending.RequestID, 2, pending.Status, raw); err != nil {
					t.Fatal(err)
				}
			default:
				msg.FailureClass = mode
				raw, _ := json.Marshal(msg)
				if _, err := m.Store.db.Exec("UPDATE messages SET record=? WHERE id=?", raw, msg.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.AcknowledgeFailure(context.Background(), req); err == nil {
				t.Fatal("unsafe acknowledgement accepted")
			}
			messages, err := m.Messages(context.Background(), req.RunID, req.WorkerID)
			if err != nil || messages[0].FailureAcknowledgement != nil {
				t.Fatal("failed acknowledgement changed evidence", messages, err)
			}
		})
	}
}

func TestFailureAcknowledgementRequiresSettledInvocationAndToolEffects(t *testing.T) {
	for _, kind := range []string{"invocation", "tool"} {
		t.Run(kind, func(t *testing.T) {
			m, req, msg := failureFixture(t)
			if kind == "invocation" {
				rec := runtimeexec.Record{ID: "native-process", RunID: req.RunID, Owner: req.Owner, Generation: req.Generation, LogicalID: msg.ID, State: "running", Kind: "worker"}
				if err := m.Store.AdmitInvocation(context.Background(), rec); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := m.Store.db.Exec("INSERT INTO tool_calls VALUES(?,?,?,?,?,?,?,?,?)", req.RunID, req.Generation, msg.ID, "write", "external", "write", "pin", "uncertain", []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.AcknowledgeFailure(context.Background(), req); err == nil {
				t.Fatal("unsettled effect acknowledged")
			}
			if kind == "invocation" {
				if _, err := m.Store.db.Exec("UPDATE invocations SET state='cleaned' WHERE run_id=?", req.RunID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := m.Store.db.Exec("UPDATE tool_calls SET status='completed' WHERE run_id=?", req.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.AcknowledgeFailure(context.Background(), req); err != nil {
				t.Fatal("settled effect still blocked", err)
			}
		})
	}
}

type workerJoinContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *workerJoinContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestFailureAcknowledgementWaitsForActualWorkerJoin(t *testing.T) {
	session := &failedNativeSession{fakeSession: fakeSession{started: make(chan string, 1), release: make(chan struct{})}}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(session.release) }) })
	m, run := workerFixture(t, &fakeSession{})
	m.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
		return session, nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := m.Spawn(ctx, WorkerSpec{RunID: run, Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Send(ctx, run, w.ID, "failure", "bounded assignment")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := m.Store.Load(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	req := WorkerFailureRequest{RunID: run, WorkerID: w.ID, MessageID: msg.ID, Generation: r.Generation, Owner: r.Owner, RequestID: "handled-join", Reason: "optional task", Evidence: "original-message:" + msg.ID}
	serveWorkers(t, m)
	select {
	case <-session.started:
	case <-ctx.Done():
		t.Fatal("native turn did not start", ctx.Err())
	}
	m.mu.Lock()
	done := m.done[w.ID]
	m.mu.Unlock()
	if done == nil {
		t.Fatal("active native turn has no production join notification")
	}
	canceled, cancelWait := context.WithCancel(ctx)
	cancelWait()
	if _, err := m.AcknowledgeFailure(canceled, req); !errors.Is(err, context.Canceled) {
		t.Fatal("waiting for the live native turn ignored cancellation", err)
	}
	waiting := &workerJoinContext{Context: ctx, waiting: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := m.AcknowledgeFailure(waiting, req)
		result <- err
	}()
	select {
	case <-waiting.waiting:
	case err := <-result:
		t.Fatal("acknowledgement returned before waiting for the native turn", err)
	case <-ctx.Done():
		t.Fatal("acknowledgement never reached the live join wait", ctx.Err())
	}
	select {
	case err := <-result:
		t.Fatal("acknowledgement returned while the native turn was blocked", err)
	default:
	}
	release.Do(func() { close(session.release) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("joined native failure could not be acknowledged", err)
		}
	case <-ctx.Done():
		t.Fatal("acknowledgement did not complete after the native turn", ctx.Err())
	}
	select {
	case <-done:
	default:
		t.Fatal("acknowledgement preceded the actual native join")
	}
}

func TestFailureAcknowledgementRejectsActiveWorkerAfterJoin(t *testing.T) {
	m, req, _ := failureFixture(t)
	m.mu.Lock()
	original := m.done[req.WorkerID]
	m.mu.Unlock()
	if original != nil {
		select {
		case <-original:
		case <-time.After(5 * time.Second):
			t.Fatal("original failed turn did not join within five seconds")
		}
	}
	done := make(chan struct{})
	close(done)
	m.mu.Lock()
	m.active[req.WorkerID] = func() {}
	m.done[req.WorkerID] = done
	m.mu.Unlock()
	if _, err := m.AcknowledgeFailure(context.Background(), req); err == nil {
		t.Fatal("active worker was ignored after join notification")
	}
	m.mu.Lock()
	delete(m.active, req.WorkerID)
	delete(m.done, req.WorkerID)
	m.mu.Unlock()
	if _, err := m.AcknowledgeFailure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestFailureAcknowledgementRollsBackReceiptFailure(t *testing.T) {
	m, req, _ := failureFixture(t)
	if _, err := m.Store.db.Exec(`CREATE TRIGGER reject_failure_receipt BEFORE INSERT ON events WHEN NEW.type='worker.failure_acknowledged' BEGIN SELECT RAISE(ABORT,'lost receipt checkpoint'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AcknowledgeFailure(context.Background(), req); err == nil {
		t.Fatal("receipt failure was ignored")
	}
	messages, err := m.Messages(context.Background(), req.RunID, req.WorkerID)
	if err != nil || messages[0].FailureAcknowledgement != nil {
		t.Fatal("partial acknowledgement survived rollback", messages, err)
	}
	worker, err := m.Get(context.Background(), req.RunID, req.WorkerID)
	if err != nil || worker.Status == "closed" || worker.FailureAcknowledgement != nil {
		t.Fatal("partial worker closure survived rollback", worker, err)
	}
	if _, err = m.Store.db.Exec("DROP TRIGGER reject_failure_receipt"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.AcknowledgeFailure(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}
