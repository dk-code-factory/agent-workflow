package workflows

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"gopkg.in/yaml.v3"
)

func isWholeExpressionValue(value any) bool {
	s, ok := value.(string)
	return ok && wholeExpression(s)
}

func resolveInteger(expr string, ec expressionContext, low, high int) (int, error) {
	var value any
	var err error
	atom := ""
	if !ec.Literal && wholeExpression(expr) {
		source := strings.TrimSpace(expr)
		atom = strings.TrimSpace(source[3 : len(source)-2])
	}
	if decimalNumberPattern.MatchString(atom) {
		// Integer controls retain literal digits; general expressions keep
		// their established floating-point evaluation semantics.
		value = json.Number(atom)
	} else if _, numericErr := strconv.ParseFloat(atom, 64); numericErr == nil {
		return 0, fmt.Errorf("integer-control numeric literals must use decimal notation")
	} else {
		value, err = renderString(expr, ec)
	}
	if err != nil {
		return 0, err
	}
	if _, stringValue := value.(string); !stringValue {
		if n, ok := parseSchemaDecimal(value); ok {
			if v, ok := n.boundedInt(); ok && v >= low && v <= high {
				return v, nil
			}
		}
	}
	return 0, fmt.Errorf("expression must resolve to an integer between %d and %d", low, high)
}

// Keep literal decoding compatible with yaml.v3, while retaining expression
// source separately in immutable engine snapshots.
func (r *Repeat) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("repeat must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		switch n.Content[i].Value {
		case "max-attempts":
			if v.Tag == "!!str" && wholeExpression(v.Value) {
				r.MaxAttemptsExpression = v.Value
			} else if err := v.Decode(&r.MaxAttempts); err != nil {
				return err
			}
		case "until":
			if err := v.Decode(&r.Until); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r Repeat) MarshalYAML() (any, error) {
	var limit any = r.MaxAttempts
	if r.MaxAttemptsExpression != "" {
		limit = r.MaxAttemptsExpression
	}
	return map[string]any{"max-attempts": limit, "until": r.Until}, nil
}
func (f *ForEach) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("for-each must be a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		switch n.Content[i].Value {
		case "items":
			if err := v.Decode(&f.Items); err != nil {
				return err
			}
		case "key":
			if err := v.Decode(&f.Key); err != nil {
				return err
			}
		case "concurrency":
			if v.Tag == "!!str" && wholeExpression(v.Value) {
				f.ConcurrencyExpression = v.Value
			} else if err := v.Decode(&f.Concurrency); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f ForEach) MarshalYAML() (any, error) {
	out := map[string]any{"items": f.Items, "key": f.Key}
	if f.ConcurrencyExpression != "" {
		out["concurrency"] = f.ConcurrencyExpression
	} else if f.Concurrency != nil {
		out["concurrency"] = *f.Concurrency
	}
	return out, nil
}

// Ownership coordinates change for each execution; implementation/configuration
// identity does not. Never expose runtime configuration or credentials to helpers.
func admittedFingerprint(all map[string]*Workflow, roles json.RawMessage, execution runtimeexec.Policy, packages ...*PackageBinding) string {
	b, _ := json.Marshal(all)
	var definitions map[string]*Workflow
	_ = decodeJSONWithNumbers(b, &definitions)
	clearOwner := func(implementation *ContainerImplementation) {
		if implementation != nil && implementation.Bundle != nil {
			implementation.Bundle.OriginID = ""
		}
	}
	for _, w := range definitions {
		for id, job := range w.Jobs {
			if job.Container != nil {
				clearOwner(job.Container.Implementation)
			}
			job.Steps, _ = mapSteps(job.Steps, "", func(step Step) (Step, error) {
				clearOwner(step.ContainerImplementation)
				for _, binding := range step.SkillAdmissions {
					if binding != nil && binding.Assets != nil {
						binding.Assets.OriginID = ""
					}
				}
				if step.Skill != nil && step.Skill.Binding.Assets != nil {
					step.Skill.Binding.Assets.OriginID = ""
				}
				return step, nil
			})
			w.Jobs[id] = job
		}
	}
	if len(packages) > 0 && packages[0] != nil {
		b := *packages[0]
		b.Assets = nil
		return actionHash([]any{definitions, roles, execution, b})
	}
	return actionHash([]any{definitions, roles, execution})
}

func hasExpressionRepeat(all map[string]*Workflow) bool {
	for _, w := range all {
		for _, j := range w.Jobs {
			if j.Repeat != nil && j.Repeat.MaxAttemptsExpression != "" {
				return true
			}
		}
	}
	return false
}
func repeatBoundPlan(run, job string, repeat *Repeat, bound int) IterationPlan {
	return IterationPlan{RunID: run, JobID: job, ResolvedBound: bound, Fingerprint: actionHash([]any{repeat, bound})}
}
