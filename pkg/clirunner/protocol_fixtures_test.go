package clirunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestPinnedCodexProtocolFixtures(t *testing.T) {
	root := filepath.Join("testdata", "codex", "0.164.0")
	data, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Files   map[string]string `json:"files"`
		Managed bool              `json:"managed_policy_certified"`
		Live    bool              `json:"live_provider_verified"`
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Managed || manifest.Live {
		t.Fatal("schema generation is not provider or managed-policy certification")
	}
	for name, fingerprint := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != fingerprint {
			t.Fatalf("upstream schema fingerprint changed: %s", name)
		}
	}
	// The pinned CLI emits draft-07 schemas. These selected wire envelopes use
	// the shared JSON Schema keywords; validate them as 2020-12 after removing
	// only the meta-schema declaration, retaining all upstream field constraints.
	validate := func(name string, value any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err = json.Unmarshal(data, &schema); err != nil {
			t.Fatal(err)
		}
		schema.Schema = ""
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = resolved.Validate(value); err != nil {
			t.Fatalf("wire envelope violates pinned upstream %s: %v", name, err)
		}
	}
	validate("v1/InitializeParams.json", map[string]any{"clientInfo": map[string]any{"name": "agent-workflow", "version": "fixture"}, "capabilities": map[string]any{"experimentalApi": true}})
	validate("v2/ThreadStartParams.json", map[string]any{"cwd": "/workspace", "approvalPolicy": "never", "sandbox": "read-only", "baseInstructions": "fixture", "dynamicTools": []any{map[string]any{"type": "function", "name": "broker_fixture", "description": "Fixture", "inputSchema": map[string]any{"type": "object"}, "deferLoading": false}}})
	validate("v2/TurnInterruptParams.json", map[string]any{"threadId": "thread", "turnId": "turn"})
	validate("DynamicToolCallParams.json", map[string]any{"threadId": "thread", "turnId": "turn", "callId": "upstream-id", "tool": "broker_fixture", "arguments": map[string]any{}})
	validate("DynamicToolCallResponse.json", map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": "fixture"}}, "success": true})
}
