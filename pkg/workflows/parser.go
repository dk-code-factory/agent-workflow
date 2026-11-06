package workflows

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse retains PicoClaw's schema while rejecting unknown fields, aliases,
// duplicate keys, and multiple documents instead of silently ignoring them.
func Parse(data []byte) (*Workflow, error) {
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("workflow exceeds 4 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := d.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one YAML document")
	}
	if len(node.Content) != 1 {
		return nil, fmt.Errorf("workflow is empty")
	}
	if err := checkNode(node.Content[0], reflect.TypeOf(Workflow{}), 0); err != nil {
		return nil, err
	}
	var w Workflow
	if err := node.Decode(&w); err != nil {
		return nil, err
	}
	for id, job := range w.Jobs {
		var err error
		job.Steps, err = mapSteps(job.Steps, "", func(step Step) (Step, error) { return normalizeAgentStep(step) })
		if err != nil {
			return nil, fmt.Errorf("job %s %w", id, err)
		}
		w.Jobs[id] = job
	}
	return &w, nil
}

// normalizeAgentStep lowers the authoring shorthand to the existing execution
// contract before role admission, expression evaluation, and durable snapshots.
func normalizeAgentStep(step Step) (Step, error) {
	if step.Agent == nil {
		if step.Prompt != nil {
			return Step{}, fmt.Errorf("prompt requires agent; use with.prompt with uses")
		}
		return step, nil
	}
	if !idPattern.MatchString(*step.Agent) {
		return Step{}, fmt.Errorf("agent requires a valid role name")
	}
	if step.Uses != "" {
		return Step{}, fmt.Errorf("agent cannot combine with uses")
	}
	if step.Prompt != nil {
		if _, exists := step.With["prompt"]; exists {
			return Step{}, fmt.Errorf("prompt cannot combine with with.prompt")
		}
		parameters := make(map[string]any, len(step.With)+1)
		for key, value := range step.With {
			parameters[key] = value
		}
		parameters["prompt"] = *step.Prompt
		step.With = parameters
	}
	step.Uses = "agent/" + *step.Agent
	step.Agent, step.Prompt = nil, nil
	return step, nil
}

// YAML field presence matters for competing authoring forms, even when an
// explicitly supplied legacy field is empty or null.
func checkAgentShorthand(n *yaml.Node) error {
	fields := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		fields[n.Content[i].Value] = n.Content[i+1]
	}
	for _, key := range []string{"parallel", "sequence"} {
		if block := fields[key]; block != nil {
			if block.Kind != yaml.SequenceNode || len(block.Content) == 0 {
				return fmt.Errorf("%s requires a nonempty step sequence", key)
			}
			for field := range fields {
				if field != key && field != "id" && field != "name" && field != "if" {
					return fmt.Errorf("%s block cannot combine with %s", key, field)
				}
			}
		}
	}
	for _, key := range []string{"agent", "prompt"} {
		if value := fields[key]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return fmt.Errorf("%s must be a string", key)
		}
	}
	if fields["agent"] != nil && fields["uses"] != nil {
		return fmt.Errorf("agent cannot combine with uses")
	}
	if fields["prompt"] != nil {
		if fields["agent"] == nil {
			return fmt.Errorf("prompt requires agent; use with.prompt with uses")
		}
		if with := fields["with"]; with != nil && with.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(with.Content); i += 2 {
				if with.Content[i].Value == "prompt" {
					return fmt.Errorf("prompt cannot combine with with.prompt")
				}
			}
		}
	}
	return nil
}

func checkNode(n *yaml.Node, t reflect.Type, depth int) error {
	if depth > 64 {
		return fmt.Errorf("YAML nesting exceeds 64")
	}
	if n.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML aliases are unsupported")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	fields := map[string]reflect.Type{}
	if t.Kind() == reflect.Struct {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			key := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if key != "" && key != "-" {
				fields[key] = f.Type
			}
		}
	}
	if n.Kind == yaml.MappingNode {
		if t == reflect.TypeOf(Step{}) {
			if err := checkAgentShorthand(n); err != nil {
				return fmt.Errorf("step at line %d: %w", n.Line, err)
			}
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Value == "<<" {
				return fmt.Errorf("invalid mapping key")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate key %q", key.Value)
			}
			seen[key.Value] = true
			child := reflect.TypeOf((*any)(nil)).Elem()
			if t.Kind() == reflect.Struct {
				var ok bool
				child, ok = fields[key.Value]
				if !ok {
					return fmt.Errorf("unknown field %q", key.Value)
				}
			} else if t.Kind() == reflect.Map {
				child = t.Elem()
			}
			if err := checkNode(n.Content[i+1], child, depth+1); err != nil {
				return err
			}
		}
	} else if n.Kind == yaml.SequenceNode {
		child := reflect.TypeOf((*any)(nil)).Elem()
		if t.Kind() == reflect.Slice {
			child = t.Elem()
		}
		for _, item := range n.Content {
			if err := checkNode(item, child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
