package workflows

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// StepDefinition is source reuse in the containing workflow, not a runtime call.
// Step remains untyped until parameters are substituted, so typed YAML fields
// (including booleans and arrays) can be parameterized without string coercion.
type StepDefinition struct {
	Parameters map[string]Input `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	Step       map[string]any   `json:"step" yaml:"step"`
}

// StepReuse is retained in the admitted snapshot and execution inspection.
// Only the root of a use owns bindings; descendants carry source coordinates.
type StepReuse struct {
	Definition     string           `json:"definition"`
	Instance       string           `json:"instance"`
	DefinitionLine int              `json:"definition_line"`
	UseLine        int              `json:"use_line"`
	Parameters     map[string]Input `json:"parameters,omitempty"`
	Bindings       map[string]any   `json:"bindings,omitempty"`
}

const maxExpandedSteps = 4096
const maxStepDefinitionDepth = 16

var stepChildFields = []string{"sequence", "parallel", "onResult", "onComplete", "onIncomplete", "onError", "onFailure", "finally"}

type definitionExpander struct {
	definitions map[string]*yaml.Node
	sources     map[*yaml.Node]*StepReuse
	schemas     *yaml.Node
	count       int
	nodes       int
}

func yamlField(n *yaml.Node, key string) *yaml.Node {
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
	}
	return nil
}
func yamlSet(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
func yamlText(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
func cloneYAML(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		out.Content[i] = cloneYAML(c)
	}
	return &out
}

func expandStepDefinitions(root *yaml.Node) (map[*yaml.Node]*StepReuse, error) {
	e := &definitionExpander{definitions: map[string]*yaml.Node{}, sources: map[*yaml.Node]*StepReuse{}, schemas: yamlField(root, "schemas")}
	if defs := yamlField(root, "step-definitions"); defs != nil {
		if defs.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("step-definitions must be a mapping")
		}
		for i := 0; i < len(defs.Content); i += 2 {
			name, def := defs.Content[i].Value, defs.Content[i+1]
			if !idPattern.MatchString(name) {
				return nil, fmt.Errorf("invalid step definition %q", name)
			}
			if body := yamlField(def, "step"); body == nil || body.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("definition %s requires a step mapping", name)
			}
			if yamlField(yamlField(def, "step"), "id") != nil {
				return nil, fmt.Errorf("definition %s: root id belongs on its use", name)
			}
			var parsed StepDefinition
			if err := def.Decode(&parsed); err != nil {
				return nil, err
			}
			for param, spec := range parsed.Parameters {
				if !idPattern.MatchString(param) {
					return nil, fmt.Errorf("definition %s: invalid parameter %q", name, param)
				}
				switch spec.Type {
				case "", "string", "boolean", "number", "object", "array":
				default:
					return nil, fmt.Errorf("definition %s parameter %s: unsupported type %q", name, param, spec.Type)
				}
				if spec.Default != nil {
					if containsExpression(spec.Default) {
						return nil, fmt.Errorf("definition %s parameter %s: defaults must be literal", name, param)
					}
					if _, err := resolveInputs(&WorkflowCall{Inputs: map[string]Input{param: spec}}, map[string]any{param: spec.Default}, nil); err != nil {
						return nil, err
					}
				}
			}
			if err := checkDefinitionFields(yamlField(def, "step"), reflect.TypeOf(Step{})); err != nil {
				return nil, fmt.Errorf("definition %s: %w", name, err)
			}
			e.definitions[name] = def
		}
		// Check every declaration, including unused definitions, without expanding
		// an exponentially growing graph merely to detect a cycle.
		names := make([]string, 0, len(e.definitions))
		for name := range e.definitions {
			names = append(names, name)
		}
		sort.Strings(names)
		state := map[string]int{}
		var visit func(string, int) error
		visit = func(name string, depth int) error {
			if state[name] == 1 {
				return fmt.Errorf("recursive step definition %s", name)
			}
			if depth > maxStepDefinitionDepth {
				return fmt.Errorf("step definition nesting exceeds %d", maxStepDefinitionDepth)
			}
			if state[name] == 2 {
				return nil
			}
			def := e.definitions[name]
			if def == nil {
				return fmt.Errorf("unknown step definition %q", name)
			}
			state[name] = 1
			var walk func(*yaml.Node) error
			walk = func(step *yaml.Node) error {
				if use := yamlField(step, "use-definition"); use != nil {
					if err := validateDefinitionUse(step); err != nil {
						return err
					}
					if err := visit(use.Value, depth+1); err != nil {
						return err
					}
				}
				for _, key := range stepChildFields {
					if list := yamlField(step, key); list != nil {
						for _, c := range list.Content {
							if err := walk(c); err != nil {
								return err
							}
						}
					}
				}
				return nil
			}
			if err := walk(yamlField(def, "step")); err != nil {
				return fmt.Errorf("definition %s: %w", name, err)
			}
			// Parameter references must be whole nodes, never textual code injection.
			var parsed StepDefinition
			_ = def.Decode(&parsed)
			if _, err := substituteParameters(yamlField(def, "step"), func(name string) (*yaml.Node, error) {
				if _, ok := parsed.Parameters[name]; !ok {
					return nil, fmt.Errorf("unknown parameter %q", name)
				}
				return yamlText("placeholder"), nil
			}); err != nil {
				return fmt.Errorf("definition %s: %w", name, err)
			}
			state[name] = 2
			return nil
		}
		for _, name := range names {
			if err := visit(name, 0); err != nil {
				return nil, err
			}
		}
	}
	jobs := yamlField(root, "jobs")
	if jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 0; i < len(jobs.Content); i += 2 {
			if list := yamlField(jobs.Content[i+1], "steps"); list != nil {
				if err := e.expandList(list, 0); err != nil {
					return nil, fmt.Errorf("job %s: %w", jobs.Content[i].Value, err)
				}
			}
		}
	}
	return e.sources, nil
}

func validateDefinitionUse(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("definition use must be a mapping")
	}
	id := yamlField(n, "id")
	if id == nil || id.Tag != "!!str" || !idPattern.MatchString(id.Value) {
		return fmt.Errorf("definition use requires an explicit valid id")
	}
	use := yamlField(n, "use-definition")
	if use == nil || use.Tag != "!!str" || !idPattern.MatchString(use.Value) {
		return fmt.Errorf("use-definition requires a literal definition name")
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "id", "name", "if", "with", "use-definition", "continue-on-error", "onResult", "onComplete", "onIncomplete", "onError", "onFailure", "finally", "handlerTimeout":
		default:
			return fmt.Errorf("definition use cannot combine with %s", n.Content[i].Value)
		}
	}
	return nil
}

func (e *definitionExpander) expandList(list *yaml.Node, depth int) error {
	if list.Tag == "!!null" {
		return nil
	}
	if list.Kind != yaml.SequenceNode {
		return fmt.Errorf("steps must be a sequence")
	}
	filtered := list.Content[:0]
	for _, n := range list.Content {
		if n.Tag != "!!null" {
			filtered = append(filtered, n)
		}
	}
	list.Content = filtered
	for i, n := range list.Content {
		e.count++
		if e.count > maxExpandedSteps {
			return fmt.Errorf("expanded steps exceed %d", maxExpandedSteps)
		}
		if use := yamlField(n, "use-definition"); use != nil {
			expanded, err := e.expandUse(n, depth)
			if err != nil {
				return err
			}
			list.Content[i] = expanded
			continue
		}
		for _, key := range stepChildFields {
			if children := yamlField(n, key); children != nil {
				if err := e.expandList(children, depth); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (e *definitionExpander) expandUse(use *yaml.Node, depth int) (*yaml.Node, error) {
	if err := validateDefinitionUse(use); err != nil {
		return nil, err
	}
	if depth >= maxStepDefinitionDepth {
		return nil, fmt.Errorf("step definition nesting exceeds %d", maxStepDefinitionDepth)
	}
	name, id := yamlField(use, "use-definition").Value, yamlField(use, "id").Value
	def := e.definitions[name]
	if def == nil {
		return nil, fmt.Errorf("step %s: unknown step definition %q", id, name)
	}
	fail := func(err error) (*yaml.Node, error) {
		return nil, fmt.Errorf("step %s uses definition %s (use line %d, definition line %d): %w", id, name, use.Line, def.Line, err)
	}
	var parsed StepDefinition
	_ = def.Decode(&parsed)
	supplied := map[string]*yaml.Node{}
	if with := yamlField(use, "with"); with != nil {
		if with.Kind != yaml.MappingNode {
			return fail(fmt.Errorf("with must be a mapping"))
		}
		for i := 0; i < len(with.Content); i += 2 {
			supplied[with.Content[i].Value] = with.Content[i+1]
		}
	}
	for key := range supplied {
		if _, ok := parsed.Parameters[key]; !ok {
			return fail(fmt.Errorf("unknown parameter %q", key))
		}
	}
	bindings := map[string]any{}
	for key, spec := range parsed.Parameters {
		value := supplied[key]
		if value == nil && spec.Default != nil {
			value = &yaml.Node{}
			if err := value.Encode(spec.Default); err != nil {
				return fail(err)
			}
			supplied[key] = value
		}
		if value == nil {
			if spec.Required {
				return fail(fmt.Errorf("required parameter %q is missing", key))
			}
			continue
		}
		var v any
		if err := value.Decode(&v); err != nil {
			return fail(err)
		}
		bindings[key] = v
		if text, dynamic := v.(string); !dynamic || !wholeExpression(text) {
			if _, err := resolveInputs(&WorkflowCall{Inputs: map[string]Input{key: spec}}, map[string]any{key: v}, nil); err != nil {
				return fail(err)
			}
		}
	}
	// Namespace only definition-authored references, before injecting bindings.
	body := cloneYAML(yamlField(def, "step"))
	if err := e.inlineDefinitionOutputs(body); err != nil {
		return fail(err)
	}
	ids := map[string]string{}
	if err := namespaceDefinition(body, id, ids); err != nil {
		return fail(err)
	}
	if err := rewriteDefinitionReferences(body, ids); err != nil {
		return fail(err)
	}
	body, err := substituteParameters(body, func(key string) (*yaml.Node, error) {
		if _, ok := parsed.Parameters[key]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", key)
		}
		if supplied[key] == nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
		}
		if err := e.charge(supplied[key]); err != nil {
			return nil, err
		}
		return cloneYAML(supplied[key]), nil
	})
	if err != nil {
		return fail(err)
	}
	// Nested root aliases are intentionally unsupported; nesting is expressed
	// inside sequence/parallel children so each use has an observable identity.
	if yamlField(body, "use-definition") != nil {
		return fail(fmt.Errorf("definition root cannot use another definition; use a sequence"))
	}
	yamlSet(body, "id", yamlText(id))
	for i := 0; i < len(use.Content); i += 2 {
		key := use.Content[i].Value
		if key != "id" && key != "with" && key != "use-definition" {
			yamlSet(body, key, cloneYAML(use.Content[i+1]))
		}
	}
	// Bound materialized YAML as well as step count: large parameter objects
	// must not multiply into unbounded admission memory.
	if err := e.charge(body); err != nil {
		return fail(err)
	}
	source := &StepReuse{Definition: name, Instance: id, DefinitionLine: def.Line, UseLine: use.Line, Parameters: parsed.Parameters, Bindings: bindings}
	var mark func(*yaml.Node)
	mark = func(n *yaml.Node) {
		child := *source
		child.Parameters = nil
		child.Bindings = nil
		e.sources[n] = &child
		for _, key := range stepChildFields {
			if list := yamlField(n, key); list != nil {
				for _, c := range list.Content {
					mark(c)
				}
			}
		}
	}
	mark(body)
	e.sources[body] = source
	// Expand every child with the use depth retained through ordinary blocks.
	for _, key := range stepChildFields {
		if list := yamlField(body, key); list != nil {
			if err := e.expandList(list, depth+1); err != nil {
				return fail(err)
			}
		}
	}
	if err := checkNode(body, reflect.TypeOf(Step{}), 0); err != nil {
		return fail(err)
	}
	var typed Step
	if err := body.Decode(&typed); err != nil {
		return fail(err)
	}
	return body, nil
}

func (e *definitionExpander) charge(n *yaml.Node) error {
	e.nodes += len(n.Value) + 32
	if e.nodes > 8<<20 {
		return fmt.Errorf("expanded definition data exceeds 8 MiB")
	}
	for _, c := range n.Content {
		if err := e.charge(c); err != nil {
			return err
		}
	}
	return nil
}

func namespaceDefinition(root *yaml.Node, instance string, ids map[string]string) error {
	var collect func(*yaml.Node, string, bool) error
	collect = func(n *yaml.Node, path string, isRoot bool) error {
		if !isRoot {
			id := path
			if value := yamlField(n, "id"); value != nil {
				id = value.Value
			}
			if !idPattern.MatchString(id) || ids[id] != "" {
				return fmt.Errorf("invalid or duplicate definition step id %q", id)
			}
			ids[id] = instance + "__" + id
			yamlSet(n, "id", yamlText(ids[id]))
		}
		for _, key := range []string{"sequence", "parallel"} {
			if list := yamlField(n, key); list != nil {
				filtered := list.Content[:0]
				for _, c := range list.Content {
					if c.Tag != "!!null" {
						filtered = append(filtered, c)
					}
				}
				list.Content = filtered
				for i, c := range list.Content {
					if err := collect(c, stepPath(path+"_"+key, i), false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return collect(root, "step", true)
}

// Rewrite only evaluated step fields. Handler-local IDs shadow the job scope;
// names, file paths and other literal configuration are left byte-for-byte alone.
func rewriteDefinitionReferences(step *yaml.Node, ids map[string]string) error {
	var value func(*yaml.Node, bool) error
	value = func(n *yaml.Node, bare bool) error {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
			rewrite := func(body string) (string, error) { return rewriteDefinitionExpression(body, ids) }
			if bare && !strings.Contains(n.Value, "${{") {
				next, err := rewrite(n.Value)
				n.Value = next
				if strings.HasPrefix(next, "parameters.") {
					n.Value = "${{ " + next + " }}"
				}
				return err
			}
			var first error
			n.Value = expressionPattern.ReplaceAllStringFunc(n.Value, func(match string) string {
				next, err := rewrite(expressionPattern.FindStringSubmatch(match)[1])
				if err != nil {
					first = err
				}
				return "${{ " + next + " }}"
			})
			return first
		}
		for i, c := range n.Content {
			if n.Kind == yaml.MappingNode && i%2 == 0 {
				continue
			}
			if err := value(c, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, key := range []string{"input", "env", "if", "with", "argv", "run", "working-directory", "prompt"} {
		if n := yamlField(step, key); n != nil {
			if err := value(n, key == "if"); err != nil {
				return err
			}
		}
	}
	for _, key := range stepChildFields {
		if list := yamlField(step, key); list != nil {
			scope := ids
			handler := key != "sequence" && key != "parallel"
			if handler {
				scope = map[string]string{}
				for k, v := range ids {
					scope[k] = v
				}
			}
			index := 0
			for _, child := range list.Content {
				if child.Tag == "!!null" {
					continue
				}
				if err := rewriteDefinitionReferences(child, scope); err != nil {
					return err
				}
				if handler {
					id := fmt.Sprintf("step_%d", index+1)
					if n := yamlField(child, "id"); n != nil {
						id = n.Value
					}
					scope[id] = id
				}
				index++
			}
		}
	}
	return nil
}

func rewriteDefinitionExpression(body string, ids map[string]string) (string, error) {
	body = strings.TrimSpace(body)
	for _, op := range []string{" or ", " and "} {
		if terms, ok := splitExpressionLogical(body, op); ok {
			for i, term := range terms {
				next, err := rewriteDefinitionExpression(term, ids)
				if err != nil {
					return "", err
				}
				terms[i] = next
			}
			return strings.Join(terms, op), nil
		}
	}
	for _, op := range []string{" == ", " != ", " >= ", " <= ", " > ", " < "} {
		if i := strings.Index(body, op); i >= 0 {
			left, err := rewriteDefinitionExpression(body[:i], ids)
			if err != nil {
				return "", err
			}
			right, err := rewriteDefinitionExpression(body[i+len(op):], ids)
			return left + op + right, err
		}
	}
	// Flatten not prefixes rather than using input-sized recursion.
	prefix := ""
	for strings.HasPrefix(body, "not ") {
		prefix += "not "
		body = strings.TrimSpace(body[4:])
	}
	parts := strings.Split(body, ".")
	if len(parts) > 1 && parts[0] == "steps" {
		replacement, ok := ids[parts[1]]
		if !ok {
			return "", fmt.Errorf("definition-local step %q not found; pass caller results through parameters", parts[1])
		}
		parts[1] = replacement
		body = strings.Join(parts, ".")
	}
	return prefix + body, nil
}

func substituteParameters(n *yaml.Node, lookup func(string) (*yaml.Node, error)) (*yaml.Node, error) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		matches := expressionPattern.FindAllStringSubmatch(n.Value, -1)
		for _, m := range matches {
			body := strings.TrimSpace(m[1])
			var parameter string
			err := walkResultExpression(body, func(parts []string) error {
				if len(parts) > 0 && parts[0] == "parameters" {
					if len(parts) != 2 {
						return fmt.Errorf("parameter references require parameters.name")
					}
					parameter = parts[1]
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if parameter != "" {
				if len(matches) != 1 || strings.TrimSpace(n.Value) != m[0] || body != "parameters."+parameter {
					return nil, fmt.Errorf("parameter references must occupy the whole value")
				}
				return lookup(parameter)
			}
		}
	}
	out := *n
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			out.Content[i] = cloneYAML(c)
			continue
		}
		next, err := substituteParameters(c, lookup)
		if err != nil {
			return nil, err
		}
		out.Content[i] = next
	}
	return &out, nil
}

func containsExpression(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, "${{")
	case map[string]any:
		for _, c := range v {
			if containsExpression(c) {
				return true
			}
		}
	case []any:
		for _, c := range v {
			if containsExpression(c) {
				return true
			}
		}
	}
	return false
}

func attachReuseSources(steps []Step, list *yaml.Node, sources map[*yaml.Node]*StepReuse) {
	if list == nil {
		return
	}
	for i := range steps {
		n := list.Content[i]
		steps[i].Reuse = sources[n]
		children := []*[]Step{&steps[i].Sequence, &steps[i].Parallel, &steps[i].OnResult, &steps[i].OnComplete, &steps[i].OnIncomplete, &steps[i].OnError, &steps[i].OnFailure, &steps[i].Finally}
		for j, key := range stepChildFields {
			attachReuseSources(*children[j], yamlField(n, key), sources)
		}
	}
}

func validateReuseBindings(step Step, ec expressionContext) error {
	if step.Reuse == nil || len(step.Reuse.Parameters) == 0 {
		return nil
	}
	bindings, err := renderMap(step.Reuse.Bindings, ec)
	if err == nil {
		_, err = resolveInputs(&WorkflowCall{Inputs: step.Reuse.Parameters}, bindings, nil)
	}
	if err != nil {
		return fmt.Errorf("step %s uses definition %s: %w", step.ID, step.Reuse.Definition, err)
	}
	return nil
}

func hasReusableSteps(steps []Step) bool {
	for _, s := range lifecycleSteps(steps) {
		if s.Reuse != nil {
			return true
		}
	}
	return false
}
func reuseBindings(step Step) any {
	if step.Reuse != nil {
		return step.Reuse.Bindings
	}
	return nil
}

// Templates may contain parameter placeholders in typed fields. Check field
// names independently of values; the fully substituted tree gets ordinary
// strict parsing and runtime validation at each use.
func checkDefinitionFields(n *yaml.Node, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf([]Step{}) && n.Kind != yaml.SequenceNode && n.Tag != "!!null" {
		return fmt.Errorf("step structure must be a literal step sequence")
	}
	if n.Kind == yaml.MappingNode {
		fields := map[string]reflect.Type{}
		if t.Kind() == reflect.Struct {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				key := strings.Split(f.Tag.Get("yaml"), ",")[0]
				if key != "" && key != "-" {
					fields[key] = f.Type
				}
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			child := reflect.TypeOf((*any)(nil)).Elem()
			if t.Kind() == reflect.Struct {
				var ok bool
				child, ok = fields[n.Content[i].Value]
				if !ok {
					return fmt.Errorf("unknown field %q", n.Content[i].Value)
				}
			} else if t.Kind() == reflect.Map {
				child = t.Elem()
			}
			if err := checkDefinitionFields(n.Content[i+1], child); err != nil {
				return err
			}
		}
	} else if n.Kind == yaml.SequenceNode {
		child := reflect.TypeOf((*any)(nil)).Elem()
		if t.Kind() == reflect.Slice {
			child = t.Elem()
		}
		for _, c := range n.Content {
			if err := checkDefinitionFields(c, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// A named output schema is rendered in its consumer's scope. Copy it before
// rewriting definition-local references, just as admission resolves ordinary
// consumers, while retaining the original declaration in the snapshot.
func (e *definitionExpander) inlineDefinitionOutputs(step *yaml.Node) error {
	uses := yamlField(step, "uses")
	if uses != nil && uses.Value == "agent/run" || yamlField(step, "conversation") != nil || yamlField(step, "argv") != nil || yamlField(step, "run") != nil {
		output := yamlField(yamlField(step, "with"), "output")
		if name := yamlField(output, "schema"); name != nil && name.Kind == yaml.ScalarNode {
			if schema := yamlField(e.schemas, name.Value); schema != nil {
				if err := e.charge(schema); err != nil {
					return err
				}
				yamlSet(output, "schema", cloneYAML(schema))
			}
		}
	}
	for _, key := range stepChildFields {
		if list := yamlField(step, key); list != nil {
			for _, child := range list.Content {
				if err := e.inlineDefinitionOutputs(child); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
