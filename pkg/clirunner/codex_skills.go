package clirunner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// Listing suppression alone does not prevent explicit $skill mentions. Disable
// every discovered path in this process's private CODEX_HOME before any thread
// starts (including resume), then verify the effective configuration.
func isolateCodexSkills(rpc *appRPC, workspace string) error {
	return isolateCodexSelectedSkill(rpc, workspace, "")
}

// A selected occurrence allows only its verified retained entry. Registration
// does not imply admission: the effective list must confirm that exact path.
func isolateCodexSelectedSkill(rpc *appRPC, workspace, selected string) error {
	list := func() ([]string, error) {
		raw, err := rpc.call("skills/list", map[string]any{"cwds": []string{workspace}, "forceReload": true})
		if err != nil {
			return nil, fmt.Errorf("skill isolation: %w", err)
		}
		var response struct {
			Data *[]struct {
				Cwd    string `json:"cwd"`
				Skills *[]struct {
					Path    string `json:"path"`
					Enabled *bool  `json:"enabled"`
				} `json:"skills"`
				Errors *[]json.RawMessage `json:"errors"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.Data == nil || len(*response.Data) != 1 {
			return nil, fmt.Errorf("skill isolation: malformed skills/list response")
		}
		entry := (*response.Data)[0]
		if filepath.Clean(entry.Cwd) != filepath.Clean(workspace) || entry.Skills == nil || entry.Errors == nil || len(*entry.Errors) != 0 {
			return nil, fmt.Errorf("skill isolation: incomplete workspace skill discovery")
		}
		var enabled []string
		for _, skill := range *entry.Skills {
			if skill.Path == "" || skill.Enabled == nil || !filepath.IsAbs(skill.Path) {
				return nil, fmt.Errorf("skill isolation: missing skill path or enabled state")
			}
			if *skill.Enabled {
				enabled = append(enabled, skill.Path)
			}
		}
		return enabled, nil
	}
	paths, err := list()
	if err != nil {
		return err
	}
	for _, path := range paths {
		if path == selected {
			continue
		}
		raw, err := rpc.call("skills/config/write", map[string]any{"path": path, "enabled": false})
		if err != nil {
			return fmt.Errorf("skill isolation: disable skill: %w", err)
		}
		var response struct {
			EffectiveEnabled *bool `json:"effectiveEnabled"`
		}
		if err := json.Unmarshal(raw, &response); err != nil || response.EffectiveEnabled == nil || *response.EffectiveEnabled {
			return fmt.Errorf("skill isolation: provider did not confirm disabled skill")
		}
	}
	remaining, err := list()
	if err != nil {
		return err
	}
	if selected != "" {
		if len(remaining) != 1 || remaining[0] != selected {
			return fmt.Errorf("skill isolation: selected retained skill is missing or unadmitted skills remain enabled")
		}
		return nil
	}
	if len(remaining) != 0 {
		return fmt.Errorf("skill isolation: provider still exposes enabled skills")
	}
	return nil
}
