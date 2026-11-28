package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
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

type reloadedPolicyFixture struct {
	*nativePolicyFixture
	afterValidation func()
}

func (f *reloadedPolicyFixture) AdmitNativeTask(definition *AgentDefinition) (*AgentDefinition, error) {
	admitted, err := f.nativePolicyFixture.AdmitNativeTask(definition)
	if err == nil {
		f.afterValidation()
	}
	return admitted, err
}

func TestWorkerPolicyReloadPreservesValidatedAuthority(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("recovering=%v/changed=%v", recovering, changed), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				manager, runID := workerFixture(t, &fakeSession{})
				run, _, err := manager.Store.Load(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				if err = manager.Store.Claim(ctx, run, RunStatusRunning); err != nil {
					t.Fatal(err)
				}
				worker, err := manager.Spawn(ctx, WorkerSpec{RunID: runID, Role: "reviewer", Turns: 2})
				if err != nil {
					t.Fatal(err)
				}
				message, err := manager.Send(ctx, runID, worker.ID, "original-policy", "validate the admitted input")
				if err != nil {
					t.Fatal(err)
				}
				worker.Definition = &AgentDefinition{ToolPolicy: "read-only", NativePolicy: &NativeTaskPolicy{Mode: "read-only", Provider: "fixture", Profile: "native-read-only-fixture-v1", NativeTools: []string{"Read"}}}
				worker.Tools = []string{"Read"}
				worker.ToolGrants = nil
				if recovering {
					worker.Status = RunStatusRunning
					worker.Remaining--
					message.Status = RunStatusRunning
					message.StartedAt = time.Now().UTC()
					message.Generation = run.Generation
					admission := &managedCodexAdmission{WorkerID: worker.ID, MessageID: message.ID, StartedAt: message.StartedAt, BoundarySHA256: managedBoundaryDigest(worker, message), RemainingCeiling: worker.Remaining}
					ctx = context.WithValue(ctx, managedCodexResumeKey{}, managedCodexResumeContext{message: message, recovery: &managedCodexRecovery{Admission: admission}})
				}
				save := func(value any, table, id string) []byte {
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					status := worker.Status
					if table == "messages" {
						status = message.Status
					}
					if _, err = manager.Store.db.ExecContext(ctx, "UPDATE "+table+" SET status=?,record=? WHERE id=?", status, raw, id); err != nil {
						t.Fatal(err)
					}
					return raw
				}
				workerBefore := save(worker, "workers", worker.ID)
				messageBefore := save(message, "messages", message.ID)
				validations := 0
				fixture := &reloadedPolicyFixture{nativePolicyFixture: &nativePolicyFixture{nativeTaskFixture: &nativeTaskFixture{respond: func(SessionRequest) (map[string]any, error) {
					return map[string]any{"text": "validated original policy"}, nil
				}}}}
				fixture.afterValidation = func() {
					validations++
					if changed {
						concurrent := worker
						// Skill is outside managedBoundaryDigest. The policy reload
						// must still reject newly introduced authority before usage,
						// assets or provider work can be admitted.
						concurrent.Skill = &SkillInvocation{Version: 1, ID: "unvalidated-selection"}
						workerBefore = save(concurrent, "workers", worker.ID)
					}
				}
				manager.Factory = func(context.Context, string, string, []string) (SessionRunner, []string, map[string]string, error) {
					return fixture, nil, nil, nil
				}
				manager.turn(ctx, worker, nil)
				if validations != 1 {
					t.Fatal("original runtime policy was not validated", validations)
				}
				if !changed {
					if len(fixture.requests) != 1 {
						t.Fatal("unchanged policy did not reach provider", fixture.requests)
					}
					messages, err := manager.Messages(ctx, runID, worker.ID)
					if err != nil || len(messages) != 1 || messages[0].Status != RunStatusSucceeded {
						t.Fatal("unchanged original did not complete", messages, err)
					}
					return
				}
				if len(fixture.requests) != 0 {
					t.Fatal("reloaded unvalidated policy reached provider", fixture.requests)
				}
				for _, check := range []struct {
					table, id string
					want      []byte
				}{{"workers", worker.ID, workerBefore}, {"messages", message.ID, messageBefore}} {
					var raw []byte
					if err := manager.Store.db.QueryRowContext(ctx, "SELECT record FROM "+check.table+" WHERE id=?", check.id).Scan(&raw); err != nil || string(raw) != string(check.want) {
						t.Fatal("rejected policy changed original admission", check.table, string(raw), err)
					}
				}
				var usage int
				if err := manager.Store.db.QueryRowContext(ctx, "SELECT count(*) FROM usage_invocations WHERE id=?", message.ID).Scan(&usage); err != nil || usage != 0 {
					t.Fatal("rejected policy admitted usage", usage, err)
				}
			})
		}
	}
}
