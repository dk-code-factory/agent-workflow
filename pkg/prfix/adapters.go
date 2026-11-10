package prfix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func BuiltinAdapters(role string) map[string]Adapter {
	specs := map[string]struct {
		kind, assignment     string
		history, head, cache bool
	}{
		"dk-pr-feedback-read":    {"inspection", "Read every review, inline thread, general comment and active Decisions in the complete PR evidence. Identify author requests, deferred work, exact acceptances and sourced no-reply obligations. Declare reply_obligations separately from actionable code findings. Do not edit or publish.", false, false, true},
		"dk-pr-decisions":        {"decisions", "Reconcile settled decisions and exact user acceptances with their original sources. Prepare only authorized Decisions edits as operation proposals. Keep private workflow evidence out of public text.", false, false, true},
		"dk-pr-feedback-address": {"fix", "Verify original feedback and reviewer findings independently. Fix actionable issues within the assignment and preferences, run relevant checks, and return sourced dispositions for every finding. Preserve exact accepted/deferred instructions. Do not publish, commit, coordinate agents, or alter workflow state.", false, false, true},
		"dk-pr-split":            {"scope", "Assess the whole diff against original scope, Decisions, partition threshold, automatic/manual/disabled modes and per-PR opt-outs. Return reasoned keep/split decisions and conditional instructions. Prepare automatic split operations only when authorized. Manual actions need concrete user authorization. Do not publish.", false, false, true},
		"dk-pr-description":      {"description", "Prepare current PR title/body from the complete diff, Decisions, repository templates and drafting policies. Preserve unrelated content. Review proposed public text for its audience. Do not publish.", false, false, true},
		"dk-commit-split":        {"cleanup", "Assess every commit in the complete series against the effective whole commit policy and provider handoff. Return per-commit coverage, grouping/boundary reasons, source-to-destination mapping, validation and recovery refs. Prepare local cleanup with exact reviewed/final heads and trees; never report prepared as published. Do not push or rewrite without explicit authorization.", true, true, false},
		"dk-pr-feedback-reply":   {"reply", "Prepare concise replies only for concrete author requests and explicit reply obligations, with sourced no-reply assessments for other-participant questions and optional discussion. Return responses covering every declared reply_obligation, citing its original native output and prepared reply operation, or an explicit sourced no-reply assessment. Resolve only fully answered inline threads after verified reply receipts. Do not send anything.", false, true, false},
		"dk-ci":                  {"ci", "Inspect exact-head CI observation, retained deadline and complete Actions evidence. Classify failures only with verified open-issue flake evidence or exact configured exceptions. Test exemptions require every exact-attempt failed step and its complete log accounted for by supported test-runner output and exact single-line signatures; unsupported output remains unknown. Pending, no checks, access denial and unknown failure block CI. Prepare eligible rerun or repair proposals; never claim a skipped or stale check passed.", false, true, false},
		"dk-ci-flakes":           {"verification", "Verify each claimed flaky test or CI exception against an open issue, exact run/job/attempt and current policy. Return original evidence and per-candidate dispositions. Do not rerun or publish.", false, true, false},
		"dk-pr-publish":          {"publication", "Review all prepared existing-PR operations against exact permissions, current head/target and lease, obligations and audience. Return concrete operation proposals or a sourced no-op assessment. Do not dispatch writes. Configuration and cached evidence grant no permission.", false, true, false},
		"dk-pr-reviewers-run":    {"review_group", "This is expanded deterministically into selected reviewer adapters; do not coordinate agents.", false, false, true},
	}
	adapters := map[string]Adapter{}
	for name, s := range specs {
		id := "dk-workflows:" + name
		adapters[id] = Adapter{Identity: id, Version: Implementation, Kind: s.kind, History: s.history, HeadSensitive: s.head, Cacheable: s.cache, Procedure: [][]Pass{{{ID: "primary", Role: role, Assignment: s.assignment, Evidence: []string{"original-output", "complete-source-coverage", "validation"}}}}}
	}
	adapters["instructions"] = Adapter{Identity: "instructions", Version: Implementation, Kind: "hook", History: true, HeadSensitive: true, Procedure: [][]Pass{{{ID: "instructions", Role: role, Assignment: "Execute only the exact invocation instructions within the pinned PR scope. Do not coordinate workers, choose workflow stages, publish or commit. Return original evidence and a focused change manifest for any authorized local edits.", Evidence: []string{"invocation-output", "validation"}}}}}
	return adapters
}
func LoadAdapters(path string) (map[string]Adapter, error) {
	result := map[string]Adapter{}
	if path == "" {
		return result, nil
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) > 4<<20 {
		return nil, fmt.Errorf("adapter document exceeds 4 MiB")
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	var list []Adapter
	if e = d.Decode(&list); e != nil {
		return nil, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("one adapter document required")
	}
	for _, a := range list {
		if _, ok := result[a.Identity]; ok {
			return nil, fmt.Errorf("conflicting adapter %s", a.Identity)
		}
		if e = validateAdapter(a); e != nil {
			return nil, e
		}
		result[a.Identity] = a
	}
	return result, nil
}
func validateAdapter(a Adapter) error {
	if !validText(a.Identity) || !validText(a.Version) || !validText(a.Kind) || len(a.Procedure) == 0 {
		return fmt.Errorf("adapter identity, immutable version, kind and YAML procedure required")
	}
	for _, paths := range [][]string{a.Reads, a.Writes} {
		for _, path := range paths {
			if path == "." {
				continue
			}
			if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.HasPrefix(path, "../") || path == ".." || strings.ContainsAny(path, "*?[\\") {
				return fmt.Errorf("adapter %s ownership must use literal repository-relative paths", a.Identity)
			}
		}
	}

	seen := map[string]bool{}
	for _, group := range a.Procedure {
		if len(group) == 0 {
			return fmt.Errorf("empty adapter parallel group")
		}
		for _, pass := range group {
			if !validText(pass.ID) || seen[pass.ID] || !validText(pass.Role) || !validText(pass.Assignment) || len(pass.Evidence) == 0 {
				return fmt.Errorf("adapter %s requires unique bounded passes, roles, assignments and evidence", a.Identity)
			}
			for _, requirement := range pass.Evidence {
				if !validText(requirement) {
					return fmt.Errorf("adapter %s evidence requirements must be nonempty text", a.Identity)
				}
			}
			seen[pass.ID] = true
		}
	}
	for _, control := range a.Controls {
		if control != "model" {
			return fmt.Errorf("adapter %s requests unsupported control %s", a.Identity, control)
		}
	}
	return nil
}
func checklist(kind string, evidence []string) []ChecklistItem {
	items := []ChecklistItem{{"scope", "Cover the exact bounded assignment and applicable preferences, including sourced conditional decisions."}, {"evidence", "Retain original outputs and complete governing source references."}, {"validation", "Provide relevant independent checks or a sourced inapplicability assessment."}, {"deliverables", "Deliver explicit completion, blockers and every requested output."}}
	switch kind {
	case "review", "verification":
		items = append(items, ChecklistItem{"native", "Every required pass must have engine-observed successful execution and original outputs."})
	case "fix", "ci_repair":
		items = append(items, ChecklistItem{"dispositions", "Give sourced dispositions for all original candidates and relevant test evidence."})
	case "scope":
		items = append(items, ChecklistItem{"partition", "Assess threshold, complete scope, per-PR opt-outs and both partition modes."})
	case "cleanup":
		items = append(items, ChecklistItem{"series", "Cover every commit, boundary and mapping, resulting tree/head, recovery and publication receipt."})
	case "reply":
		items = append(items, ChecklistItem{"responses", "Cover every reply/no-reply obligation and only fully answered resolutions."})
	case "audience":
		items = append(items, ChecklistItem{"audience", "Review the exact actual publication payload hash for its intended audience; exclude private reports, handles, receipts and operational diagnostics."})
	case "completion":
		items = append(items, ChecklistItem{"live", "Check current discussion, Decisions, instructions, head, target and selected plan, including an empty plan."}, ChecklistItem{"readiness", "Report selected-workflow completion separately from PR readiness and every omitted default duty."})
	}
	for i, v := range evidence {
		items = append(items, ChecklistItem{fmt.Sprintf("adapter_%d", i), v})
	}
	return items
}
func childGroups(s Selection, id string, options map[string]any, kind string) [][]Resolved {
	for signature, groups := range s.Children {
		var key []any
		if jsonDecode([]byte(signature), &key) == nil && len(key) == 2 && key[0] == id && digest(key[1]) == digest(options) {
			return groups[kind]
		}
	}
	return nil
}
func jsonDecode(b []byte, out any) error { return json.Unmarshal(b, out) }
func supportedOptions(a Adapter, call Resolved) error {
	if len(call.Invocation.Agent) > 0 {
		for key := range call.Invocation.Agent {
			if !contains(a.Controls, key) {
				return fmt.Errorf("adapter %s cannot enforce agent control %s", a.Identity, key)
			}
		}
	}
	if !strings.HasPrefix(a.Identity, "dk-workflows:") && call.Configuration != nil {
		for key := range call.Configuration.Options {
			if !contains(a.Options, key) && key != "reviewers" {
				return fmt.Errorf("adapter %s does not declare option %s", a.Identity, key)
			}
		}
	}
	return nil
}
func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

func mutates(o Occurrence) bool {
	return o.Kind == "fix" || o.Kind == "ci_repair" || o.Kind == "cleanup" || o.Kind == "hook" || len(o.Adapter.Writes) > 0
}
func pathsConflict(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == "." || y == "." || x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") || strings.ContainsAny(x+y, "*?[") {
				return true
			}
		}
	}
	return false
}
func checkParallelConflicts(p Plan) error {
	all := map[string]Occurrence{}
	for _, o := range p.Occurrences {
		all[o.ID] = o
	}
	for _, groups := range p.Groups {
		for _, group := range groups {
			// These procedures use plan-wide singleton occurrences. Independent
			// parallel calls cannot safely share their audience or repair claims.
			singletons := map[string]bool{}
			for _, id := range group {
				kind := all[id].Kind
				if kind == "publication" || kind == "ci" {
					if singletons[kind] {
						return fmt.Errorf("conflicting parallel %s procedures share managed occurrences; select sequential calls", kind)
					}
					singletons[kind] = true
				}
			}
			for i, id := range group {
				a := all[id]
				writes := a.Adapter.Writes
				if mutates(a) && len(writes) == 0 {
					writes = []string{"."}
				}
				if len(writes) == 0 {
					continue
				}
				for j, other := range group {
					if i == j {
						continue
					}
					b := all[other]
					access := append(append([]string{}, b.Adapter.Reads...), b.Adapter.Writes...)
					if len(access) == 0 {
						access = []string{"."}
					}
					if mutates(a) && mutates(b) {
						return fmt.Errorf("parallel mutations %s and %s share the Git index; select sequential procedures", id, other)
					}
					if pathsConflict(writes, access) {
						return fmt.Errorf("conflicting parallel selections %s and %s require disjoint declared read/write ownership", id, other)
					}
				}
			}
		}
	}
	return nil
}
