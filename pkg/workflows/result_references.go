package workflows

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

func validateResultReferences(w *Workflow) error {
	stepIndexes := make(map[string]map[string]bool, len(w.Jobs))
	dependencies := make(map[string]map[string]bool, len(w.Jobs))
	for id, job := range w.Jobs {
		indexes := make(map[string]bool)
		for _, step := range FlattenSteps(job.Steps) {
			indexes[step.ID] = true
		}
		stepIndexes[id] = indexes
		declared := make(map[string]bool, len(job.Needs))
		for _, dependency := range job.Needs {
			declared[dependency] = true
		}
		dependencies[id] = declared
	}
	check := func(value any, field, jobID string, available map[string]bool, bare bool) error {
		return walkResultReferences(value, bare, func(parts []string) error {
			if !newResultReference(parts) && !(len(parts) >= 2 && parts[0] == "steps" && hasStepBlocks(w.Jobs[jobID].Steps)) {
				return nil
			}
			fail := func(reason string) error { return fmt.Errorf("%s: %s: %s", field, strings.Join(parts, "."), reason) }
			producer, step, limit := jobID, "", available
			switch parts[0] {
			case "steps":
				if jobID == "" || len(available) == 0 {
					return fail("local step results are unavailable here")
				}
				step = parts[1]
			case "needs", "jobs":
				producer = parts[1]
				if parts[0] == "jobs" && jobID != "" {
					return fail("jobs results are only available in workflow_call outputs; use a declared needs dependency")
				}
				if parts[0] == "needs" {
					if jobID == "" || !dependencies[jobID][producer] {
						return fail("job is not a declared needs dependency")
					}
				}
				if len(parts) < 5 {
					return fail("step id and result field are required")
				}
				step = parts[3]
				limit = stepIndexes[producer]
			}
			job, ok := w.Jobs[producer]
			if !ok {
				return fail("producer job not found")
			}
			if job.Uses != "" {
				return fail("reusable workflow steps are private; use its declared outputs")
			}
			if stepIndexes[producer][step] {
				if !limit[step] {
					return fail("step result is unavailable before that step completes")
				}
				return nil
			}
			return fail("producer step not found")
		})
	}
	for id, job := range w.Jobs {
		prefix := "jobs." + id
		for field, value := range map[string]any{"if": job.If, "with": job.With, "secrets": job.Secrets} {
			if err := check(value, prefix+"."+field, id, nil, field == "if"); err != nil {
				return err
			}
		}
		if job.ForEach != nil {
			if text, ok := job.ForEach.Items.(string); ok {
				if err := check(text, prefix+".for-each.items", id, nil, false); err != nil {
					return err
				}
			}
			if err := check(job.ForEach.Key, prefix+".for-each.key", id, nil, true); err != nil {
				return err
			}
		}
		var handlers func([]Step, map[string]bool, string) error
		handlers = func(steps []Step, inherited map[string]bool, path string) error {
			local := copyStepScope(inherited)
			for index, source := range steps {
				sid := stepID(source, index)
				step, err := resolveReferenceStep(w, source)
				if err != nil {
					return fmt.Errorf("%s.%s.with.output: %w", path, sid, err)
				}
				for field, value := range map[string]any{"if": step.If, "with": step.With, "argv": step.Argv, "run": step.Run, "working-directory": step.WorkingDirectory} {
					if err := walkResultReferences(value, field == "if", func(parts []string) error {
						if len(parts) > 1 && parts[0] == "steps" {
							if !local[parts[1]] {
								return fmt.Errorf("%s.%s: step result %s unavailable before completion", path, field, parts[1])
							}
							return nil
						}
						if len(parts) > 0 && (parts[0] == "needs" || parts[0] == "jobs") {
							return check("${{ "+strings.Join(parts, ".")+" }}", path+"."+field, id, nil, false)
						}
						return nil
					}); err != nil {
						return err
					}
				}
				for _, entry := range []struct {
					name  string
					steps []Step
				}{{"onError", step.OnError}, {"onFailure", step.OnFailure}, {"finally", step.Finally}} {
					if err := handlers(entry.steps, local, path+"."+sid+"."+entry.name); err != nil {
						return err
					}
				}
				local[sid] = true
			}
			return nil
		}
		var inspect func([]Step, map[string]bool) error
		inspect = func(steps []Step, available map[string]bool) error {
			for _, source := range steps {
				sid := source.ID
				step, err := normalizeAgentStep(source)
				if err != nil {
					return err
				}
				with := step.With
				if strings.HasPrefix(step.Uses, "agent/") && step.With["output"] != nil {
					output, err := resolveNamedOutput(w, step.With["output"])
					if err != nil {
						return fmt.Errorf("%s.steps.%s.with.output: %w", prefix, sid, err)
					}
					with = make(map[string]any, len(step.With))
					for key, value := range step.With {
						with[key] = value
					}
					with["output"] = output
				}
				for field, value := range map[string]any{"if": step.If, "with": with, "argv": step.Argv, "run": step.Run, "working-directory": step.WorkingDirectory} {
					if err := check(value, fmt.Sprintf("%s.steps.%s.%s", prefix, sid, field), id, available, field == "if"); err != nil {
						return err
					}
				}
				for _, entry := range []struct {
					name  string
					steps []Step
				}{{"onError", step.OnError}, {"onFailure", step.OnFailure}, {"finally", step.Finally}} {
					if err := handlers(entry.steps, available, prefix+".steps."+sid+"."+entry.name); err != nil {
						return err
					}
				}
				if source.Parallel != nil {
					for _, branch := range source.Parallel {
						branchAvailable := copyStepScope(available)
						if err := inspect([]Step{branch}, branchAvailable); err != nil {
							return err
						}
					}
					for _, descendant := range FlattenSteps(source.Parallel) {
						available[descendant.ID] = true
					}
				} else if source.Sequence != nil {
					if err := inspect(source.Sequence, available); err != nil {
						return err
					}
				}
				available[sid] = true
			}
			return nil
		}
		if err := inspect(identifiedSteps(job.Steps, ""), map[string]bool{}); err != nil {
			return err
		}
		for name, value := range job.Outputs {
			if err := check(value, prefix+".outputs."+name, id, stepIndexes[id], false); err != nil {
				return err
			}
		}
		if job.Repeat != nil {
			if err := check(job.Repeat.Until, prefix+".repeat.until", id, stepIndexes[id], true); err != nil {
				return err
			}
		}
	}
	if w.On.WorkflowCall != nil {
		for name, output := range w.On.WorkflowCall.Outputs {
			if err := check(output.Value, "on.workflow_call.outputs."+name+".value", "", nil, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func resolveReferenceStep(w *Workflow, source Step) (Step, error) {
	step, err := normalizeAgentStep(source)
	if err != nil {
		return step, err
	}
	if strings.HasPrefix(step.Uses, "agent/") && step.With["output"] != nil {
		output, err := resolveNamedOutput(w, step.With["output"])
		if err != nil {
			return step, err
		}
		with := make(map[string]any, len(step.With))
		for key, value := range step.With {
			with[key] = value
		}
		with["output"] = output
		step.With = with
	}
	return step, nil
}

func newResultReference(parts []string) bool {
	return len(parts) >= 3 && (parts[0] == "steps" && parts[2] != "" && !legacyStepField(parts[2]) || (parts[0] == "needs" || parts[0] == "jobs") && parts[2] == "steps")
}

func walkResultReferences(value any, bare bool, check func([]string) error) error {
	switch value := value.(type) {
	case string:
		bodies := []string{}
		if bare {
			body := strings.TrimSpace(value)
			if strings.HasPrefix(body, "${{") && strings.HasSuffix(body, "}}") {
				body = strings.TrimSpace(body[3 : len(body)-2])
			}
			bodies = append(bodies, body)
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
	operators := []string{" == ", " != ", " >= ", " <= ", " > ", " < "}
	positions := make([][]int, len(operators))
	for index, operator := range operators {
		for cursor := 0; cursor < len(expression); {
			found := strings.Index(expression[cursor:], operator)
			if found < 0 {
				break
			}
			cursor += found
			positions[index] = append(positions[index], cursor)
			cursor++
		}
	}
	type boundary struct {
		offset  int
		quote   int
		escaped bool
	}
	var boundaries []boundary
	backslashes := 0
	for offset := 0; offset < len(expression); offset++ {
		character := expression[offset]
		switch {
		case character == '\'' || character == '"':
			quote := 1
			if character == '"' {
				quote = 2
			}
			boundaries = append(boundaries, boundary{offset, quote, backslashes%2 == 1})
		case character == ' ' && strings.HasPrefix(expression[offset:], " and "):
			boundaries = append(boundaries, boundary{offset: offset})
		}
		if character == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}
	// Comparisons reset quote state when their operands are evaluated independently.
	var nextAnd [3][]int
	for state := range nextAnd {
		nextAnd[state] = make([]int, len(boundaries)+1)
		nextAnd[state][len(boundaries)] = -1
	}
	for index := len(boundaries) - 1; index >= 0; index-- {
		item := boundaries[index]
		for state := range nextAnd {
			if state == 0 && item.quote == 0 {
				nextAnd[state][index] = item.offset
				continue
			}
			nextState := state
			if item.quote != 0 && state == 0 {
				nextState = item.quote
			} else if item.quote == state && !item.escaped {
				nextState = 0
			}
			nextAnd[state][index] = nextAnd[nextState][index+1]
		}
	}
	type operand struct {
		start, end int
		scan       bool
	}
	pending := []operand{{0, len(expression), true}}
	for len(pending) > 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		body := expression[item.start:item.end]
		trimmed := strings.TrimSpace(body)
		item.start += len(body) - len(strings.TrimLeftFunc(body, unicode.IsSpace))
		item.end = item.start + len(trimmed)
		if item.scan {
			boundaryIndex := sort.Search(len(boundaries), func(index int) bool { return boundaries[index].offset >= item.start })
			found := nextAnd[0][boundaryIndex]
			width := 5
			if found < item.start || found+width > item.end {
				found = -1
				for index, offsets := range positions {
					position := sort.SearchInts(offsets, item.start)
					if position < len(offsets) && offsets[position]+len(operators[index]) <= item.end {
						found, width = offsets[position], len(operators[index])
						break
					}
				}
			}
			if found >= 0 {
				pending = append(pending, operand{found + width, item.end, true}, operand{item.start, found, true})
				continue
			}
		}
		if strings.HasPrefix(trimmed, "not ") {
			pending = append(pending, operand{item.start + 4, item.end, false})
			continue
		}
		if err := check(strings.Split(trimmed, ".")); err != nil {
			return err
		}
	}
	return nil
}
