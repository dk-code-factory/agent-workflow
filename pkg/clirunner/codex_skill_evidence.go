package clirunner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// The pinned native loader produces this user fragment with server-owned
// metadata (rust-v0.167.1 ext/skills/src/fragments.rs). A requested typed input,
// model report, analytics event or historical message is not this receipt.
func codexSkillObservation(req workflows.SessionRequest, session, turn string, raw json.RawMessage) (*workflows.SkillObservation, error) {
	var event struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Item     struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Metadata struct {
				TurnID  string   `json:"turn_id"`
				Kinds   []string `json:"content_item_kinds"`
				Content []struct {
					Provenance struct {
						Type string `json:"type"`
					} `json:"provenance"`
				} `json:"content_item_metadata"`
			} `json:"internal_chat_message_metadata_passthrough"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("malformed native skill event: %w", err)
	}
	selected := false
	for _, kind := range event.Item.Metadata.Kinds {
		if kind == "skills.selected_skill_instructions" {
			selected = true
		}
	}
	if !selected {
		return nil, nil
	}
	item := event.Item
	if req.Skill == nil || event.ThreadID != session || event.TurnID != turn || item.Metadata.TurnID != turn || item.Type != "message" || item.ID == "" || item.Role != "user" || len(item.Content) != 1 || item.Content[0].Type != "input_text" || len(item.Metadata.Kinds) != 1 || len(item.Metadata.Content) != 1 || item.Metadata.Content[0].Provenance.Type != "skills" {
		return nil, fmt.Errorf("native skill event has mismatched identity or provenance")
	}
	entry := filepath.Join(req.SkillRoot, filepath.FromSlash(req.Skill.Binding.Entry))
	body, err := os.ReadFile(entry)
	if err != nil {
		return nil, fmt.Errorf("retained skill evidence bytes unavailable: %w", err)
	}
	digest := sha256.Sum256(body)
	bodyDigest := hex.EncodeToString(digest[:])
	verified := false
	for _, asset := range req.Skill.Binding.Assets.Entries {
		if asset.Path == req.Skill.Binding.Entry && asset.SHA256 == bodyDigest {
			verified = true
		}
	}
	if !verified {
		return nil, fmt.Errorf("retained skill evidence digest changed")
	}
	want := "<skill>\n<name>" + req.Skill.Binding.Name + "</name>\n<path>" + entry + "</path>\n" + string(body) + "\n</skill>"
	if item.Content[0].Text != want {
		return nil, fmt.Errorf("native skill fragment differs from admitted identity or bytes")
	}
	return &workflows.SkillObservation{Key: "consumption:" + item.ID, Kind: "consumption", SessionID: session, TurnID: turn, Name: req.Skill.Binding.Name, Arguments: req.Skill.Arguments, BodySHA256: bodyDigest, Raw: append(json.RawMessage(nil), raw...)}, nil
}
