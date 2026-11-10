package prfix

import (
	"context"
	"fmt"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type ciExecutionContextKey struct{}

// This small provenance pin distinguishes a live selected CI observation from
// the same checkpoint reused after an interrupted execution.
type CIExecution struct {
	Run        string `json:"run"`
	Generation int64  `json:"generation"`
}

func (r *Runtime) ciExecution(ctx context.Context) (*CIExecution, error) {
	ac, ok := ctx.Value(ciExecutionContextKey{}).(workflows.ActionContext)
	if !ok {
		return nil, nil
	}
	root, err := r.origin(ctx, ac)
	if err != nil {
		return nil, err
	}
	if ac.Generation <= 0 {
		return nil, fmt.Errorf("CI observation requires an admitted execution generation")
	}
	return &CIExecution{root, ac.Generation}, nil
}

func (r *Runtime) refreshResumedCI(ctx context.Context, p Plan, s Snapshot, saved CIObservation) (CIObservation, bool, error) {
	execution, err := r.ciExecution(ctx)
	if err != nil || execution == nil || digest(saved.Execution) == digest(execution) {
		return saved, false, err
	}
	bounded, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	current, err := r.provider().ObserveCI(bounded, p.Identity, s.Head)
	if err != nil {
		return current, false, err
	}
	current.Execution = execution
	changed := current.Head != saved.Head || digest(current.Checks) != digest(saved.Checks)
	if !changed {
		current.Verified = saved.Verified
	}
	path, err := artifact(r.Root, current)
	if err != nil {
		return current, changed, err
	}
	l, err := r.ledger()
	if err != nil {
		return current, changed, err
	}
	defer l.Close()
	_, err = l.db.ExecContext(ctx, "INSERT INTO progress(run,source,kind,reference) VALUES(?,?,?,?)", p.LogicalRun, s.Evidence, "ci:"+s.Head, path)
	return current, changed, err
}

// Checkpoint hooks only read existing evidence. A previously completed readiness
// result cannot serve as a fresh CI check in another execution generation.
func (r *Runtime) validateCompletionCI(ctx context.Context, p Plan, out ActionOutput) error {
	selected, _, _, err := r.selectedOccurrences(ctx, p)
	if err != nil {
		return err
	}
	ciSelected := false
	for _, o := range p.Occurrences {
		ciSelected = ciSelected || selected[o.ID] && o.Kind == "ci"
	}
	if !ciSelected {
		return nil
	}
	var report map[string]any
	if err := readJSON(out.Report, &report); err != nil {
		return err
	}
	head := stringValue(report, "head")
	path, err := r.latestCI(ctx, p, head)
	if err != nil {
		return err
	}
	var ci CIObservation
	if err := readJSON(path, &ci); err != nil {
		return err
	}
	execution, err := r.ciExecution(ctx)
	if err != nil {
		return err
	}
	if execution != nil {
		current, _, err := r.Store.Load(ctx, execution.Run)
		if err != nil {
			return err
		}
		execution.Generation = current.Generation
	}
	if execution == nil || digest(ci.Execution) != digest(execution) || ci.Head != head || !ci.Success() {
		return fmt.Errorf("completed readiness requires current-execution CI; start a fresh execution of the retained plan")
	}
	return nil
}
