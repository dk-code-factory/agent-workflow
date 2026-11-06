package workflowdef

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
)

func result(value any) NodeResult { return NodeResult{Value: value, Exports: map[string]any{}} }

func (r *Runtime) body(ctx context.Context, node, env map[string]any, path string, observer bool) (NodeResult, error) {
	var out NodeResult
	var err error
	switch {
	case node["fail"] != nil:
		failure := &Failure{Code: "workflow.failed", Message: "workflow explicitly failed", EffectState: "notStarted", Abort: true}
		if original := object(env["error"]); original != nil {
			failure.Code = text(original["code"])
			failure.Message = text(original["message"])
			failure.EffectState = text(original["effectState"])
		}
		if message, ok := node["fail"].(string); ok {
			failure.Message = message
		}
		return result(nil), failure
	case node["agent"] != nil || node["run"] != nil || node["action"] != nil:
		value, err := r.task(ctx, node, env, path, nil)
		return result(value), err
	case node["conversation"] != nil:
		value, err := r.conversation(ctx, node, env, path)
		return result(value), err
	case node["recover"] != nil:
		value, err := r.recoverAgent(ctx, node, env, path)
		return result(value), err
	case node["parallel"] != nil:
		children := list(node["parallel"])
		out, err = r.concurrent(ctx, len(children), integer(node["maxConcurrency"], r.maxParallel), func(ctx context.Context, i int) (NodeResult, error) {
			return r.node(ctx, object(children[i]), contextCopy(env), fmt.Sprintf("%s/parallel/%d", path, i), observer)
		})
	case node["if"] != nil:
		decision, err := r.planned(ctx, path+"/condition", func() (any, error) { return r.Definition.Condition(node["if"], env) })
		if err != nil {
			return result(nil), err
		}
		branch := "else"
		if decision == true {
			branch = "then"
		}
		out, err = r.sequence(ctx, list(node[branch]), env, path+"/"+branch, observer)
	case node["repeat"] != nil:
		initial, err := r.planned(ctx, path+"/initial", func() (any, error) {
			if value, ok := node["initial"]; ok {
				return Resolve(value, env)
			}
			return map[string]any{}, nil
		})
		if err != nil {
			return result(nil), err
		}
		previous, ok := initial.(map[string]any)
		if !ok {
			return result(nil), fault("binding.type", fmt.Errorf("repeat initial must be an object"))
		}
		for i := 0; i < integer(node["repeat"], 0); i++ {
			body := contextCopy(env)
			body["previous"] = clone(previous)
			body["run"] = maps.Clone(object(env["run"]))
			object(body["run"])["iteration"] = float64(i)
			prefix := fmt.Sprintf("%s/cycle/%d", path, i)
			cycle, err := r.sequence(ctx, list(node["steps"]), body, prefix, observer)
			if err != nil {
				return cycle, err
			}
			previous = clone(cycle.Exports).(map[string]any)
			if until, ok := node["until"]; ok {
				decision, err := r.planned(ctx, prefix+"/until", func() (any, error) { return r.Definition.Condition(until, body) })
				if err != nil {
					return result(nil), err
				}
				if decision == true {
					return result(cycle.Value), nil
				}
			} else if i+1 == integer(node["repeat"], 0) {
				return result(cycle.Value), nil
			}
		}
		return result(nil), fault("limit.iterations", fmt.Errorf("repeat exhausted its iteration limit"))
	case node["each"] != nil:
		planned, err := r.planned(ctx, path+"/items", func() (any, error) { return Select(text(node["each"]), env, false) })
		if err != nil {
			return result(nil), err
		}
		items, ok := planned.([]any)
		if !ok {
			return result(nil), fault("binding.type", fmt.Errorf("each requires an array"))
		}
		if len(items) > integer(node["maxItems"], 1000) {
			return result(nil), fault("limit.items", fmt.Errorf("each exceeds maxItems"))
		}
		out, err = r.concurrent(ctx, len(items), integer(node["maxConcurrency"], 1), func(ctx context.Context, i int) (NodeResult, error) {
			body := contextCopy(env)
			body["item"] = map[string]any{"index": float64(i), "value": clone(items[i])}
			child, err := r.sequence(ctx, list(node["steps"]), body, fmt.Sprintf("%s/item/%d", path, i), observer)
			return result(child.Value), err
		})
	case has(node, "value"):
		value, err := Resolve(node["value"], env)
		return result(value), err
	default:
		out, err = r.sequence(ctx, list(node["steps"]), env, path+"/steps", observer)
	}
	if err == nil {
		if value, ok := node["_return"]; ok {
			out.Value, err = Resolve(value, env)
		}
	}
	if err == nil {
		if schema, ok := node["output"]; ok {
			if validationErr := r.Definition.ValidateValue(schema, out.Value); validationErr != nil {
				err = fault("validation.output", validationErr)
			}
		}
	}
	return out, err
}

func has(node map[string]any, key string) bool { _, ok := node[key]; return ok }

func (r *Runtime) concurrent(ctx context.Context, count, limit int, invoke func(context.Context, int) (NodeResult, error)) (NodeResult, error) {
	if limit > r.maxParallel {
		limit = r.maxParallel
	}
	if limit < 1 {
		limit = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]NodeResult, count)
	failures := make([]error, count)
	slots := make(chan struct{}, limit)
	var group sync.WaitGroup
dispatch:
	for i := 0; i < count; i++ {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		group.Add(1)
		go func(i int) {
			defer group.Done()
			defer func() { <-slots }()
			results[i], failures[i] = invoke(ctx, i)
			if failures[i] != nil {
				cancel()
			}
		}(i)
	}
	group.Wait()
	var err error
	for _, failure := range failures {
		if failure != nil && !canceledOnly(failure) {
			err = errors.Join(err, failure)
		}
	}
	if err == nil {
		for _, failure := range failures {
			err = errors.Join(err, failure)
		}
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return result(nil), err
	}
	values := make([]any, count)
	exports := map[string]any{}
	for i, child := range results {
		values[i] = child.Value
		maps.Copy(exports, child.Exports)
	}
	return NodeResult{Value: values, Exports: exports}, nil
}

// Preserve joined cleanup/storage errors when a sibling was canceled.
func canceledOnly(err error) bool {
	if err == nil {
		return true
	}
	if f, ok := err.(*Failure); ok && (f.Code != "execution.canceled" || f.Cause == nil) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !canceledOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return canceledOnly(wrapped.Unwrap())
	}
	return err == context.Canceled
}
