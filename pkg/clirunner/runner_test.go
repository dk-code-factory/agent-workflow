package clirunner

import (
	"strings"
	"testing"
)

func TestCLIOutputProtocols(t *testing.T) {
	claude, err := parseClaude([]byte(`{"type":"result","subtype":"success","result":"hello","usage":{"input_tokens":3}}`))
	if err != nil || claude["text"] != "hello" {
		t.Fatalf("Claude: %+v %v", claude, err)
	}
	for _, raw := range []string{`{"type":"result","is_error":true,"result":"failed"}`, `{"type":"result","subtype":"error_max_turns","result":"partial"}`, `{"result":"no terminal type"}`, `not json`} {
		if _, err := parseClaude([]byte(raw)); err == nil {
			t.Errorf("accepted Claude failure: %s", raw)
		}
	}
	codex, err := parseCodex([]byte("{\"type\":\"thread.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"hello\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":3}}\n"))
	if err != nil || codex["text"] != "hello" {
		t.Fatalf("Codex: %+v %v", codex, err)
	}
	for _, raw := range []string{`{"type":"turn.failed","error":{"message":"failed"}}`, `{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}`, `{"type":"turn.completed"}`, "broken JSON"} {
		if _, err := parseCodex([]byte(raw)); err == nil {
			t.Errorf("accepted Codex failure: %s", raw)
		}
	}
}

func TestBoundedBuffer(t *testing.T) {
	var b boundedBuffer
	data := []byte(strings.Repeat("x", maxOutput+1))
	n, err := b.Write(data)
	if err != nil || n != len(data) || b.Len() != maxOutput || !b.overflow {
		t.Fatalf("bad output limit: %d %d %v", n, b.Len(), err)
	}
}

func TestClaudeNativeStructuredOutputPreservesText(t *testing.T) {
	out, err := parseClaude([]byte(`{"type":"result","subtype":"success","result":"Attached","structured_output":{"ok":true}}`))
	if err != nil || out["text"] != "Attached" || out["response"] != "Attached" || out["structured"].(map[string]any)["ok"] != true {
		t.Fatal(out, err)
	}
}
