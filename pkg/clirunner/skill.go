package clirunner

import (
	"fmt"
	"path/filepath"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const claudeSkillProfile = "claude-sdk@0.3.322/local-skill-v1"
const codexSkillProfile = "codex-app-server@0.167.1/local-skill-no-delegation-v1"

// AdmitSkillTask advertises only capabilities established for the pinned
// adapter. This runs before provider discovery, run claims and worker enqueue.
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
		if copy.Profile != "" && copy.Profile != claudeSkillProfile || copy.ContinuationRequiresEvidence {
			return nil, fmt.Errorf("unsupported retained skill capability profile")
		}
		copy.Profile = claudeSkillProfile
	case "codex-app-server":
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
	if req.SkillRoot == "" || !filepath.IsAbs(req.SkillRoot) {
		return nil, fmt.Errorf("skill requires verified retained root")
	}
	entry := filepath.Join(req.SkillRoot, filepath.FromSlash(req.Skill.Binding.Entry))
	return map[string]any{"plan_id": req.Skill.ID, "name": req.Skill.Binding.Name, "entry": entry, "root": req.SkillRoot, "arguments": req.Skill.Arguments, "profile": req.Skill.Profile, "qualified": "aw-" + req.Skill.Binding.Name + ":" + req.Skill.Binding.Name}, nil
}
