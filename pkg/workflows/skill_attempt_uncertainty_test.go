package workflows

import (
	"encoding/json"
	"testing"
)

func TestSkillNativeAttemptErrorCannotBorrowLaterCompletion(t *testing.T) {
	for _, kind := range []string{"api_retry", "subagent_retry", "assistant_error", "codex_retry"} {
		t.Run(kind, func(t *testing.T) {
			plan, ledger, f := childLedgerFixture(t, "selected-child")
			for _, o := range f.Observations {
				if err := ledger.ObserveChild(plan, o); err != nil {
					t.Fatal(err)
				}
			}
			if err := ledger.Complete(plan); err != nil {
				t.Fatal(err)
			}
			raw := map[string]any{"session_id": f.Session, "type": "system", "subtype": "api_retry", "attempt": 1}
			provider, turn := "claude-sdk", ""
			switch kind {
			case "subagent_retry":
				raw = map[string]any{"session_id": f.Session, "type": "tool_progress", "subagent_retry": map[string]any{"agent_id": "original-child", "attempt": 1}}
			case "assistant_error":
				raw = map[string]any{"session_id": f.Session, "type": "assistant", "error": "api_error"}
			case "codex_retry":
				provider, turn = "codex-app-server", "original-turn"
				raw = map[string]any{"method": "error", "params": map[string]any{"threadId": f.Session, "turnId": turn, "willRetry": true, "error": map[string]any{"message": "Reconnecting"}}}
			}
			body, _ := json.Marshal(raw)
			o := SkillAttemptUncertainty{Key: "original-attempt-error", Provider: provider, SessionID: f.Session, TurnID: turn, Raw: body}
			if err := ledger.ObserveUncertainAttempt(plan, o); err != nil {
				t.Fatal(err)
			}
			if ledger.Complete(plan) == nil || ledger.Continuation(plan, f.Session) == nil {
				t.Fatal("later complete native evidence settled unknown original attempt")
			}
			if err := ledger.ObserveUncertainAttempt(plan, o); err != nil || len(ledger.UncertainAttempts) != 1 {
				t.Fatal("exact original replay changed ledger", err)
			}
			o.Raw = json.RawMessage(`{"session_id":"foreign","type":"system","subtype":"api_retry"}`)
			if ledger.ObserveUncertainAttempt(plan, o) == nil {
				t.Fatal("foreign original error accepted")
			}
			o.Raw = json.RawMessage(`{"session_id":"` + f.Session + `","type":"assistant","message":{"content":[{"type":"text","text":"api_retry"}]}}`)
			if ledger.ObserveUncertainAttempt(plan, o) == nil {
				t.Fatal("model report accepted as original attempt error")
			}
		})
	}
}
