package workflows

import "context"

type ActionRequest struct {
	RunID     string
	JobID     string
	StepID    string
	Iteration int
	Uses      string
	With      map[string]any
	Metadata  map[string]any
	Secrets   map[string]string
	Needs     map[string]JobExecution
	Steps     map[string]StepExecution
}
type ActionRunner interface {
	Admission(context.Context, map[string]*Workflow) (map[string]any, error)
	RunAction(context.Context, ActionRequest) (map[string]any, error)
}

func IsControllerAction(uses string) bool {
	switch uses {
	case "workspace/create", "workspace/snapshot", "workspace/integrate", "git/commit", "github/pr":
		return true
	}
	return false
}
