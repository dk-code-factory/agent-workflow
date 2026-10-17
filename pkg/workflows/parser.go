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
	return &w, nil
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
