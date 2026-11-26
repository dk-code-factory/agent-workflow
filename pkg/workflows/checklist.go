package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var errChecklistExhausted = errors.New("checklist retry budget exhausted")

type checklistCommandKey struct{}

func checklistCommandFailed(result StepExecution) bool {
	code, numeric := parseSchemaDecimal(result.Outputs["exit_code"])
	exit, integer := code.boundedInt()
	return numeric && integer && exit > 0 && result.FailureClass == "command" && result.UnsafeFailureClass == ""
}

type checklistStage struct {
	Prompt  string
	Command Step
	Retries int
}

type agentChecklist struct {
	Retries int
	Stages  []checklistStage
}

func hasChecklist(step Step) bool {
	_, present := step.With["checklist"]
	return present
}

func checklistRetries(value any) (int, error) {
	if _, text := value.(string); !text {
		if number, ok := parseSchemaDecimal(value); ok {
			if n, ok := number.boundedInt(); ok && n >= 0 && n <= 100 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("checklist max-retries must be an integer from 0 to 100")
}

// Limits and stage shape are literal admitted policy. Prompt and command strings
// retain the ordinary workflow expression semantics.
func parseChecklist(value any) (agentChecklist, error) {
	var result agentChecklist
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return result, fmt.Errorf("checklist requires max-retries and stages")
	}
	var err error
	result.Retries, err = checklistRetries(object["max-retries"])
	if err != nil {
		return result, err
	}
	stages, ok := object["stages"].([]any)
	if !ok || len(stages) == 0 || len(stages) > 100 {
		return result, fmt.Errorf("checklist requires 1–100 stages")
	}
	for i, raw := range stages {
		stage := checklistStage{Retries: result.Retries}
		if prompt, ok := raw.(string); ok {
			raw = map[string]any{"prompt": prompt}
		}
		fields, ok := raw.(map[string]any)
		if !ok {
			return result, fmt.Errorf("checklist stage %d requires a prompt or command", i+1)
		}
		for key, value := range fields {
			switch key {
			case "max-retries":
				stage.Retries, err = checklistRetries(value)
			case "prompt", "run", "shell", "working-directory", "timeout":
				text, ok := value.(string)
				if !ok || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
					err = fmt.Errorf("%s must be a nonempty UTF-8 string", key)
					break
				}
				switch key {
				case "prompt":
					stage.Prompt = text
				case "run":
					stage.Command.Run = text
				case "shell":
					stage.Command.Shell = text
				case "working-directory":
					stage.Command.WorkingDirectory = text
				case "timeout":
					stage.Command.Timeout = text
				}
			case "argv":
				args, ok := value.([]any)
				if !ok || len(args) == 0 {
					err = fmt.Errorf("argv must be a nonempty string array")
					break
				}
				for _, arg := range args {
					text, ok := arg.(string)
					if !ok || !utf8.ValidString(text) {
						err = fmt.Errorf("argv values must be UTF-8 strings")
						break
					}
					stage.Command.Argv = append(stage.Command.Argv, text)
				}
				if len(stage.Command.Argv) > 0 && strings.TrimSpace(stage.Command.Argv[0]) == "" {
					err = fmt.Errorf("argv requires an executable")
				}
			default:
				err = fmt.Errorf("unknown checklist stage field %q", key)
			}
			if err != nil {
				return result, fmt.Errorf("checklist stage %d: %w", i+1, err)
			}
		}
		forms := 0
		if stage.Prompt != "" {
			forms++
		}
		if stage.Command.Run != "" {
			forms++
		}
		if len(stage.Command.Argv) > 0 {
			forms++
		}
		if forms != 1 || (stage.Command.Run != "") != (stage.Command.Shell != "") {
			return result, fmt.Errorf("checklist stage %d requires exactly one of prompt, argv, or run with shell", i+1)
		}
		if stage.Prompt != "" && (stage.Command.Timeout != "" || stage.Command.WorkingDirectory != "") {
			return result, fmt.Errorf("checklist prompt stage %d cannot declare command settings", i+1)
		}
		if stage.Command.Timeout != "" {
			d, err := time.ParseDuration(stage.Command.Timeout)
			if err != nil || d <= 0 {
				return result, fmt.Errorf("checklist stage %d timeout must be a positive duration", i+1)
			}
		}
		result.Stages = append(result.Stages, stage)
	}
	return result, nil
}

func checklistOutput() map[string]any {
	return map[string]any{"format": "json", "repair-attempts": 0, "schema": map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"complete", "evidence", "remaining"},
		"properties": map[string]any{"complete": map[string]any{"type": "boolean"},
			"evidence":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"remaining": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}},
	}}
}

// Each initial turn, check and repair has a stable child coordinate. Replaying
// their committed receipts reconstructs both budgets without replenishing them.
func (e *Executor) executeChecklist(ctx context.Context, step Step, ec expressionContext, slots chan struct{}, retry bool) (map[string]any, error) {
	config, err := parseChecklist(step.With["checklist"])
	if err != nil {
		return nil, err
	}
	initial := Step{ID: step.ID + "/checklist/task", Uses: "agent/run", With: cloneJSONMap(step.With), Definition: step.Definition, Skill: step.Skill, Definitions: step.Definitions}
	delete(initial.With, "checklist")
	args, err := renderMap(initial.With, ec)
	if err != nil {
		return nil, err
	}
	local := ec
	local.Attempt = nil
	runAgent := func(child Step) (StepExecution, error) {
		child.ContinueOnError = step.ContinueOnError
		checked, checkErr := e.executeLifecycleStep(ctx, child, local, slots, retry, true)
		if checkErr != nil && ec.Attempt != nil {
			ec.Attempt.ChecklistFailure = checked.AttemptID
		}
		return checked, checkErr
	}
	result, err := runAgent(initial)
	if err != nil {
		return result.Outputs, err
	}
	outputs := cloneJSONMap(result.Outputs)
	agent := nativeString(outputs, "agent")
	owner := nativeString(ec.Metadata, "origin_run_id")
	if owner == "" {
		owner = ec.RunID
	}
	worker, err := e.Workers.Get(ctx, owner, agent)
	if err != nil {
		return outputs, err
	}
	workspace := worker.TaskWorkspace
	if workspace == "" {
		workspace = e.WorkspaceDir
	}
	used := 0
	reports := []any{}
	report := map[string]any{"complete": false, "retries": used, "stages": reports}
	outputs["checklist"] = report
	for i, stage := range config.Stages {
		entry := map[string]any{"number": i + 1, "complete": false, "retries": 0, "attempts": []any{}}
		reports = append(reports, entry)
		report["stages"] = reports
		var feedback any
		for round := 0; ; round++ {
			if round > 0 {
				if round > stage.Retries || used >= config.Retries {
					return outputs, fmt.Errorf("%w at stage %d (stage retries %d, global retries %d)", errChecklistExhausted, i+1, round-1, used)
				}
				used++
				report["retries"], entry["retries"] = used, round
			}
			local.Event = map[string]any{"type": "checklist", "stage": i + 1, "retry": round, "previous": redactValue(feedback, e.redactions(ec.Secrets)), "workspace": workspace, "model": nativeString(args, "model")}
			prefix := fmt.Sprintf("%s/checklist/stage_%d/attempt_%d", step.ID, i+1, round+1)
			continuation := func(id, prompt string, output any) Step {
				with := map[string]any{"agent": agent, "prompt": "${{ event.prompt }}", "input": "${{ event }}"}
				// A literal opaque reference is internal; dispatch requires a whole
				// expression for public continuations, so bind it as event data.
				local.Event["agent"] = agent
				local.Event["prompt"] = prompt
				with["agent"] = "${{ event.agent }}"
				if nativeString(args, "model") != "" {
					with["model"] = "${{ event.model }}"
				}
				if output != nil {
					with["output"] = output
				}
				return Step{ID: id, Uses: "agent/run", With: with}
			}
			if stage.Prompt == "" && round > 0 {
				repair := continuation(prefix+"/repair", "The checklist command failed. Inspect the previous command result supplied as input, fix the underlying problem, and report what you changed. The framework will rerun the command to verify the repair.", nil)
				if _, err := runAgent(repair); err != nil {
					return outputs, err
				}
			}
			check := stage.Command
			check.ID = prefix + "/check"
			if stage.Prompt != "" {
				prompt, err := renderString(stage.Prompt, ec)
				if err != nil {
					return outputs, err
				}
				text, ok := prompt.(string)
				if !ok {
					return outputs, fmt.Errorf("checklist prompt must render to a string")
				}
				check = continuation(check.ID, text+"\nComplete this checklist stage, fixing missing work where possible. Return complete, concrete evidence, and remaining unfinished work. Set complete to true only when the whole stage is satisfied.", checklistOutput())
			} else if check.WorkingDirectory == "" {
				check.WorkingDirectory = "${{ private.checklist_workspace }}"
			}
			var checked StepExecution
			var checkErr error
			if stage.Prompt == "" {
				// User commands retain the incoming workflow expression context;
				// internal continuation input is bound in a separate context.
				commandContext := ec
				commandContext.Attempt = nil
				commandContext.Private = cloneJSONMap(ec.Private)
				if commandContext.Private == nil {
					commandContext.Private = map[string]any{}
				}
				commandContext.Private["checklist_workspace"] = workspace
				checked, checkErr = e.executeChecklistCommand(ctx, check, commandContext, slots, retry)
			} else {
				checked, checkErr = runAgent(check)
			}
			entry["attempts"] = append(entry["attempts"].([]any), map[string]any{
				"attempt_id": checked.AttemptID, "status": checked.Status, "result": checked.Outputs["structured"],
			})
			if checkErr != nil {
				// Only ordinary nonzero exits enter agent repair. Provider errors,
				// timeouts, cancellation, cleanup and storage failures remain errors.
				if stage.Prompt != "" || !checklistCommandFailed(checked) || hardExecutionFailure(checkErr) || ctx.Err() != nil {
					return outputs, checkErr
				}
				feedback = checklistCommandFeedback(check, checked)
				continue
			}
			if stage.Prompt != "" {
				complete, err := completionValue(checked.Outputs)
				if err != nil {
					return outputs, err
				}
				feedback = checked.Outputs["structured"]
				if !complete {
					continue
				}
			}
			entry["complete"] = true
			break
		}
	}
	report["complete"] = true
	return outputs, nil
}

func (e *Executor) executeChecklistCommand(ctx context.Context, step Step, ec expressionContext, slots chan struct{}, retry bool) (StepExecution, error) {
	return e.executeLifecycleStep(context.WithValue(ctx, checklistCommandKey{}, true), step, ec, slots, retry, false)
}

func checklistCommandFeedback(command Step, result StepExecution) map[string]any {
	// Keep diagnostics well within the native mailbox limit; full streams remain
	// in the command's durable artifacts. Data is bound once through event input.
	clip := func(value string) string {
		if len(value) > 32768 {
			return value[:32768] + "\n[truncated; see command artifacts]"
		}
		return value
	}
	return map[string]any{"argv": command.Argv, "run": command.Run, "shell": command.Shell,
		"exit_code": result.Outputs["exit_code"], "stdout": clip(nativeString(result.Outputs, "stdout")),
		"stderr": clip(nativeString(result.Outputs, "stderr")), "error": clip(result.Error)}
}
