package workflows

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var idPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

func Validate(w *Workflow) error {
	if w == nil || len(w.Jobs) == 0 {
		return fmt.Errorf("at least one job is required")
	}
	if len(w.Gates) != 0 {
		return fmt.Errorf("gates require PicoClaw and are unsupported")
	}
	if len(w.On.Schedule) != 0 || w.On.ChannelMessage != nil || w.On.Command != nil || w.On.RuntimeEvent != nil || w.On.Event != nil {
		return fmt.Errorf("standalone runner supports manual and workflow_call triggers only")
	}
	if w.On.WorkflowCall != nil {
		for id, input := range w.On.WorkflowCall.Inputs {
			switch input.Type {
			case "", "string", "boolean", "number", "object", "array":
			default:
				return fmt.Errorf("input %s: unsupported type %q", id, input.Type)
			}
		}
	}
	for id, job := range w.Jobs {
		if !idPattern.MatchString(id) {
			return fmt.Errorf("invalid job id %q", id)
		}
		if job.RunsOn != "" && job.RunsOn != "local" {
			return fmt.Errorf("job %s: only runs-on: local is supported", id)
		}
		if job.Repeat != nil && (job.Repeat.MaxAttempts < 1 || job.Repeat.MaxAttempts > 100 || strings.TrimSpace(job.Repeat.Until) == "") {
			return fmt.Errorf("job %s: repeat requires max-attempts between 1 and 100 and until", id)
		}
		if job.Context != (RunContext{}) {
			return fmt.Errorf("job %s: session/delivery context requires PicoClaw", id)
		}
		if job.Uses != "" {
			if _, err := CanonicalLocalRef(job.Uses); err != nil {
				return err
			}
			if len(job.Steps) != 0 {
				return fmt.Errorf("job %s cannot have both uses and steps", id)
			}
		} else {
			if len(job.Steps) == 0 {
				return fmt.Errorf("job %s requires steps", id)
			}
			if len(job.With) != 0 || job.Secrets != nil {
				return fmt.Errorf("job %s: with/secrets require a reusable workflow", id)
			}
		}
		seen := map[string]bool{}
		for i, step := range job.Steps {
			sid := stepID(step, i)
			if !idPattern.MatchString(sid) || seen[sid] {
				return fmt.Errorf("job %s: invalid or duplicate step id %q", id, sid)
			}
			seen[sid] = true
			command := len(step.Argv) > 0 || step.Run != ""
			if command {
				if step.Uses != "" || len(step.With) > 0 {
					return fmt.Errorf("step %s: commands cannot have uses or with", sid)
				}
				if step.Run != "" && (step.Shell == "" || len(step.Argv) > 0) {
					return fmt.Errorf("step %s: run requires an explicit shell and cannot have argv", sid)
				}
				if step.Run == "" && step.Shell != "" {
					return fmt.Errorf("step %s: shell requires run", sid)
				}
				if step.Context != (RunContext{}) {
					return fmt.Errorf("step %s: command context is unsupported", sid)
				}
				if step.Timeout != "" {
					d, err := time.ParseDuration(step.Timeout)
					if err != nil || d <= 0 {
						return fmt.Errorf("step %s: invalid timeout", sid)
					}
				}
				continue
			}
			if step.Shell != "" || step.Timeout != "" || step.WorkingDirectory != "" {
				return fmt.Errorf("step %s: command fields require argv or run", sid)
			}
			if isApplicationAction(step.Uses) {
				if !idPattern.MatchString(strings.TrimPrefix(step.Uses, "action/")) {
					return fmt.Errorf("step %s: invalid registered action name", sid)
				}
				if step.Context != (RunContext{}) {
					return fmt.Errorf("step %s: registered actions cannot use session context", sid)
				}
				continue
			}
			if IsControllerAction(step.Uses) {
				allowed := map[string]bool{}
				switch step.Uses {
				case "workspace/create":
					allowed = map[string]bool{"commit": true, "independent": true}
				case "workspace/snapshot":
					allowed = map[string]bool{"commit": true}
				case "workspace/integrate":
					allowed = map[string]bool{"path": true, "commits": true}
				case "git/commit":
					allowed = map[string]bool{"path": true, "message": true}
				case "github/pr":
					allowed = map[string]bool{"path": true, "commit": true, "title": true, "body": true}
				}
				for key := range step.With {
					if !allowed[key] {
						return fmt.Errorf("step %s: unsupported controller action parameter %q", sid, key)
					}
				}
				if step.Uses == "workspace/integrate" || step.Uses == "git/commit" || step.Uses == "github/pr" {
					if step.With["path"] == nil {
						return fmt.Errorf("step %s requires managed path", sid)
					}
				}
				if step.Uses == "github/pr" && (step.With["commit"] == nil || step.With["title"] == nil) {
					return fmt.Errorf("step %s requires commit and title", sid)
				}
				if step.Uses == "git/commit" && step.With["message"] == nil {
					return fmt.Errorf("step %s requires message", sid)
				}
				if step.Context != (RunContext{}) {
					return fmt.Errorf("controller actions cannot use session context")
				}
				continue
			}
			if !strings.HasPrefix(step.Uses, "agent/") || !idPattern.MatchString(strings.TrimPrefix(step.Uses, "agent/")) {
				return fmt.Errorf("step %s: only agent/<name> targets are supported", sid)
			}
			if step.Context != (RunContext{}) {
				return fmt.Errorf("step %s: session/delivery context requires PicoClaw", sid)
			}
			for key := range step.With {
				switch key {
				case "model", "prompt", "message", "workspace", "output":
				default:
					return fmt.Errorf("step %s: unsupported parameter %q", sid, key)
				}
			}
			if step.With["prompt"] == nil && step.With["message"] == nil {
				return fmt.Errorf("step %s requires prompt or message", sid)
			}
			if _, err := ParseAgentOutputContract(step.With["output"]); err != nil {
				return fmt.Errorf("step %s: %w", sid, err)
			}
		}
	}
	_, err := topoJobs(w.Jobs)
	return err
}

func stepID(step Step, i int) string {
	if step.ID != "" {
		return step.ID
	}
	return fmt.Sprintf("step_%d", i+1)
}

func topoJobs(jobs map[string]Job) ([]string, error) {
	state := map[string]int{}
	var order []string
	var visit func(string) error
	visit = func(id string) error {
		job, ok := jobs[id]
		if !ok {
			return fmt.Errorf("unknown dependency %q", id)
		}
		if state[id] == 1 {
			return fmt.Errorf("job dependency cycle at %q", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range job.Needs {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(jobs))
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}
