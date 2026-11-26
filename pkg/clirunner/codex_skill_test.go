package clirunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func codexSkillFixture(t *testing.T) workflows.SessionRequest {
	t.Helper()
	root := t.TempDir()
	entry := "skills/probe/SKILL.md"
	body := []byte("---\nname: probe\ndescription: Fixture.\n---\nEXACT_NATIVE_SKILL\n")
	if err := os.MkdirAll(filepath.Join(root, "skills/probe"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, entry), body, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return workflows.SessionRequest{AgentRequest: workflows.AgentRequest{SkillRoot: root, Skill: &workflows.SkillInvocation{Version: 1, ID: "original-plan", Binding: workflows.SkillBinding{Version: 1, Name: "probe", Entry: entry, Assets: &workflows.AssetBundle{Entries: []workflows.AssetEntry{{Path: entry, SHA256: hex.EncodeToString(digest[:])}}}}, Model: "gpt-5.4", Profile: codexSkillProfile, ContinuationRequiresEvidence: true}}}
}

func codexSkillFrame(t *testing.T, req workflows.SessionRequest) map[string]any {
	t.Helper()
	path := filepath.Join(req.SkillRoot, req.Skill.Binding.Entry)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"threadId": "original-session", "turnId": "original-turn", "item": map[string]any{"id": "native-item", "type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<skill>\n<name>probe</name>\n<path>" + path + "</path>\n" + string(body) + "\n</skill>"}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "original-turn", "content_item_kinds": []string{"skills.selected_skill_instructions"}, "content_item_metadata": []any{map[string]any{"provenance": map[string]any{"type": "skills"}}}}}}
}

func TestCodexNativeSkillReceiptRejectsReportsAndChangedProvenance(t *testing.T) {
	for _, variant := range []string{"native", "report", "kind", "role", "provenance", "session", "turn", "metadata-turn", "name", "path", "bytes", "retained-bytes"} {
		t.Run(variant, func(t *testing.T) {
			req := codexSkillFixture(t)
			frame := codexSkillFrame(t, req)
			item := frame["item"].(map[string]any)
			metadata := item["internal_chat_message_metadata_passthrough"].(map[string]any)
			content := item["content"].([]any)[0].(map[string]any)
			switch variant {
			case "report":
				delete(item, "internal_chat_message_metadata_passthrough")
			case "kind":
				metadata["content_item_kinds"] = []string{"message"}
			case "role":
				item["role"] = "assistant"
			case "provenance":
				metadata["content_item_metadata"] = []any{map[string]any{"provenance": map[string]any{"type": "model"}}}
			case "session":
				frame["threadId"] = "foreign-session"
			case "turn":
				frame["turnId"] = "foreign-turn"
			case "metadata-turn":
				metadata["turn_id"] = "foreign-turn"
			case "name":
				content["text"] = strings.Replace(content["text"].(string), "<name>probe</name>", "<name>foreign</name>", 1)
			case "path":
				content["text"] = strings.Replace(content["text"].(string), "<path>", "<path>/foreign", 1)
			case "bytes":
				content["text"] = strings.Replace(content["text"].(string), "EXACT_NATIVE_SKILL", "different", 1)
			case "retained-bytes":
				if err := os.WriteFile(filepath.Join(req.SkillRoot, req.Skill.Binding.Entry), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(frame)
			observation, err := codexSkillObservation(req, "original-session", "original-turn", raw)
			if variant == "native" {
				if err != nil || observation == nil || string(observation.Raw) != string(raw) {
					t.Fatalf("original receipt lost: %+v %v", observation, err)
				}
				ledger := &workflows.SkillEvidence{}
				if err = ledger.Observe(req.Skill, *observation); err != nil {
					t.Fatal(err)
				}
				if err = ledger.Continuation(req.Skill, "original-session"); err != nil {
					t.Fatal(err)
				}
				if err = ledger.Observe(req.Skill, *observation); err != nil || len(ledger.Observations) != 1 {
					t.Fatal("original receipt retry was not idempotent", err)
				}
				changed := *observation
				changed.Raw = json.RawMessage(`{"changed":true}`)
				if ledger.Observe(req.Skill, changed) == nil {
					t.Fatal("conflicting repeated native receipt accepted")
				}
				changed = *observation
				changed.Key = "consumption:other"
				changed.BodySHA256 = strings.Repeat("0", 64)
				if ledger.Observe(req.Skill, changed) == nil {
					t.Fatal("different bytes appended to completed native receipt")
				}
			} else if observation != nil {
				t.Fatal("foreign/report-only receipt accepted", observation)
			}
		})
	}
}

func TestCodexSkillCapabilityAdmission(t *testing.T) {
	r := &Runner{Config: Config{AgentRuntime: "native", Agents: map[string]Agent{"native": {Provider: "codex-app-server"}}}}
	req := codexSkillFixture(t)
	for _, variant := range []string{"supported", "arguments", "effort", "model", "profile", "continuation-policy"} {
		t.Run(variant, func(t *testing.T) {
			plan := *req.Skill
			switch variant {
			case "arguments":
				plan.Arguments = "unproved native argument transport"
			case "effort":
				plan.Effort = "high"
			case "model":
				plan.Model = ""
			case "profile":
				plan.Profile = "foreign-profile"
			case "continuation-policy":
				plan.ContinuationRequiresEvidence = false
			}
			admitted, err := r.AdmitSkillTask(&plan)
			if variant == "supported" {
				if err != nil || admitted.Profile != codexSkillProfile || !admitted.ContinuationRequiresEvidence {
					t.Fatal(admitted, err)
				}
			} else if err == nil {
				t.Fatal("unsupported control admitted", admitted)
			}
		})
	}
}
