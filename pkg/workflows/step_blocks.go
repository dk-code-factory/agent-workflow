package workflows

import "fmt"

// FlattenSteps returns all structural and executable steps in declaration
// preorder, assigning stable job-scoped IDs to anonymous steps. Child IDs use
// their physical index path, independent of explicit ancestor IDs.
func FlattenSteps(steps []Step) []Step {
	var out []Step
	var walk func([]Step, string)
	walk = func(steps []Step, prefix string) {
		for i, step := range steps {
			path := stepPath(prefix, i)
			if step.ID == "" {
				step.ID = path
			}
			out = append(out, step)
			walk(step.Parallel, path+"_parallel")
			walk(step.Sequence, path+"_sequence")
		}
	}
	walk(steps, "")
	return out
}

func stepPath(prefix string, index int) string {
	if prefix == "" {
		prefix = "step"
	}
	return fmt.Sprintf("%s_%d", prefix, index+1)
}

func mapSteps(steps []Step, prefix string, f func(Step) (Step, error)) ([]Step, error) {
	out := make([]Step, len(steps))
	for i, source := range steps {
		path := stepPath(prefix, i)
		step, err := f(source)
		if err != nil {
			id := source.ID
			if id == "" {
				id = path
			}
			return nil, fmt.Errorf("step %s: %w", id, err)
		}
		if step.Parallel != nil {
			step.Parallel, err = mapSteps(step.Parallel, path+"_parallel", f)
			if err != nil {
				return nil, err
			}
		}
		if step.Sequence != nil {
			step.Sequence, err = mapSteps(step.Sequence, path+"_sequence", f)
			if err != nil {
				return nil, err
			}
		}
		for _, list := range []*[]Step{&step.OnResult, &step.OnComplete, &step.OnIncomplete, &step.OnError, &step.OnFailure, &step.Finally} {
			if *list != nil {
				*list, err = mapSteps(*list, "", f)
				if err != nil {
					return nil, err
				}
			}
		}
		out[i] = step
	}
	return out, nil
}

func isStepBlock(step Step) bool { return step.Parallel != nil || step.Sequence != nil }

func hasStepBlocks(steps []Step) bool {
	for _, step := range steps {
		if isStepBlock(step) {
			return true
		}
	}
	return false
}

func copyStepScope(source map[string]bool) map[string]bool {
	out := make(map[string]bool, len(source))
	for id, value := range source {
		out[id] = value
	}
	return out
}

func identifiedSteps(steps []Step, prefix string) []Step {
	out := make([]Step, len(steps))
	for i, step := range steps {
		path := stepPath(prefix, i)
		if step.ID == "" {
			step.ID = path
		}
		if step.Parallel != nil {
			step.Parallel = identifiedSteps(step.Parallel, path+"_parallel")
		}
		if step.Sequence != nil {
			step.Sequence = identifiedSteps(step.Sequence, path+"_sequence")
		}
		out[i] = step
	}
	return out
}

func validateStepBlock(step Step) error {
	if step.Parallel != nil && step.Sequence != nil {
		return fmt.Errorf("parallel and sequence cannot combine")
	}
	if len(step.Parallel) == 0 && len(step.Sequence) == 0 {
		return fmt.Errorf("block requires a nonempty step sequence")
	}
	if step.Conversation != nil || step.Start != "" || step.FinishBy != "" || step.MaxTurns != 0 || step.MaxMessages != 0 || step.Prompt != nil || step.Uses != "" || len(step.With) != 0 || len(step.Argv) != 0 || step.Run != "" || step.Shell != "" || step.WorkingDirectory != "" || step.Timeout != "" || step.Context != (RunContext{}) || step.ContinueOnError || hasLifecycle(step) || step.HandlerTimeout != "" {
		return fmt.Errorf("blocks allow only id, name, if, and parallel or sequence")
	}
	return nil
}
