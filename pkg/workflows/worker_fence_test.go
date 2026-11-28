package workflows

import (
	"context"
	"encoding/json"
	"testing"
)

type staleWorkerPublicationSession struct {
	fakeSession
	replaceOwner func()
	emitProgress bool
}

func (s *staleWorkerPublicationSession) RunTurn(_ context.Context, _ SessionRequest, emit func(ProviderEvent)) (SessionResult, error) {
	s.replaceOwner()
	if s.emitProgress {
		emit(ProviderEvent{Type: "session", SessionID: "stale-provider-session"})
	}
	return SessionResult{Status: RunStatusSucceeded, SessionID: "stale-provider-session", Outputs: map[string]any{"text": "stale result"}}, nil
}

func TestOrdinaryWorkerRejectsStaleProgressAndTerminalPublication(t *testing.T) {
	for _, progress := range []bool{false, true} {
		name := "terminal"
		if progress {
			name = "progress-and-terminal"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m, runID := workerFixture(t, &fakeSession{})
			run, _, err := m.Store.Load(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Store.Claim(ctx, run, RunStatusRunning); err != nil {
				t.Fatal(err)
			}
			oldOwner := run.Owner
			worker, err := m.Spawn(ctx, WorkerSpec{RunID: runID, Role: "reviewer", Turns: 2})
			if err != nil || worker.Definition != nil {
				t.Fatal("fixture must cover ordinary workers without a native definition", worker, err)
			}
			message, err := m.Send(ctx, runID, worker.ID, "original", "ordinary turn")
			if err != nil {
				t.Fatal(err)
			}
			var workerBefore, messageBefore []byte
			session := &staleWorkerPublicationSession{emitProgress: progress, replaceOwner: func() {
				if err = m.Store.db.QueryRow("SELECT record FROM workers WHERE id=?", worker.ID).Scan(&workerBefore); err != nil {
					t.Fatal(err)
				}
				if err = m.Store.db.QueryRow("SELECT record FROM messages WHERE id=?", message.ID).Scan(&messageBefore); err != nil {
					t.Fatal(err)
				}
				// A second legitimate claim fences the old provider even within
				// the same generation, before it emits or returns its result.
				if err = m.Store.Claim(ctx, run, RunStatusRunning); err != nil || run.Owner == oldOwner {
					t.Fatal("execution owner was not replaced", run, err)
				}
			}}
			m.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
				return session, nil, nil, nil
			}
			m.turn(ctx, worker, nil)
			for _, check := range []struct {
				query string
				id    string
				want  []byte
			}{{"SELECT record FROM workers WHERE id=?", worker.ID, workerBefore}, {"SELECT record FROM messages WHERE id=?", message.ID, messageBefore}} {
				var got []byte
				if err = m.Store.db.QueryRow(check.query, check.id).Scan(&got); err != nil || string(got) != string(check.want) {
					t.Fatalf("stale provider changed retained publication: %s -> %s (%v)", check.want, got, err)
				}
			}
			var events int
			if err = m.Store.db.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type IN ('worker.progress','worker.turn_completed')", runID).Scan(&events); err != nil || events != 0 {
				t.Fatal("stale progress or terminal event published", events, err)
			}
			var admitted WorkerMessage
			if err = json.Unmarshal(messageBefore, &admitted); err != nil || admitted.Status != RunStatusRunning || admitted.StartedAt.IsZero() {
				t.Fatal("provider boundary was not reached after original admission", admitted, err)
			}
		})
	}
}
