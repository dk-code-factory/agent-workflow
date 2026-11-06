package workflowdef

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/theory/jsonpath"
	"github.com/theory/jsonpath/spec"
)

var reserved = []string{"input", "steps", "previous", "item", "error", "failed", "event", "run", "breaker"}

type validation struct {
	d           *Definition
	ids         map[string]bool
	policies    map[string]any
	resourceIDs map[string]bool
}

type scope struct {
	steps   map[string]bool
	roots   map[string]bool
	recover bool
}

func (s scope) copy() scope { return scope{maps.Clone(s.steps), maps.Clone(s.roots), s.recover} }

func (d *Definition) validate() error {
	v := &validation{d: d, ids: map[string]bool{}, policies: map[string]any{}, resourceIDs: map[string]bool{}}
	for _, name := range slices.Sorted(maps.Keys(object(d.Data["schemas"]))) {
		use := object(d.Data["schemas"])[name]
		schema, err := resolveSchema(use)
		if err != nil {
			return fmt.Errorf("schema %s: %w", name, err)
		}
		d.schemas[name] = schema
		if err := v.resourceID(use); err != nil {
			return err
		}
	}
	s := scope{steps: map[string]bool{}, roots: map[string]bool{"input": true, "steps": true, "run": true}}
	for name, value := range object(d.Data["sources"]) {
		if slices.Contains(reserved, name) {
			return fmt.Errorf("reserved source name %q", name)
		}
		if _, secret := object(value)["secret"]; !secret {
			s.roots[name] = true
		}
		if use, exists := object(value)["schema"]; exists {
			if err := v.schema(use); err != nil {
				return err
			}
		}
		if _, isSecret := object(value)["secret"]; !isSecret {
			if value, exists := object(value)["value"]; exists {
				if use, exists := object(d.Data["sources"])[name].(map[string]any)["schema"]; exists {
					if err := d.ValidateValue(use, value); err != nil {
						return fmt.Errorf("source %s: %w", name, err)
					}
				}
			}
		}
	}
	for _, key := range []string{"input", "output"} {
		if use, exists := d.Data[key]; exists {
			if err := v.schema(use); err != nil {
				return err
			}
		}
	}
	end, err := v.sequence(list(d.Data["steps"]), s)
	if err != nil {
		return err
	}
	if value, exists := d.Data["return"]; exists {
		if err := v.binding(value, end); err != nil {
			return err
		}
	}
	return v.handlers(d.Data, s, end)
}

func (v *validation) resourceID(use any) error {
	if id := text(object(use)["$id"]); id != "" {
		if v.resourceIDs[id] {
			return fmt.Errorf("conflicting schema resource ID %q", id)
		}
		v.resourceIDs[id] = true
	}
	return nil
}

func (v *validation) schema(use any) error {
	if _, err := v.d.schema(use); err != nil {
		return err
	}
	if _, named := use.(string); !named {
		return v.resourceID(use)
	}
	return nil
}

func (v *validation) sequence(steps []any, s scope) (scope, error) {
	s = s.copy()
	for i, value := range steps {
		var err error
		s, err = v.step(object(value), s)
		if err != nil {
			return s, fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return s, nil
}

func (v *validation) step(node map[string]any, s scope) (scope, error) {
	s = s.copy()
	startScope := s.copy()
	if id := text(node["id"]); id != "" {
		if v.ids[id] {
			return s, fmt.Errorf("duplicate step ID %q", id)
		}
		v.ids[id] = true
	}
	for _, field := range []string{"output"} {
		if use, exists := node[field]; exists {
			if err := v.schema(use); err != nil {
				return s, err
			}
		}
	}
	if prompt, exists := node["prompt"]; exists {
		if err := v.prompt(text(prompt), s); err != nil {
			return s, err
		}
	}
	for _, field := range []string{"with", "value", "initial"} {
		if value, exists := node[field]; exists {
			if err := v.binding(value, s); err != nil {
				return s, err
			}
		}
	}
	for _, name := range object(node["credentials"]) {
		source := object(object(v.d.Data["sources"])[text(name)])
		if _, secret := source["secret"]; !secret {
			return s, fmt.Errorf("credential source %q is not a secret", name)
		}
	}
	roles := []string{}
	if role := text(node["agent"]); role != "" {
		roles = append(roles, role)
	}
	for _, role := range list(node["conversation"]) {
		roles = append(roles, text(role))
	}
	if policy, exists := node["breaker"]; exists {
		for _, role := range roles {
			if text(object(policy)["agent"]) == role {
				return s, fmt.Errorf("breaker checker must differ from guarded role %q", role)
			}
			if previous, exists := v.policies[role]; exists && !reflect.DeepEqual(previous, policy) {
				return s, fmt.Errorf("conflicting breaker policies for role %q", role)
			}
			v.policies[role] = policy
		}
		if prompt, exists := object(policy)["prompt"]; exists {
			ps := s.copy()
			ps.roots["breaker"] = true
			if err := v.prompt(text(prompt), ps); err != nil {
				return s, err
			}
		}
	}
	if _, exists := node["recover"]; exists && !s.recover {
		return s, fmt.Errorf("recover requires a failed agent context")
	}
	if participants := list(node["conversation"]); len(participants) > 0 {
		for _, field := range []string{"start", "finishBy"} {
			if role := text(node[field]); role != "" && !slices.Contains(roles, role) {
				return s, fmt.Errorf("conversation %s %q is not a participant", field, role)
			}
		}
	}
	switch {
	case node["parallel"] != nil:
		merged := s.copy()
		for _, child := range list(node["parallel"]) {
			out, err := v.step(object(child), s)
			if err != nil {
				return s, err
			}
			maps.Copy(merged.steps, out.steps)
		}
		s = merged
	case node["if"] != nil:
		if err := v.condition(node["if"], s); err != nil {
			return s, err
		}
		branchScope := s.copy()
		for _, branch := range []string{"then", "else"} {
			out, err := v.sequence(list(node[branch]), branchScope)
			if err != nil {
				return s, err
			}
			maps.Copy(s.steps, out.steps)
		}
	case node["repeat"] != nil:
		body := s.copy()
		body.roots["previous"] = true
		out, err := v.sequence(list(node["steps"]), body)
		if err != nil {
			return s, err
		}
		if until, exists := node["until"]; exists {
			if err := v.condition(until, out); err != nil {
				return s, err
			}
		}
	case node["each"] != nil:
		if err := v.path(text(node["each"]), s); err != nil {
			return s, err
		}
		body := s.copy()
		body.roots["item"] = true
		if _, err := v.sequence(list(node["steps"]), body); err != nil {
			return s, err
		}
	case node["steps"] != nil:
		var err error
		s, err = v.sequence(list(node["steps"]), s)
		if err != nil {
			return s, err
		}
	}
	if err := v.handlers(node, startScope, s); err != nil {
		return s, err
	}
	if id := text(node["id"]); id != "" {
		s.steps[id] = true
	}
	return s, nil
}

func (v *validation) handlers(node map[string]any, start, s scope) error {
	for _, phase := range []string{"onStart", "onSuccess", "onFailure", "finally", "onError"} {
		hs := s.copy()
		if phase == "onStart" || phase == "onError" || phase == "onFailure" || phase == "finally" {
			hs = start.copy()
		}
		if phase == "onError" {
			hs.roots["error"], hs.roots["failed"] = true, true
			hs.recover = text(node["agent"]) != ""
		} else {
			hs.roots["event"] = true
			if phase == "onFailure" {
				hs.roots["error"] = true
			}
		}
		if _, err := v.sequence(list(node[phase]), hs); err != nil {
			return fmt.Errorf("%s: %w", phase, err)
		}
	}
	return nil
}

func (v *validation) path(query string, s scope) error {
	if len(query) > 4096 {
		return fmt.Errorf("JSONPath exceeds 4096 bytes")
	}
	p, err := jsonpath.Parse(query)
	if err != nil {
		return fmt.Errorf("invalid JSONPath %q: %w", query, err)
	}
	segments := p.Query().Segments()
	name := func(i int) string {
		if i >= len(segments) || strings.HasPrefix(segments[i].String(), "..") || len(segments[i].Selectors()) != 1 {
			return ""
		}
		n, _ := segments[i].Selectors()[0].(spec.Name)
		return string(n)
	}
	root := name(0)
	if root != "" && !s.roots[root] {
		return fmt.Errorf("context %q is unavailable in %s", root, query)
	}
	if root == "steps" && name(1) != "" && !s.steps[name(1)] {
		return fmt.Errorf("step %q is not visible in %s", name(1), query)
	}
	return nil
}

func (v *validation) binding(value any, s scope) error {
	switch value := value.(type) {
	case string:
		if strings.HasPrefix(value, "$") {
			return v.path(value, s)
		}
	case []any:
		for _, child := range value {
			if err := v.binding(child, s); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(value) == 1 {
			if _, literal := value["literal"]; literal {
				return nil
			}
			if query, all := value["all"]; all {
				if _, ok := query.(string); !ok {
					return fmt.Errorf("all requires a JSONPath string")
				}
				return v.path(text(query), s)
			}
		}
		for _, child := range value {
			if err := v.binding(child, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *validation) condition(value any, s scope) error {
	switch value := value.(type) {
	case string:
		return v.path(value, s)
	case map[string]any:
		if path, exists := value["path"]; exists {
			if err := v.path(text(path), s); err != nil {
				return err
			}
			if schema, exists := value["matches"]; exists {
				return v.schema(schema)
			}
			return nil
		}
		for _, field := range []string{"all", "any"} {
			for _, child := range list(value[field]) {
				if err := v.condition(child, s); err != nil {
					return err
				}
			}
		}
		if child, exists := value["not"]; exists {
			return v.condition(child, s)
		}
	}
	return nil
}

func (v *validation) prompt(prompt string, s scope) error {
	_, err := interpolate(prompt, func(query string) (string, error) { return "", v.path(query, s) })
	return err
}

// Interpolation is one pass; inserted text cannot inject a second expression.
func interpolate(prompt string, resolve func(string) (string, error)) (string, error) {
	var out strings.Builder
	for {
		start := strings.Index(prompt, "{{")
		if start < 0 {
			out.WriteString(prompt)
			return out.String(), nil
		}
		out.WriteString(prompt[:start])
		prompt = prompt[start+2:]
		quote := byte(0)
		escaped := false
		end := -1
		for i := 0; i+1 < len(prompt); i++ {
			c := prompt[i]
			if quote != 0 {
				if escaped {
					escaped = false
				} else if c == '\\' {
					escaped = true
				} else if c == quote {
					quote = 0
				}
				continue
			}
			if c == '\'' || c == '"' {
				quote = c
				continue
			}
			if c == '}' && prompt[i+1] == '}' {
				end = i
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("unterminated prompt placeholder")
		}
		value, err := resolve(strings.TrimSpace(prompt[:end]))
		if err != nil {
			return "", err
		}
		out.WriteString(value)
		prompt = prompt[end+2:]
	}
}
