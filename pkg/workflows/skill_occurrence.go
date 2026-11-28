package workflows

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Skill occurrences use the immutable, fenced plan journal. The reserved job
// namespace cannot collide with YAML job IDs and requires no schema migration.
const skillOccurrencePrefix = "$skill/"

type skillOccurrence struct {
	Skill      *SkillInvocation `json:"skill"`
	Definition *AgentDefinition `json:"definition"`
}

func decodeSkillOccurrence(p IterationPlan) (skillOccurrence, error) {
	var occurrence skillOccurrence
	if len(p.Items) != 1 || p.Items[0].Key != "skill" || p.Items[0].Number != 1 {
		return occurrence, fmt.Errorf("%w: invalid skill occurrence", ErrActionIntegrity)
	}
	raw, err := json.Marshal(p.Items[0].Inputs)
	if err == nil {
		err = decodeJSONWithNumbers(raw, &occurrence)
	}
	if err != nil || occurrence.Skill == nil || occurrence.Skill.ID != skillPlanID(occurrence.Skill) || p.Fingerprint != actionHash(occurrence) {
		return occurrence, fmt.Errorf("%w: skill occurrence identity changed", ErrActionIntegrity)
	}
	return occurrence, nil
}

// bindSkillOccurrence runs before Begin/BeginInput. It consults captured bindings
// and occurrence inputs, never a skill directory or installed package.
func (e *Executor) bindSkillOccurrence(ctx context.Context, step Step, ec expressionContext) (Step, error) {
	if step.Uses != "agent/run" {
		return step, nil
	}
	// Step.With belongs to the shared definition closure. Pinning controls must
	// not rewrite another call's or iteration's input expressions.
	step.With = cloneJSONMap(step.With)
	var continued *Worker
	if reference, ok := step.With["agent"].(string); ok && wholeExpression(reference) {
		value, err := renderString(reference, ec)
		if err != nil {
			return step, err
		}
		if name, ok := value.(string); ok && strings.HasPrefix(name, "agent_") && e.Workers != nil {
			owner := nativeString(ec.Metadata, "origin_run_id")
			if owner == "" {
				owner = ec.RunID
			}
			worker, err := e.Workers.Get(ctx, owner, name)
			if err != nil {
				return step, err
			}
			continued = &worker
		}
	}
	if len(step.SkillAdmissions) != 0 {
		selection, err := parseSkillSelection(step.With["skill"])
		if err != nil {
			return step, err
		}
		binding := step.SkillAdmissions[actionHash(ec.Inputs[selection.Input])]
		if binding == nil {
			return step, fmt.Errorf("selected skill input %s is outside admitted entry inputs", selection.Input)
		}
		if len(step.Definitions) != 0 {
			agent, err := renderValue(step.With["agent"], ec)
			if err != nil {
				return step, err
			}
			selection, ok := agent.(map[string]any)
			if !ok {
				return step, fmt.Errorf("invalid admitted agent selection")
			}
			name, _ := selection["select"].(string)
			step.Definition = step.Definitions[name]
			if step.Definition == nil {
				return step, fmt.Errorf("unknown admitted agent definition selection: %s", name)
			}
		}
		if continued != nil {
			step.Definition = continued.Definition
		}
		if step.Definition == nil {
			return step, fmt.Errorf("skill task requires an admitted agent definition or session")
		}
		// Named output contracts were expanded by the loader.
		step, err = e.admitSkillInvocation(ctx, step, nil, binding, selection, ec, ec.Secrets)
		if err != nil {
			return step, err
		}
		if continued == nil {
			logical := nativeString(ec.Metadata, "iteration_logical_run_id")
			if logical == "" {
				logical = ec.RunID
			}
			step.Skill.Occurrence = actionHash([]any{logical, ec.JobID, step.ID, ec.IterationNumber, step.Definition})
		} else if continued.Skill != nil {
			step.Skill.Occurrence = continued.Skill.Occurrence
		}
		step.Skill.ID = skillPlanID(step.Skill)
	}
	if continued != nil && continued.Skill != nil {
		if step.Skill != nil && actionHash(step.Skill) != actionHash(continued.Skill) {
			return step, fmt.Errorf("%w: continued skill binding changed", ErrActionIntegrity)
		}
		step.Skill = continued.Skill
		step.Definition = continued.Definition
		if value, ok := step.With["workspace"]; !ok || value == nil {
			step.With["workspace"] = continued.TaskWorkspace
		}
		if value, ok := step.With["model"]; !ok || value == nil {
			step.With["model"] = continued.Skill.Model
		}
	}
	if continued != nil && step.Skill != nil {
		if continued.Skill == nil {
			return step, fmt.Errorf("%w: cannot add a skill to an existing session", ErrActionIntegrity)
		}
		args, err := renderMap(step.With, ec)
		if err != nil {
			return step, err
		}
		workspace := nativeString(args, "workspace")
		if workspace != "" && !filepath.IsAbs(workspace) {
			workspace = filepath.Join(e.WorkspaceDir, workspace)
		}
		if workspace != "" && workspace != continued.TaskWorkspace {
			return step, fmt.Errorf("%w: continued skill workspace changed", ErrActionIntegrity)
		}
		if model := nativeString(args, "model"); model != "" && model != continued.Skill.Model {
			return step, fmt.Errorf("%w: continued skill model changed", ErrActionIntegrity)
		}
	}
	if step.Skill == nil {
		return step, nil
	}
	journal := iterationJournal(e.Store)
	if journal == nil {
		return step, fmt.Errorf("skill occurrence requires a durable plan journal")
	}
	key := skillOccurrencePrefix + actionHash([]any{ec.JobID, step.ID, ec.IterationNumber})
	checkpoint, err := journal.IterationRun(ctx, ec.RunID, nativeString(ec.Metadata, "iteration_logical_run_id"), key)
	if err != nil {
		return step, err
	}
	occurrence := skillOccurrence{Skill: step.Skill, Definition: step.Definition}
	value, err := json.Marshal(occurrence)
	if err != nil {
		return step, err
	}
	var inputs map[string]any
	if err = decodeJSONWithNumbers(value, &inputs); err != nil {
		return step, err
	}
	plan := IterationPlan{RunID: checkpoint, JobID: key, Fingerprint: actionHash(occurrence), Items: []IterationPlanItem{{Key: "skill", Number: 1, Inputs: inputs}}}
	if err = journal.PinIteration(ctx, plan); err != nil {
		return step, err
	}
	if continued != nil {
		// Keep the source expression's session-only meaning at dispatch.
		step.Definition = nil
	}
	return step, nil
}

func (s *SQLiteStore) skillOccurrences(ctx context.Context, origin string) ([]skillOccurrence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.record FROM iteration_plans p JOIN runs r ON r.id=p.run_id WHERE (r.id=? OR r.origin_id=?) AND substr(p.job_id,1,?)=?`, origin, origin, len(skillOccurrencePrefix), skillOccurrencePrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []skillOccurrence
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var plan IterationPlan
		if err = decodeJSONWithNumbers(raw, &plan); err != nil {
			return nil, err
		}
		occurrence, err := decodeSkillOccurrence(plan)
		if err != nil {
			return nil, err
		}
		out = append(out, occurrence)
	}
	return out, rows.Err()
}

func (e *Executor) validateSkillOccurrences(ctx context.Context, origin string) error {
	store := invocationStore(e.Store)
	if store == nil {
		return nil
	}
	occurrences, err := store.skillOccurrences(ctx, origin)
	if err != nil {
		return err
	}
	for _, occurrence := range occurrences {
		if err := e.validateRetainedSkill(ctx, occurrence.Skill); err != nil {
			return err
		}
	}
	return nil
}

// A selected invocation must be both durably pinned and drawn from the run's
// immutable source projection. Merely knowing a bundle ID grants no access.
func (s *SQLiteStore) admittedSkillOccurrence(ctx context.Context, origin string, plan *SkillInvocation) error {
	occurrences, err := s.skillOccurrences(ctx, origin)
	if err != nil {
		return err
	}
	for _, occurrence := range occurrences {
		if actionHash(occurrence.Skill) == actionHash(plan) {
			return nil
		}
	}
	return errors.Join(ErrActionIntegrity, sql.ErrNoRows)
}
