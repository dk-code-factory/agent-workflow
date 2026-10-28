package workflows

import (
	"context"
	"encoding/json"
	"testing"
)

func appendIterationFixture(t *testing.T) (*SQLiteStore, string, IterationItemResult) {
	t.Helper()
	s, run := executionStore(t)
	ctx := context.Background()
	p := IterationPlan{RunID: run.ID, JobID: "review", Fingerprint: "plan", Items: []IterationPlanItem{{Key: "item", Number: 1}}}
	if err := s.PinIteration(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := IterationItemResult{PlanFingerprint: p.Fingerprint, Key: "item", Number: 1, Attempt: 1, ChildID: iterationChildID(run.ID, "review", "item", 1), Status: "pending"}
	r.Fingerprint = iterationResultHash(r)
	if err := s.AppendIteration(ctx, run.ID, "review", r); err != nil {
		t.Fatal(err)
	}
	return s, run.ID, r
}

func TestAppendIterationConflictingTerminalResultIsRejected(t *testing.T) {
	for _, status := range []string{"exhausted", "provider_failure", "interrupted", "completed"} {
		for _, change := range []string{"payload", "status"} {
			t.Run(status+"/"+change, func(t *testing.T) {
				s, id, first := appendIterationFixture(t)
				ctx := context.Background()
				first.Status = status
				first.Outputs = map[string]any{"revision": "first"}
				first.Fingerprint = iterationResultHash(first)
				if err := s.AppendIteration(ctx, id, "review", first); err != nil {
					t.Fatal(err)
				}
				before, err := s.Events(ctx, id, 0)
				if err != nil {
					t.Fatal(err)
				}
				conflicting := first
				if change == "payload" {
					conflicting.Outputs = map[string]any{"revision": "changed"}
				} else if status == "interrupted" {
					conflicting.Status = "provider_failure"
				} else {
					conflicting.Status = "interrupted"
				}
				conflicting.Fingerprint = iterationResultHash(conflicting)
				if err = s.AppendIteration(ctx, id, "review", conflicting); err == nil {
					t.Error("conflicting terminal result silently accepted")
				}
				records, err := s.IterationAttempts(ctx, id, "review")
				if err != nil || len(records) != 2 || records[1].Fingerprint != first.Fingerprint {
					t.Fatalf("immutable stored result changed: %+v %v", records, err)
				}
				after, err := s.Events(ctx, id, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != len(before) {
					t.Errorf("rejected append must not emit event: before=%d after=%d", len(before), len(after))
				}
				// Each reported result must correspond to a durable immutable observation.
				fingerprints := map[string]bool{}
				for _, r := range records {
					fingerprints[r.Fingerprint] = true
				}
				for _, event := range after {
					if event.Type != "iteration."+conflicting.Status {
						continue
					}
					var observation IterationItemResult
					if err = json.Unmarshal(event.Data, &observation); err != nil {
						t.Fatal(err)
					}
					if !fingerprints[observation.Fingerprint] {
						t.Errorf("event %q reports unpersisted fingerprint %s", event.Type, observation.Fingerprint)
					}
				}
			})
		}
	}
}

func TestAppendIterationRepeatedWaitingPreservesObservations(t *testing.T) {
	s, id, first := appendIterationFixture(t)
	ctx := context.Background()
	first.Status = "waiting"
	first.Outputs = map[string]any{"gate_id": "first"}
	first.Fingerprint = iterationResultHash(first)
	if err := s.AppendIteration(ctx, id, "review", first); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendIteration(ctx, id, "review", first); err != nil {
		t.Fatal("identical waiting observation must remain admissible", err)
	}
	changed := first
	changed.Outputs = map[string]any{"gate_id": "second"}
	changed.Fingerprint = iterationResultHash(changed)
	if err := s.AppendIteration(ctx, id, "review", changed); err != nil {
		t.Fatal("new waiting observation must remain append-only", err)
	}
	records, err := s.IterationAttempts(ctx, id, "review")
	if err != nil || len(records) != 3 || records[1].Fingerprint != first.Fingerprint || records[2].Fingerprint != changed.Fingerprint {
		t.Fatalf("waiting history: %+v %v", records, err)
	}
	events, err := s.Events(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string]bool{first.Fingerprint: true, changed.Fingerprint: true}
	for _, event := range events {
		if event.Type != "iteration.waiting" {
			continue
		}
		var observation IterationItemResult
		if err = json.Unmarshal(event.Data, &observation); err != nil {
			t.Fatal(err)
		}
		if !saved[observation.Fingerprint] {
			t.Fatal("waiting event diverged from retained observations", observation)
		}
	}
}

func TestAppendIterationRecoveredResultIsImmutable(t *testing.T) {
	s, id, r := appendIterationFixture(t)
	ctx := context.Background()
	r.Status = "interrupted"
	r.Fingerprint = iterationResultHash(r)
	if err := s.AppendIteration(ctx, id, "review", r); err != nil {
		t.Fatal(err)
	}
	r.Status = "completed"
	r.Outputs = map[string]any{"revision": "recovered"}
	r.Fingerprint = iterationResultHash(r)
	if err := s.AppendIteration(ctx, id, "review", r); err != nil {
		t.Fatal("initial recovery observation rejected", err)
	}
	original := r.Fingerprint
	before, err := s.Events(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.Outputs = map[string]any{"revision": "conflicting"}
	r.Fingerprint = iterationResultHash(r)
	if err = s.AppendIteration(ctx, id, "review", r); err == nil {
		t.Error("conflicting recovered result silently accepted")
	}
	records, err := s.IterationAttempts(ctx, id, "review")
	if err != nil || len(records) != 3 || records[2].Fingerprint != original {
		t.Fatalf("recovered result changed: %+v %v", records, err)
	}
	after, err := s.Events(ctx, id, 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("failed recovery append emitted event: before=%d after=%d err=%v", len(before), len(after), err)
	}
}
