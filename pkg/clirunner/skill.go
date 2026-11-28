package clirunner

import (
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const claudeSkillProfile = "claude-sdk@0.3.322/local-skill-v1"
const claudeSkillChildProfile = "claude-sdk@0.3.322/sonnet46-one-read-child-v1"
const codexSkillProfile = "codex-app-server@0.167.1/local-skill-no-delegation-v1"

// AdmitSkillTask advertises only capabilities established for the pinned
// adapter. Initial admission, occurrence binding and retained-profile
// revalidation call it before the selected step's claim and worker enqueue.
func (r *Runner) AdmitSkillTask(plan *workflows.SkillInvocation) (*workflows.SkillInvocation, error) {
	role, err := r.nativeTaskRuntime()
	if err != nil {
		return nil, err
	}
	if plan == nil || plan.Version != workflows.SkillAdmissionVersion || plan.Binding.Version != workflows.SkillAdmissionVersion || plan.Binding.Assets == nil {
		return nil, fmt.Errorf("invalid admitted skill contract")
	}
	if role.Execution.Mode == "container" {
		return nil, fmt.Errorf("supplied skill container projection is not certified")
	}
	if plan.Model == "" {
		return nil, fmt.Errorf("skill tasks require a pinned model")
	}
	if plan.Effort != "" {
		return nil, fmt.Errorf("skill reasoning-effort honoring is not certified")
	}
	copy := *plan
	switch role.Provider {
	case "claude-sdk":
		if err := verifySDKBridge(role); err != nil {
			return nil, err
		}
		if len(role.MCPGrants) != 0 {
			return nil, fmt.Errorf("selected skill broker authority is not certified")
		}
		for _, tool := range role.Tools {
			if tool != "Read" {
				return nil, fmt.Errorf("selected Claude skill native tools exceed certified Read authority")
			}
		}
		if len(plan.Children) != 0 {
			return pinSkillChildProfile(plan, role)
		}
		if plan.ResolvedEffort != "" || len(plan.EffectiveTools) != 0 {
			return nil, fmt.Errorf("unsupported retained skill resolved controls")
		}
		if copy.Profile != "" && copy.Profile != claudeSkillProfile || copy.ContinuationRequiresEvidence {
			return nil, fmt.Errorf("unsupported retained skill capability profile")
		}
		copy.Profile = claudeSkillProfile
	case "codex-app-server":
		if len(plan.Children) != 0 || plan.ResolvedEffort != "" || len(plan.EffectiveTools) != 0 || role.Sandbox != "" && role.Sandbox != "read-only" {
			return nil, fmt.Errorf("unsupported Codex selected skill child/permission profile")
		}
		if plan.Arguments != "" {
			return nil, fmt.Errorf("Codex typed skill arguments are not certified")
		}
		if copy.Profile != "" && (copy.Profile != codexSkillProfile || !copy.ContinuationRequiresEvidence) {
			return nil, fmt.Errorf("unsupported retained skill capability profile")
		}
		copy.Profile = codexSkillProfile
		copy.ContinuationRequiresEvidence = true
	default:
		return nil, fmt.Errorf("unsupported supplied-skill provider")
	}
	return &copy, nil
}

func skillBridgeInput(req workflows.SessionRequest) (map[string]any, error) {
	if req.Skill == nil {
		return nil, nil
	}
	if req.Skill.Workspace != "" && req.Workspace != req.Skill.Workspace {
		return nil, fmt.Errorf("selected skill workspace differs from admission")
	}
	if req.SkillRoot == "" || !filepath.IsAbs(req.SkillRoot) {
		return nil, fmt.Errorf("skill requires verified retained root")
	}
	entry := filepath.Join(req.SkillRoot, filepath.FromSlash(req.Skill.Binding.Entry))
	input := map[string]any{"plan_id": req.Skill.ID, "name": req.Skill.Binding.Name, "entry": entry, "root": req.SkillRoot, "arguments": req.Skill.Arguments, "profile": req.Skill.Profile, "qualified": "aw-" + req.Skill.Binding.Name + ":" + req.Skill.Binding.Name}
	if len(req.Skill.Children) != 0 {
		if req.Skill.Profile != claudeSkillChildProfile || len(req.Skill.Children) != 1 || req.AuthorizeSkillChild == nil {
			return nil, fmt.Errorf("skill child lacks certified profile or durable authorization")
		}
		child := req.Skill.Children[0]
		resources := []string{}
		for _, name := range child.Resources {
			resources = append(resources, filepath.Join(filepath.Dir(entry), filepath.FromSlash(name)))
		}
		input["child"] = map[string]any{"name": child.Name, "type": child.Type, "prompt": child.Prompt, "description": child.Description, "model": child.Model, "effort": child.Effort, "max_turns": child.MaxTurns, "max_calls": child.MaxCalls, "background": child.Background, "resources": resources}
		input["child_complete"] = req.SkillEvidence != nil && len(req.SkillEvidence.Children) > 0 && req.SkillEvidence.Complete(req.Skill) == nil
	}
	return input, nil
}

// Exact native controls established with the locked SDK and a local gateway.
// The profile never expands the workspace role grant.
func pinSkillChildProfile(plan *workflows.SkillInvocation, role Agent) (*workflows.SkillInvocation, error) {
	if plan.Model != "claude-sonnet-4-6" || plan.Effort != "" || plan.ResolvedEffort != "" && plan.ResolvedEffort != "high" || len(plan.Children) != 1 || len(role.MCPGrants) != 0 || !slices.Contains(role.Tools, "Read") {
		return nil, fmt.Errorf("unsupported native skill child authority/model/effort")
	}
	for _, tool := range role.Tools {
		if tool != "Read" {
			return nil, fmt.Errorf("native skill child requires certified parent Read ceiling")
		}
	}
	copy := *plan
	copy.Children = append([]workflows.SkillChild(nil), plan.Children...)
	child := &copy.Children[0]
	if !regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`).MatchString(child.Name) || len(child.Name) > 48 || child.Type != "aw-child-"+child.Name || child.Prompt == "" || child.Description == "" || len(child.Description) > 512 || len(child.Resources) == 0 || child.Model != plan.Model || child.Effort != "" && child.Effort != "high" || child.MaxTurns != 2 || child.MaxCalls != 1 || child.Background || !slices.Equal(child.Tools, []string{"Read"}) {
		return nil, fmt.Errorf("unsupported retained native child controls")
	}
	for _, name := range child.Resources {
		if filepath.IsAbs(name) || name == "." || name == ".." || strings.HasPrefix(name, "../") || filepath.ToSlash(filepath.Clean(name)) != name {
			return nil, fmt.Errorf("invalid retained child resource path")
		}
	}
	copy.Profile = claudeSkillChildProfile
	copy.ContinuationRequiresEvidence = true
	copy.ResolvedEffort = "high"
	copy.EffectiveTools = append([]string(nil), role.Tools...)
	slices.Sort(copy.EffectiveTools)
	copy.EffectiveTools = slices.Compact(copy.EffectiveTools)
	if len(plan.EffectiveTools) != 0 && !slices.Equal(plan.EffectiveTools, copy.EffectiveTools) {
		return nil, fmt.Errorf("retained native child effective authority changed")
	}
	child.Effort = "high"
	if plan.Profile != "" && !reflect.DeepEqual(plan, &copy) {
		return nil, fmt.Errorf("retained native child capability controls changed")
	}
	return &copy, nil
}
