package workflows

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// SkillChildSelection is an explicit application request, never skill metadata.
// The first supported profile deliberately has one synchronous, read-only child.
type SkillChildSelection struct {
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Resources   []string `json:"resources"`
	Model       string   `json:"model"`
	MaxTurns    int      `json:"max-turns"`
	MaxCalls    int      `json:"max-calls"`
	Background  *bool    `json:"background"`
}

// SkillChild pins resolved native controls in the invocation identity. Declared
// parent effort remains separate from the profile's resolved native effort.
type SkillChild struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Prompt      string   `json:"prompt"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Resources   []string `json:"resources"`
	Model       string   `json:"model"`
	Effort      string   `json:"effort"`
	MaxTurns    int      `json:"max_turns"`
	MaxCalls    int      `json:"max_calls"`
	Background  bool     `json:"background"`
}

func admitSkillChildren(plan *SkillInvocation, selection *SkillSelection, ec expressionContext) error {
	if len(selection.Children) == 0 {
		return nil
	}
	if len(selection.Children) != 1 || len(selection.Evidence.Children) != 1 || selection.Evidence.Children[0] != selection.Children[0].Name {
		return fmt.Errorf("skill requires exactly one declared and required child")
	}
	child := selection.Children[0]
	if !skillNamePattern.MatchString(child.Name) || len(child.Name) > 48 || child.Background == nil || *child.Background || child.MaxTurns != 2 || child.MaxCalls != 1 || child.Model != "inherit" || !slices.Equal(child.Tools, []string{"Read"}) || len(child.Resources) == 0 || len(child.Resources) > 32 {
		return fmt.Errorf("unsupported skill child controls: require one foreground Read child, inherited model, max-turns 2 and max-calls 1")
	}
	prompt, err := admitSkillText(child.Prompt, ec)
	if err != nil {
		return err
	}
	description, err := admitSkillText(child.Description, ec)
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(description) == "" || len(description) > 512 {
		return fmt.Errorf("skill child requires bounded pinned prompt and description")
	}
	resources := []string{}
	for _, requested := range child.Resources {
		name, err := assetPath(requested)
		if err != nil || name == "." || strings.Contains(requested, "${{") {
			return fmt.Errorf("skill child resources must be literal retained files")
		}
		allowed := false
		for _, parent := range plan.Resources {
			if name == parent || strings.HasPrefix(name, parent+"/") {
				allowed = true
			}
		}
		if !allowed {
			return fmt.Errorf("skill child resource %s exceeds parent resources", name)
		}
		found := false
		for _, entry := range plan.Binding.Assets.Entries {
			if entry.Path == path.Join(path.Dir(plan.Binding.Entry), name) && entry.Kind == "file" {
				found = true
			}
		}
		if !found || slices.Contains(resources, name) {
			return fmt.Errorf("skill child resource %s is missing or repeated", name)
		}
		resources = append(resources, name)
	}
	plan.Children = []SkillChild{{Name: child.Name, Type: "aw-child-" + child.Name, Prompt: prompt, Description: description, Tools: []string{"Read"}, Resources: resources, Model: plan.Model, MaxTurns: 2, MaxCalls: 1}}
	return nil
}
