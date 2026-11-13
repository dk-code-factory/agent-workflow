package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNativeTaskRejectsEscapedCredentialsBeforeDispatch(t *testing.T) {
	for _, secret := range []string{"private&token", "private<token>", "private\"token", "private\\token", "private\ntoken"} {
		for _, target := range []string{"prompt", "input"} {
			t.Run(fmt.Sprintf("%q/%s", secret, target), func(t *testing.T) {
				prompt, input := "review", "{ok: true}"
				if target == "prompt" {
					prompt = "${{ secrets.token }}"
				} else {
					input = "{credential: '${{ secrets.token }}'}"
				}
				source := fmt.Sprintf("jobs:\n  review:\n    steps:\n      - uses: agent/run\n        with:\n          agent: reviewer.toml\n          prompt: '%s'\n          input: %s\n", prompt, input)
				executor, store, fake := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) {
					return map[string]any{"text": "unexpected dispatch"}, nil
				})
				run, err := executor.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": secret}})
				if err == nil || !strings.Contains(err.Error(), "credential references") {
					t.Fatalf("credential was not rejected: %v", err)
				}
				attempts, err := store.Attempts(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, attempt := range attempts {
					if attempt.AgentTask != nil {
						t.Fatal("credential-bearing dispatch persisted")
					}
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if len(fake.requests) != 0 {
					t.Fatal("credential reached provider")
				}
			})
		}
	}
}

func TestMailboxRejectsEscapedCredentialValues(t *testing.T) {
	manager, run := workerFixture(t, &fakeSession{})
	original := manager.Factory
	const secret = "private&token"
	manager.Factory = func(ctx context.Context, run, role string, tools []string) (SessionRunner, []string, map[string]string, error) {
		runner, grants, _, err := original(ctx, run, role, tools)
		return runner, grants, map[string]string{"token": secret}, err
	}
	worker, err := manager.Spawn(context.Background(), WorkerSpec{RunID: run, Role: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []AgentRequest{{Prompt: secret}, {Prompt: "review", Input: map[string]any{secret: "value"}}, {Prompt: "review", Input: map[string]any{"nested": []any{secret}}}} {
		if _, err := manager.sendTask(context.Background(), run, worker.ID, "escaped", task); err == nil || !strings.Contains(err.Error(), "credential references") {
			t.Fatal("escaped credential accepted", err)
		}
	}
	messages, err := manager.Messages(context.Background(), run, worker.ID)
	if err != nil || len(messages) != 0 {
		t.Fatal("rejected mailbox input retained", messages, err)
	}
	if _, err := manager.sendTask(context.Background(), run, worker.ID, "benign", AgentRequest{Prompt: "safe & quoted input", Input: map[string]any{"exact": json.Number("9007199254740993")}}); err != nil {
		t.Fatal("benign escaped input rejected", err)
	}
}

func TestNativeTaskBoundsEncodedInputBeforeRetainingDispatch(t *testing.T) {
	executor, store, fake := agentTaskFixture(t, `on: {workflow_call: {inputs: {payload: {type: string}}}}
jobs:
  review:
    steps:
      - uses: agent/run
        with: {agent: reviewer.toml, prompt: x, input: '${{ inputs.payload }}'}
`, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "unexpected"}, nil })
	run, err := executor.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Inputs: map[string]any{"payload": strings.Repeat("&", (1<<20)/2)}})
	// Each ampersand takes six bytes in the actual JSON prompt.
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatal("oversized encoded input accepted", err)
	}
	attempts, err := store.Attempts(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.AgentTask != nil {
			t.Fatal("oversized dispatch persisted")
		}
	}
	workers, err := executor.Workers.List(context.Background(), run.ID)
	if err != nil || len(workers) != 0 {
		t.Fatal("oversized input created worker", workers, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Fatal("oversized input reached provider")
	}
}

func TestNativeTaskPromptCountsExactEncodedBoundary(t *testing.T) {
	prefix, err := AgentTaskPrompt("x", "")
	if err != nil {
		t.Fatal(err)
	}
	exact := strings.Repeat("a", (1<<20)-len(prefix))
	prompt, err := AgentTaskPrompt("x", exact)
	if err != nil || len(prompt) != 1<<20 {
		t.Fatal("exact encoded boundary rejected", len(prompt), err)
	}
	if _, err = AgentTaskPrompt("x", exact+"a"); err == nil {
		t.Fatal("over-bound input accepted")
	}
}

func TestNativeTaskTurnAllowanceCannotBeExtended(t *testing.T) {
	for _, followup := range []string{"", ", turns: 3"} {
		t.Run(followup, func(t *testing.T) {
			source := fmt.Sprintf(`jobs:
  review:
    steps:
      - id: first
        uses: agent/run
        with: {agent: reviewer.toml, prompt: first, turns: 1}
      - id: second
        uses: agent/run
        with: {agent: '${{ steps.first.outputs.agent }}', prompt: second%s}
`, followup)
			executor, _, fake := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "done"}, nil })
			run, err := executor.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil {
				t.Fatal("exhausted or extended task allowance accepted")
			}
			workers, loadErr := executor.Workers.List(context.Background(), run.ID)
			if loadErr != nil || len(workers) != 1 || workers[0].TaskTurns != 1 || workers[0].Remaining != 0 {
				t.Fatal("original allowance changed", workers, loadErr)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != 1 {
				t.Fatal("followup extended original allowance", len(fake.requests))
			}
		})
	}
}

func TestNativeTaskTurnAllowanceRejectsInvalidAndExcessiveLimits(t *testing.T) {
	for _, turns := range []string{"0", "-1", "1.5", "'5'", "999", "2147483648"} {
		t.Run(turns, func(t *testing.T) {
			source := fmt.Sprintf("jobs: {review: {steps: [{uses: agent/run, with: {agent: reviewer.toml, prompt: review, turns: %s}}]}}", turns)
			executor, _, fake := agentTaskFixture(t, source, func(SessionRequest) (map[string]any, error) { return map[string]any{"text": "unexpected"}, nil })
			_, err := executor.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if err == nil {
				t.Fatal("invalid turn allowance accepted")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != 0 {
				t.Fatal("invalid allowance reached provider")
			}
		})
	}
}
