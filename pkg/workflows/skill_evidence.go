package workflows

import (
	"encoding/json"
	"fmt"
)

const MaxSkillObservations = 128
const MaxSkillEvidenceBytes = 4 << 20

// SkillObservation is adapter-produced evidence, never a model report. Raw
// preserves the original provider output/provenance after credential rejection.
type SkillObservation struct {
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	SessionID  string          `json:"session_id"`
	TurnID     string          `json:"turn_id,omitempty"`
	CallID     string          `json:"call_id,omitempty"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments,omitempty"`
	BodySHA256 string          `json:"body_sha256,omitempty"`
	Raw        json.RawMessage `json:"raw"`
}

type SkillEvidence struct {
	Children     []SkillChildObservation `json:"children,omitempty"`
	PlanID       string                  `json:"plan_id"`
	Observations []SkillObservation      `json:"observations"`
}

func (e *SkillEvidence) Observe(plan *SkillInvocation, observation SkillObservation) error {
	if plan == nil || plan.ID == "" || e.PlanID != "" && e.PlanID != plan.ID {
		return fmt.Errorf("skill evidence plan changed")
	}
	if observation.Key == "" || len(observation.Key) > 512 || observation.SessionID == "" || observation.Name != plan.Binding.Name || observation.Arguments != plan.Arguments || len(observation.Raw) == 0 || !json.Valid(observation.Raw) {
		return fmt.Errorf("skill evidence identity is incomplete or mismatched")
	}
	switch observation.Kind {
	case "invocation", "result":
		if observation.CallID == "" {
			return fmt.Errorf("skill invocation/result omitted original call identity")
		}
	case "consumption":
		matched := false
		if plan.Binding.Assets != nil {
			for _, entry := range plan.Binding.Assets.Entries {
				if entry.Path == plan.Binding.Entry && entry.SHA256 != "" && entry.SHA256 == observation.BodySHA256 {
					matched = true
				}
			}
		}
		if observation.TurnID == "" || !matched {
			return fmt.Errorf("skill consumption omitted original turn or differs from admitted bytes")
		}
	default:
		return fmt.Errorf("unsupported skill evidence kind %q", observation.Kind)
	}
	for _, old := range e.Observations {
		if old.SessionID != observation.SessionID {
			return fmt.Errorf("skill evidence changed original session")
		}
		if old.Key == observation.Key {
			if actionHash(old) != actionHash(observation) {
				return fmt.Errorf("conflicting repeated skill evidence")
			}
			return nil
		}
	}
	if len(e.Observations)+len(e.Children) >= MaxSkillObservations {
		return fmt.Errorf("skill evidence observation limit")
	}
	candidate := *e
	candidate.PlanID = plan.ID
	candidate.Observations = append(append([]SkillObservation(nil), e.Observations...), observation)
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	if len(data) > MaxSkillEvidenceBytes {
		return fmt.Errorf("skill evidence exceeds 4 MiB")
	}
	*e = candidate
	return nil
}

func (e *SkillEvidence) Complete(plan *SkillInvocation) error {
	if err := e.InvocationComplete(plan); err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	return e.completeChildren(plan)
}

func (e *SkillEvidence) InvocationComplete(plan *SkillInvocation) error {
	if plan == nil {
		return nil
	}
	if e == nil || e.PlanID != plan.ID {
		return fmt.Errorf("skill invocation has no observed evidence")
	}
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, o := range e.Observations {
		if o.Kind == "consumption" && o.BodySHA256 != "" {
			for _, entry := range plan.Binding.Assets.Entries {
				if entry.Path == plan.Binding.Entry && entry.SHA256 == o.BodySHA256 {
					return nil
				}
			}
		}
		if o.Kind == "invocation" && o.CallID != "" {
			calls[o.CallID] = true
		}
		if o.Kind == "result" && o.CallID != "" {
			results[o.CallID] = true
		}
	}
	for call := range calls {
		if results[call] {
			return nil
		}
	}
	return fmt.Errorf("skill invocation evidence remains incomplete")
}

// Continuation validates the original session's durable receipt. It never
// fabricates a new invocation event for a resumed provider process.
func (e *SkillEvidence) Continuation(plan *SkillInvocation, session string) error {
	if plan == nil || !plan.ContinuationRequiresEvidence || session == "" {
		return nil
	}
	if e == nil || e.PlanID != plan.ID || len(e.Observations) == 0 {
		return fmt.Errorf("skill continuation requires original observed invocation")
	}
	verified := &SkillEvidence{}
	for _, observation := range e.Observations {
		if observation.SessionID != session {
			return fmt.Errorf("skill continuation changed original session")
		}
		if err := verified.Observe(plan, observation); err != nil {
			return err
		}
	}
	for _, observation := range e.Children {
		if err := verified.ObserveChild(plan, observation); err != nil {
			return err
		}
	}
	return verified.Complete(plan)
}

func (w *Worker) validateSkillContinuation() error {
	if w.Skill != nil && w.Skill.ContinuationRequiresEvidence && w.TaskTurns > 0 && w.Remaining < w.TaskTurns && w.SessionID == "" {
		return fmt.Errorf("original skill session is uncertain after an admitted turn")
	}
	if err := w.SkillEvidence.Continuation(w.Skill, w.SessionID); err != nil {
		return err
	}
	if w.Skill != nil && w.SkillTurnUncertain {
		return fmt.Errorf("original selected-skill turn has uncertain effects; original terminal reconciliation is required")
	}
	return nil
}
