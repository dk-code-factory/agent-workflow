package workflows

import (
	"fmt"
	"regexp"
	"strings"
)

var quotedResultOperandPattern = regexp.MustCompile(`^(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')$`)

func validateResultReferences(w *Workflow) error {
	check := func(value any, field, jobID string, available int, bare bool) error {
		return walkResultReferences(value, bare, func(parts []string) error {
			if !newResultReference(parts) {
				return nil
			}
			fail := func(reason string) error { return fmt.Errorf("%s: %s: %s", field, strings.Join(parts, "."), reason) }
			producer, step, limit := jobID, "", available
			switch parts[0] {
			case "steps":
				if jobID == "" || available == 0 {
					return fail("local step results are unavailable here")
				}
				step = parts[1]
			case "needs", "jobs":
				producer = parts[1]
				if parts[0] == "jobs" && jobID != "" {
					return fail("jobs results are only available in workflow_call outputs; use a declared needs dependency")
				}
				if parts[0] == "needs" {
					declared := false
					for _, dependency := range w.Jobs[jobID].Needs {
						declared = declared || dependency == producer
					}
					if jobID == "" || !declared {
						return fail("job is not a declared needs dependency")
					}
				}
				if len(parts) < 5 {
					return fail("step id and result field are required")
				}
				step = parts[3]
				limit = len(w.Jobs[producer].Steps)
			}
			job, ok := w.Jobs[producer]
			if !ok {
				return fail("producer job not found")
			}
			if job.Uses != "" {
				return fail("reusable workflow steps are private; use its declared outputs")
			}
			for index, candidate := range job.Steps {
				if stepID(candidate, index) == step {
					if index >= limit {
						return fail("step result is unavailable before that step completes")
					}
					return nil
				}
			}
			return fail("producer step not found")
		})
	}
	for id, job := range w.Jobs {
		prefix := "jobs." + id
		for field, value := range map[string]any{"if": job.If, "with": job.With, "secrets": job.Secrets} {
			if err := check(value, prefix+"."+field, id, 0, field == "if"); err != nil {
				return err
			}
		}
		if job.ForEach != nil {
			if text, ok := job.ForEach.Items.(string); ok {
				if err := check(text, prefix+".for-each.items", id, 0, false); err != nil {
					return err
				}
			}
			if err := check(job.ForEach.Key, prefix+".for-each.key", id, 0, true); err != nil {
				return err
			}
		}
		for index, source := range job.Steps {
			step, err := normalizeAgentStep(source)
			if err != nil {
				return err
			}
			for field, value := range map[string]any{"if": step.If, "with": step.With, "argv": step.Argv, "run": step.Run, "working-directory": step.WorkingDirectory} {
				if err := check(value, fmt.Sprintf("%s.steps[%d].%s", prefix, index, field), id, index, field == "if"); err != nil {
					return err
				}
			}
		}
		for name, value := range job.Outputs {
			if err := check(value, prefix+".outputs."+name, id, len(job.Steps), false); err != nil {
				return err
			}
		}
		if job.Repeat != nil {
			if err := check(job.Repeat.Until, prefix+".repeat.until", id, len(job.Steps), true); err != nil {
				return err
			}
		}
	}
	if w.On.WorkflowCall != nil {
		for name, output := range w.On.WorkflowCall.Outputs {
			if err := check(output.Value, "on.workflow_call.outputs."+name+".value", "", 0, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func newResultReference(parts []string) bool {
	return len(parts) >= 3 && (parts[0] == "steps" && !legacyStepField(parts[2]) || (parts[0] == "needs" || parts[0] == "jobs") && parts[2] == "steps")
}

func walkResultReferences(value any, bare bool, check func([]string) error) error {
	switch value := value.(type) {
	case string:
		bodies := []string{}
		if bare && !strings.HasPrefix(strings.TrimSpace(value), "${{") {
			bodies = append(bodies, value)
		} else {
			for _, match := range expressionPattern.FindAllStringSubmatch(value, -1) {
				bodies = append(bodies, match[1])
			}
		}
		for _, body := range bodies {
			if err := walkResultExpression(body, check); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range value {
			if err := walkResultReferences(item, false, check); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range value {
			if err := walkResultReferences(item, false, check); err != nil {
				return err
			}
		}
	case []string:
		for _, item := range value {
			if err := walkResultReferences(item, false, check); err != nil {
				return err
			}
		}
	}
	return nil
}

// Follow expression operands instead of restricting structured object keys to identifiers.
func walkResultExpression(expression string, check func([]string) error) error {
	expression = strings.TrimSpace(expression)
	if terms, ok := splitExpressionLogicalAND(expression); ok {
		for _, term := range terms {
			if err := walkResultExpression(term, check); err != nil {
				return err
			}
		}
		return nil
	}
	if quotedResultOperandPattern.MatchString(expression) {
		return nil
	}
	for _, operator := range []string{" == ", " != ", " >= ", " <= ", " > ", " < "} {
		if index := strings.Index(expression, operator); index >= 0 {
			if err := walkResultExpression(expression[:index], check); err != nil {
				return err
			}
			return walkResultExpression(expression[index+len(operator):], check)
		}
	}
	if strings.HasPrefix(expression, "not ") {
		return walkResultExpression(strings.TrimPrefix(expression, "not "), check)
	}
	return check(strings.Split(expression, "."))
}
