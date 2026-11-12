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

// validateCanonicalAgentStep rejects removed authoring forms. Admitted snapshots
// must already use the canonical interface; resume never converts old runs.
func normalizeAgentStep(step Step) (Step, error) {
	if step.Prompt != nil && step.Conversation == nil {
		return Step{}, fmt.Errorf("prompt requires conversation; use with.prompt for agent/run")
	}
	if strings.HasPrefix(step.Uses, "agent/") && step.Uses != "agent/run" {
		return Step{}, fmt.Errorf("only agent/run is supported; supply a native definition in with.agent")
	}
	return step, nil
}

// YAML field presence matters for competing authoring forms, even when an
// explicitly supplied legacy field is empty or null.
func checkAgentShorthand(n *yaml.Node) error {
	fields := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		fields[n.Content[i].Value] = n.Content[i+1]
	}
	if roles := fields["conversation"]; roles != nil {
		if roles.Kind != yaml.SequenceNode || len(roles.Content) < 2 {
			return fmt.Errorf("conversation requires at least two participant roles")
		}
		for _, role := range roles.Content {
			if role.Kind != yaml.ScalarNode || role.Tag != "!!str" {
				return fmt.Errorf("conversation roles must be strings")
			}
		}
		for _, key := range []string{"onResult", "onComplete", "onIncomplete", "onError", "onFailure", "finally", "handlerTimeout"} {
			if fields[key] != nil {
				return fmt.Errorf("conversations cannot own or appear inside lifecycle handlers")
			}
		}
		for _, key := range []string{"agent", "uses", "argv", "run", "shell", "working-directory", "context", "parallel", "sequence"} {
			if fields[key] != nil {
				return fmt.Errorf("conversation cannot combine with %s", key)
			}
		}
	}
	for _, key := range []string{"maxTurns", "maxMessages"} {
		if value := fields[key]; value != nil {
			var count int
			if value.Tag != "!!int" || value.Decode(&count) != nil || count < 1 {
				return fmt.Errorf("%s must be a positive integer", key)
			}
		}
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
	for _, key := range []string{"agent", "prompt", "start", "finishBy"} {
		if value := fields[key]; value != nil && (value.Kind != yaml.ScalarNode || value.Tag != "!!str") {
			return fmt.Errorf("%s must be a string", key)
		}
	}
	if fields["agent"] != nil {
		return fmt.Errorf("agent shorthand is unsupported; use agent/run with with.agent")
	}
	if fields["prompt"] != nil && fields["conversation"] == nil {
		return fmt.Errorf("prompt requires conversation; use with.prompt for agent/run")
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
