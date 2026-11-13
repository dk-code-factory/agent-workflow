package workflows

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestLifecycleRedactsCandidateAndVerificationSnapshots(t *testing.T) {
	const secret = "private-lifecycle-snapshot-token"
	original := StepExecution{Lifecycle: &StepLifecycle{
		Candidate:     map[string]any{"stdout": secret},
		Verification:  map[string]any{"feedback": secret},
		Verifications: []any{map[string]any{"feedback": []any{secret}}},
	}}
	safe := redactStep(original, map[string]string{"token": secret})
	encoded, err := json.Marshal(safe)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("lifecycle snapshot retained secret: %s (%v)", encoded, err)
	}
	if original.Lifecycle.Candidate["stdout"] != secret || original.Lifecycle.Verification.(map[string]any)["feedback"] != secret || original.Lifecycle.Verifications[0].(map[string]any)["feedback"].([]any)[0] != secret {
		t.Fatal("redaction mutated execution snapshots")
	}
}

func TestCommandLifecycleCandidateIsRedactedInRunAndAttempts(t *testing.T) {
	const secret = "private-command-candidate-token"
	e, store := durableExecutor(t, `jobs:
  task:
    steps:
      - id: command
        argv: [printf, '%s', '${{ secrets.token }}']
        finally:
          - argv: [true]
`, nil)
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": secret}})
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.Load(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := store.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{run, stored, attempts} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), secret) {
			t.Fatalf("lifecycle persisted secret: %s (%v)", encoded, err)
		}
	}
	if run.Jobs["task"].Steps["command"].Lifecycle.Candidate["stdout"] != "[REDACTED]" {
		t.Fatal("candidate receipt missing redacted output")
	}
}

func TestCommandErrorHookEventRedactsSecretsBeforeNativeRecovery(t *testing.T) {
	const secret = "nonexistent-private-command-token"
	called := false
	e, store, _ := agentTaskFixture(t, `jobs:
  task:
    steps:
      - id: command
        argv: ['${{ secrets.token }}']
        onError:
          - uses: agent/run
            with: {agent: reviewer.toml, prompt: recover}
`, func(req SessionRequest) (map[string]any, error) {
		called = true
		event, ok := req.Input.(map[string]any)
		if !ok {
			t.Errorf("missing hook event: %T", req.Input)
			return nil, nil
		}
		failure, ok := event["error"].(map[string]any)
		if !ok || !strings.Contains(nativeString(failure, "message"), "[REDACTED]") {
			t.Errorf("missing redacted error: %+v", event)
		}
		data, err := json.Marshal(event)
		if err != nil || strings.Contains(string(data), secret) {
			t.Errorf("hook received secret: %s (%v)", data, err)
		}
		return map[string]any{"text": "recovered"}, nil
	})
	run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": secret}})
	if err != nil || !called {
		t.Fatalf("native recovery did not run: called=%v error=%v", called, err)
	}
	if run.Jobs["task"].Steps["command"].Outputs["text"] != "recovered" {
		t.Fatal("replacement missing")
	}
	attempts, err := store.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(attempts)
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatalf("hook dispatch retained secret: %s (%v)", data, err)
	}
}
