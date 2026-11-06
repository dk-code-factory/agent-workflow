package workflows

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestLifecycleArgumentFailurePreservesPromptContext(t *testing.T) {
	for _, field := range []string{"prompt", "message"} {
		t.Run(field, func(t *testing.T) {
			source := `jobs:
  task:
    steps:
      - id: seed
        agent: seed
        prompt: Seed
      - id: work
        uses: agent/primary
        with:
          ` + field + `: 'original ${{ secrets.token }}'
          workspace: '${{ steps.seed.text.foo }}'
        onFailure:
          - agent: observer
            prompt: '${{ failed.prompt }}'
        finally:
          - agent: cleanup
            prompt: '${{ failed.prompt }}'
`
			var calls []string
			e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.AgentID)
				if r.AgentID != "seed" && (!strings.HasPrefix(r.Prompt, "original ") || strings.Contains(r.Prompt, "private-token")) {
					t.Errorf("lost or unredacted failed prompt: %q", r.Prompt)
				}
				return map[string]any{"text": "seed"}, nil
			}))
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml", Secrets: map[string]string{"token": "private-token"}})
			if err == nil || !strings.Contains(err.Error(), `cannot access result field "foo" on non-object`) {
				t.Fatalf("original argument failure lost: run=%+v err=%v", run, err)
			}
			if !reflect.DeepEqual(calls, []string{"seed", "observer", "cleanup"}) {
				t.Fatalf("failure context prevented handlers: calls=%v err=%v", calls, err)
			}
		})
	}
}

func TestLifecycleFallbackRecoversArgumentFailure(t *testing.T) {
	for _, contract := range []string{"none", "valid", "invalid"} {
		t.Run(contract, func(t *testing.T) {
			source := `jobs:
  task:
    outputs:
      result: ${{ steps.work.text }}
    steps:
      - id: seed
        agent: seed
        prompt: Seed
      - id: work
        agent: primary
        prompt: original
        with:
          workspace: '${{ steps.seed.text.foo }}'
        onError:
          - agent: backup
            prompt: Recover
`
			if contract != "none" {
				source = strings.Replace(source, "        with:\n", `        with:
          output:
            schema:
              type: object
              required: [ok]
              properties:
                ok: {type: boolean}
`, 1)
			}
			var calls []string
			e, _ := durableExecutor(t, source, agentFunc(func(_ context.Context, r AgentRequest) (map[string]any, error) {
				calls = append(calls, r.AgentID)
				value := map[string]any{"ok": true}
				if contract == "invalid" {
					value = map[string]any{"wrong": true}
				}
				return map[string]any{"text": r.AgentID, "structured": value}, nil
			}))
			run, err := e.Run(context.Background(), RunRequest{Ref: "workflows/main.yml"})
			if contract == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "structured output fallback") {
					t.Fatalf("invalid replacement contract accepted or not checked: run=%+v err=%v", run, err)
				}
			} else if err != nil || run.Jobs["task"].Outputs["result"] != "backup" {
				t.Fatalf("successful fallback did not replace argument failure: run=%+v err=%v", run, err)
			}
			if !reflect.DeepEqual(calls, []string{"seed", "backup"}) {
				t.Fatalf("wrong recovery effects: %v", calls)
			}
			if original := run.Jobs["task"].Steps["work"].Lifecycle.OriginalError; !strings.Contains(original, `cannot access result field "foo" on non-object`) {
				t.Fatalf("original argument failure not retained: %q", original)
			}
		})
	}
}
