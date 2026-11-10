package prfix

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

type scopeAccounting struct {
	AddedLines   int    `json:"added_lines"`
	Threshold    int    `json:"threshold"`
	EvacuateMode string `json:"evacuate_unrelated"`
	RelatedMode  string `json:"split_related"`
	Enabled      bool   `json:"enabled"`
}

func (r *Runtime) scopeAccounting(ctx context.Context, p Plan, o Occurrence) (scopeAccounting, error) {
	s, e := r.live(ctx, p)
	if e != nil {
		return scopeAccounting{}, e
	}
	raw, e := gitRaw(ctx, r.Root, "diff", "--numstat", "-z", s.Base, "--")
	if e != nil {
		return scopeAccounting{}, e
	}
	a := scopeAccounting{Threshold: 1000, EvacuateMode: "automatic", RelatedMode: "automatic", Enabled: true}
	for _, row := range strings.Split(raw, "\x00") {
		fields := strings.SplitN(row, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "-" {
			continue
		}
		n, e := strconv.Atoi(fields[0])
		if e != nil {
			return a, e
		}
		a.AddedLines += n
	}
	saved, _ := p.Selection.Settings["pr_size"].(map[string]any)
	opts, _ := saved["pr_size"].(map[string]any)
	apply := func(values map[string]any) {
		if n, ok := values["added_lines_threshold"].(float64); ok {
			a.Threshold = int(n)
		}
		if v, ok := values["evacuate_unrelated"].(string); ok {
			a.EvacuateMode = v
		}
		if v, ok := values["split_related"].(string); ok {
			a.RelatedMode = v
		}
	}
	apply(opts)
	// The call's explicitly scoped/invocation options override legacy size inputs;
	// source defaults alone do not erase an existing legacy setting.
	if o.Call.Configuration != nil {
		values := map[string]any{}
		for k, v := range o.Call.Configuration.Options {
			origin := o.Call.Configuration.Provenance[k]
			if !strings.Contains(origin, "/codex/skills/") {
				values[k] = v
			}
		}
		apply(values)
	}
	policy, _ := p.Selection.Settings["partition"].(map[string]any)
	if enabled, ok := policy["enabled"].(bool); ok {
		a.Enabled = enabled
	}
	return a, nil
}
func (r *Runtime) validateScope(ctx context.Context, p Plan, o Occurrence, report *ScopeReport) error {
	a, e := r.scopeAccounting(ctx, p, o)
	if e != nil {
		return e
	}
	if report == nil || report.AddedLines != a.AddedLines || report.Threshold != a.Threshold || report.EvacuateMode != a.EvacuateMode || report.RelatedMode != a.RelatedMode || report.Enabled != a.Enabled || !validText(report.Assessment) || len(report.Evidence) == 0 {
		return fmt.Errorf("scope report must cover exact final-diff size, both effective modes, per-PR opt-out and sourced boundaries")
	}
	for _, ref := range report.Evidence {
		if e = r.validateSource(ctx, p, ref); e != nil {
			return e
		}
	}
	for _, b := range report.Boundaries {
		if !contains([]string{"evacuate_unrelated", "split_related"}, b.Behavior) || len(b.Paths) == 0 || !validText(b.Reason) || len(b.Evidence) == 0 || !validSHA(b.Head) || !validSHA(b.Base) || b.RecoveryRef == "" {
			return fmt.Errorf("split boundary needs ownership, exact objects, recovery and validation evidence")
		}
		if _, e = git(ctx, r.Root, "check-ref-format", "refs/heads/"+b.Branch); e != nil {
			return e
		}
		if _, e = git(ctx, r.Root, "merge-base", "--is-ancestor", b.Base, b.Head); e != nil {
			return fmt.Errorf("split base is not an ancestor of its prepared head")
		}
		if _, e = git(ctx, r.Root, "rev-parse", "--verify", b.RecoveryRef+"^{commit}"); e != nil {
			return e
		}
		for _, ref := range b.Evidence {
			if e = r.validateSource(ctx, p, ref); e != nil {
				return e
			}
		}
	}
	return nil
}
func (r *Runtime) splitBoundary(ctx context.Context, p Plan, i Intent) (SplitBoundary, error) {
	live, e := r.live(ctx, p)
	if e != nil {
		return SplitBoundary{}, e
	}
	reports, e := r.reports(ctx, p)
	if e != nil {
		return SplitBoundary{}, e
	}
	branch := stringValue(i.Payload, "branch")
	head := stringValue(i.Payload, "head")
	behavior := stringValue(i.Payload, "behavior")
	for n := len(reports) - 1; n >= 0; n-- {
		rr := reports[n]
		if rr.Occurrence.Kind != "scope" || rr.Claim.State != "complete" || rr.Report.Scope == nil {
			continue
		}
		var initial Snapshot
		if e = readJSON(rr.Claim.Snapshot, &initial); e != nil {
			return SplitBoundary{}, e
		}
		if occurrenceFingerprint(initial, rr.Occurrence) != occurrenceFingerprint(live, rr.Occurrence) {
			continue
		}
		if e = r.validateScope(ctx, p, rr.Occurrence, rr.Report.Scope); e != nil {
			return SplitBoundary{}, e
		}
		for _, b := range rr.Report.Scope.Boundaries {
			if b.Branch == branch && b.Head == head && b.Behavior == behavior {
				mode := rr.Report.Scope.EvacuateMode
				if behavior == "split_related" {
					mode = rr.Report.Scope.RelatedMode
				}
				if !rr.Report.Scope.Enabled || mode == "disabled" {
					return b, fmt.Errorf("partition policy disables this boundary")
				}
				if mode == "manual" && p.Authorizations["split_push"].RequestID == "" {
					return b, fmt.Errorf("manual partition needs a separate concrete split_push authorization")
				}
				return b, nil
			}
		}
	}
	return SplitBoundary{}, fmt.Errorf("split publication requires the accepted exact scope boundary")
}
