package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func Redact(text string, secrets map[string]string) string {
	values := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			values = append(values, s)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, s := range values {
		text = strings.ReplaceAll(text, s, "[REDACTED]")
	}
	return text
}
func redactStep(step StepExecution, secrets map[string]string) StepExecution {
	step.Error = Redact(step.Error, secrets)
	step.Outputs, _ = redactValue(step.Outputs, secrets).(map[string]any)
	return step
}
func redactRun(run *Run, secrets map[string]string) *Run {
	safe := *run
	safe.Error = Redact(run.Error, secrets)
	safe.Outputs, _ = redactValue(run.Outputs, secrets).(map[string]any)
	safe.Jobs = map[string]JobExecution{}
	for id, job := range run.Jobs {
		job.Error = Redact(job.Error, secrets)
		job.Outputs, _ = redactValue(job.Outputs, secrets).(map[string]any)
		steps := map[string]StepExecution{}
		for sid, step := range job.Steps {
			steps[sid] = redactStep(step, secrets)
		}
		job.Steps = steps
		history := make([]JobIteration, len(job.Iterations))
		for i, iteration := range job.Iterations {
			iteration.Error = Redact(iteration.Error, secrets)
			iteration.Outputs, _ = redactValue(iteration.Outputs, secrets).(map[string]any)
			iteration.Steps = map[string]StepExecution{}
			for sid, step := range job.Iterations[i].Steps {
				iteration.Steps[sid] = redactStep(step, secrets)
			}
			history[i] = iteration
		}
		job.Iterations = history
		safe.Jobs[id] = job
	}
	return &safe
}
func redactValue(value any, secrets map[string]string) any {
	switch v := value.(type) {
	case string:
		return Redact(v, secrets)
	case map[string]any:
		if v == nil {
			return map[string]any(nil)
		}
		out := map[string]any{}
		for k, x := range v {
			out[Redact(k, secrets)] = redactValue(x, secrets)
		}
		return out
	case []any:
		if v == nil {
			return []any(nil)
		}
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = redactValue(x, secrets)
		}
		return out
	}
	return value
}

type redactedStore struct {
	Journal
	secrets map[string]string
}

func (s redactedStore) Save(ctx context.Context, r *Run) error {
	return s.Journal.Save(ctx, redactRun(r, s.secrets))
}
func (s redactedStore) Admit(ctx context.Context, r *Run, snap Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	var value any
	if err = decodeJSONWithNumbers(b, &value); err != nil {
		return err
	}
	safe, err := json.Marshal(redactValue(value, s.secrets))
	if err != nil {
		return err
	}
	// Definitions and nonsecret inputs must retain their exact meaning on resume.
	// Reject credential literals instead of silently modifying admitted resources.
	var original any
	if err = decodeJSONWithNumbers(b, &original); err != nil {
		return err
	}
	canonical, err := json.Marshal(original)
	if err != nil {
		return err
	}
	if string(canonical) != string(safe) {
		return fmt.Errorf("admission snapshot contains a credential literal; use secret/environment references")
	}
	return s.Journal.Admit(ctx, r, snap)
}
func (s redactedStore) Complete(ctx context.Context, a Attempt, r StepExecution) error {
	return s.Journal.Complete(ctx, redactAttempt(a, s.secrets), redactStep(r, s.secrets))
}

func RedactValue(value any, secrets map[string]string) any { return redactValue(value, secrets) }

type redactedError struct {
	cause error
	text  string
}

func (e redactedError) Error() string { return e.text }
func (e redactedError) Unwrap() error { return e.cause }
func RedactError(err error, secrets map[string]string) error {
	if err == nil {
		return nil
	}
	return redactedError{cause: err, text: Redact(err.Error(), secrets)}
}
func safeResult(run *Run, err error, secrets map[string]string) (*Run, error) {
	if run != nil {
		run = redactRun(run, secrets)
	}
	if err != nil {
		err = redactedError{cause: err, text: Redact(err.Error(), secrets)}
	}
	return run, err
}

func redactAttempt(a Attempt, secrets map[string]string) Attempt {
	a.TaskOutputs, _ = redactValue(a.TaskOutputs, secrets).(map[string]any)
	repairs := make([]map[string]any, len(a.RepairOutputs))
	for i, out := range a.RepairOutputs {
		repairs[i], _ = redactValue(out, secrets).(map[string]any)
	}
	a.RepairOutputs = repairs
	return a
}
func (s redactedStore) RecordAttempt(ctx context.Context, a Attempt) error {
	return s.Journal.RecordAttempt(ctx, redactAttempt(a, s.secrets))
}
