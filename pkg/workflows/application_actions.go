package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

const RegisteredGoActionsV1 = "registered-go-actions-v1"

// Capabilities reports library contracts, independently of configured actions.
const OrderedDynamicIterationV1 = "ordered-dynamic-iteration-v1"

func Capabilities() []string { return []string{RegisteredGoActionsV1, OrderedDynamicIterationV1} }

type ActionEffect string

const (
	ActionReadOnly         ActionEffect = "read_only"
	ActionExternalMutation ActionEffect = "external_mutation"
)

// ActionContext identifies a physical attempt and a stable logical operation.
// OperationKey is unchanged by resume, reconciliation, or a new attempt.
type ActionContext struct {
	RunID        string `json:"run_id"`
	JobID        string `json:"job_id"`
	StepID       string `json:"step_id"`
	Iteration    int    `json:"iteration"`
	AttemptID    string `json:"attempt_id"`
	Attempt      int    `json:"attempt"`
	Generation   int64  `json:"generation"`
	OperationKey string `json:"operation_key"`
}

type ActionResult[O any] struct {
	Output   O               `json:"output"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
	Receipt  json.RawMessage `json:"receipt,omitempty"`
}

type ActionCheckpoint[I, O any] struct {
	Input    I
	Bindings json.RawMessage
	Result   ActionResult[O]
}

type ActionReconciliation[O any] struct {
	// Outcome must be completed, no_effect, or unknown. Proof is required for
	// completed/no_effect; it must identify application-owned evidence/receipts.
	Outcome string
	Proof   json.RawMessage
	Result  ActionResult[O]
}

// ActionSpec hooks are application code. Bindings, ValidateCheckpoint and
// Reconcile must be read-only. Implementation must change whenever executable
// semantics change. Credentials belong in application-owned clients/closures.
type ActionSpec[I, O any] struct {
	Implementation     string
	Effect             ActionEffect
	Handler            func(context.Context, ActionContext, I) (ActionResult[O], error)
	Bindings           func(context.Context, ActionContext, I) (json.RawMessage, error)
	ValidateCheckpoint func(context.Context, ActionContext, ActionCheckpoint[I, O]) error
	Reconcile          func(context.Context, ActionContext, ActionCheckpoint[I, O]) (ActionReconciliation[O], error)
}

type ActionContract struct {
	Name           string          `json:"name"`
	Implementation string          `json:"implementation"`
	Effect         ActionEffect    `json:"effect"`
	InputSchema    json.RawMessage `json:"input_schema"`
	OutputSchema   json.RawMessage `json:"output_schema"`
	Fingerprint    string          `json:"fingerprint"`
}

// ActionValue is the serialized result retained by an ActionJournal.
// Application handlers use the typed ActionResult instead.
type ActionValue struct {
	Output   json.RawMessage `json:"output,omitempty"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
	Receipt  json.RawMessage `json:"receipt,omitempty"`
}
type actionReconciliation struct {
	Outcome string
	Proof   json.RawMessage
	Result  ActionValue
}
type registeredAction struct {
	contract      ActionContract
	input, output *jsonschema.Resolved
	bindings      func(context.Context, ActionContext, json.RawMessage) (json.RawMessage, error)
	handler       func(context.Context, ActionContext, json.RawMessage) (ActionValue, error)
	validate      func(context.Context, ActionContext, json.RawMessage, json.RawMessage, ActionValue) error
	reconcile     func(context.Context, ActionContext, json.RawMessage, json.RawMessage, ActionValue) (actionReconciliation, error)
}

// ActionRegistry is safe for concurrent registration. First execution freezes
// it permanently; configure a new registry to admit different contracts.
type ActionRegistry struct {
	mu      sync.Mutex
	frozen  bool
	actions map[string]*registeredAction
}

func RegisterAction[I, O any](r *ActionRegistry, name string, spec ActionSpec[I, O]) error {
	if r == nil {
		return fmt.Errorf("action registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("action registry is frozen")
	}
	if !idPattern.MatchString(name) {
		return fmt.Errorf("invalid action name %q", name)
	}
	if r.actions[name] != nil {
		return fmt.Errorf("duplicate action %q", name)
	}
	if strings.TrimSpace(spec.Implementation) == "" || (spec.Effect != ActionReadOnly && spec.Effect != ActionExternalMutation) || spec.Handler == nil || spec.Bindings == nil || spec.ValidateCheckpoint == nil {
		return fmt.Errorf("action %s requires implementation identity, explicit effect, handler, bindings, and checkpoint validator", name)
	}
	in, ir, err := actionSchema[I]()
	if err != nil {
		return fmt.Errorf("action %s input: %w", name, err)
	}
	out, or, err := actionSchema[O]()
	if err != nil {
		return fmt.Errorf("action %s output: %w", name, err)
	}
	a := &registeredAction{contract: ActionContract{Name: name, Implementation: spec.Implementation, Effect: spec.Effect, InputSchema: in, OutputSchema: out}, input: ir, output: or}
	a.contract.Fingerprint = actionHash(a.contract)
	decode := func(input, bindings json.RawMessage, result ActionValue) (cp ActionCheckpoint[I, O], err error) {
		err = json.Unmarshal(input, &cp.Input)
		cp.Bindings = append(json.RawMessage(nil), bindings...)
		if err == nil && len(result.Output) > 0 {
			err = json.Unmarshal(result.Output, &cp.Result.Output)
		}
		cp.Result.Evidence = append(json.RawMessage(nil), result.Evidence...)
		cp.Result.Receipt = append(json.RawMessage(nil), result.Receipt...)
		return
	}
	a.bindings = func(ctx context.Context, ac ActionContext, input json.RawMessage) (json.RawMessage, error) {
		var i I
		if err := json.Unmarshal(input, &i); err != nil {
			return nil, err
		}
		return spec.Bindings(ctx, ac, i)
	}
	a.handler = func(ctx context.Context, ac ActionContext, input json.RawMessage) (ActionValue, error) {
		var i I
		if err := json.Unmarshal(input, &i); err != nil {
			return ActionValue{}, err
		}
		result, callErr := spec.Handler(ctx, ac, i)
		data, err := json.Marshal(result.Output)
		if err != nil {
			return ActionValue{}, fmt.Errorf("serialize action output: %w", err)
		}
		return ActionValue{data, result.Evidence, result.Receipt}, callErr
	}
	a.validate = func(ctx context.Context, ac ActionContext, input, bindings json.RawMessage, value ActionValue) error {
		cp, err := decode(input, bindings, value)
		if err != nil {
			return err
		}
		return spec.ValidateCheckpoint(ctx, ac, cp)
	}
	if spec.Reconcile != nil {
		a.reconcile = func(ctx context.Context, ac ActionContext, input, bindings json.RawMessage, value ActionValue) (actionReconciliation, error) {
			cp, err := decode(input, bindings, value)
			if err != nil {
				return actionReconciliation{}, err
			}
			r, err := spec.Reconcile(ctx, ac, cp)
			if err != nil {
				return actionReconciliation{}, err
			}
			data, err := json.Marshal(r.Result.Output)
			return actionReconciliation{r.Outcome, r.Proof, ActionValue{data, r.Result.Evidence, r.Result.Receipt}}, err
		}
	}
	if r.actions == nil {
		r.actions = map[string]*registeredAction{}
	}
	r.actions[name] = a
	return nil
}

func actionSchema[T any]() (json.RawMessage, *jsonschema.Resolved, error) {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, nil, err
	}
	if s.Type != "object" {
		return nil, nil, fmt.Errorf("Go type must describe a JSON object")
	}
	r, err := s.Resolve(nil)
	if err != nil {
		return nil, nil, err
	}
	b, err := json.Marshal(s)
	return b, r, err
}

func actionHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (r *ActionRegistry) freeze() map[string]*registeredAction {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
	out := map[string]*registeredAction{}
	for k, v := range r.actions {
		out[k] = v
	}
	return out
}

func isApplicationAction(uses string) bool { return strings.HasPrefix(uses, "action/") }

func (e *Executor) configureActions(all map[string]*Workflow) error {
	e.applicationActions = e.ApplicationActions.freeze()
	e.actionContracts = map[string]ActionContract{}
	for ref, w := range all {
		for job, j := range w.Jobs {
			for i, s := range j.Steps {
				if !isApplicationAction(s.Uses) {
					continue
				}
				name := strings.TrimPrefix(s.Uses, "action/")
				a := e.applicationActions[name]
				if a == nil {
					return fmt.Errorf("%s job %s step %s: missing registered Go action %q (%s)", ref, job, stepID(s, i), name, RegisteredGoActionsV1)
				}
				if actionJournal(e.Store) == nil {
					return fmt.Errorf("%s job %s step %s: registered actions require durable ActionJournal", ref, job, stepID(s, i))
				}
				e.actionContracts[name] = a.contract
			}
		}
	}
	return nil
}
