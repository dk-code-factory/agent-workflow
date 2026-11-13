package workflows

import (
	"fmt"
	"maps"
)

const DurableTaskStateV1 = "durable-task-state-v1"

// The group is host admission data, retained outside the authored YAML graph.
// Resume uses the snapshot's group; a replacement run opts into it explicitly.
func admitTaskGroup(req *RunRequest) error {
	req.Metadata = maps.Clone(req.Metadata)
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	group := req.TaskGroup
	if raw, present := req.Metadata["task_group"]; present {
		retained, ok := raw.(string)
		if !ok || (group != "" && group != retained) {
			return fmt.Errorf("task group conflicts with admission metadata")
		}
		if group == "" {
			group = retained
		}
	}
	if group == "" {
		delete(req.Metadata, "task_group")
		return nil
	}
	if err := ValidateTaskGroup(group); err != nil {
		return err
	}
	req.Metadata["task_group"] = group
	return nil
}
