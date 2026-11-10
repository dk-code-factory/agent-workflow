package prfix

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	workflowassets "github.com/dk-code-factory/agent-workflow"
	"github.com/dk-code-factory/agent-workflow/pkg/clirunner"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

type PrepareRequest struct {
	Resume         bool
	RequestID      string
	Capabilities   func(string) (workflows.ProviderCapabilities, error)
	PR             string
	Run            string
	Instructions   string
	Upstream       string
	Options        map[string]any
	Adapters       map[string]Adapter
	Role           string
	Config         clirunner.Config
	ConfigPath     string
	MaintainOther  *Authorization
	Authorizations map[string]Authorization
}
type Prepared struct {
	LogicalRun   string          `json:"logical_run"`
	Workflow     string          `json:"workflow"`
	Plan         string          `json:"plan"`
	Roles        json.RawMessage `json:"roles"`
	Inputs       map[string]any  `json:"inputs"`
	RequestID    string          `json:"request_id"`
	Omitted      []string        `json:"omitted"`
	EngineLimits map[string]int  `json:"engine_limits"`
	Warnings     []string        `json:"warnings"`
}

func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func Prepare(ctx context.Context, root string, provider Provider, req PrepareRequest) (Prepared, error) {
	for kind, auth := range req.Authorizations {
		if e := auth.Validate(); e != nil {
			return Prepared{}, fmt.Errorf("authorization %s: %w", kind, e)
		}
	}
	if req.MaintainOther != nil {
		if e := req.MaintainOther.Validate(); e != nil {
			return Prepared{}, e
		}
	}
	id, e := ParseIdentity(req.PR)
	if e != nil {
		return Prepared{}, e
	}
	if !validText(req.Instructions) {
		return Prepared{}, fmt.Errorf("retain the exact user instruction with --instruction")
	}
	if req.Role == "" {
		return Prepared{}, fmt.Errorf("explicit managed leaf role required")
	}
	adapters := BuiltinAdapters(req.Role)
	for k, a := range req.Adapters {
		if k != a.Identity {
			return Prepared{}, fmt.Errorf("adapter map key %s differs from declared identity %s", k, a.Identity)
		}
		if adapters[k].Identity != "" {
			return Prepared{}, fmt.Errorf("custom adapter conflicts with bundled identity %s", k)
		}
		adapters[k] = a
	}
	p := Plan{Version: Version, LogicalRun: req.Run, Identity: id, Instructions: req.Instructions, Options: req.Options, Upstream: req.Upstream, Adapters: adapters, MaintainOther: req.MaintainOther, Authorizations: req.Authorizations, Groups: map[string][][]string{}, Sources: map[string]string{}, ConfigPath: req.ConfigPath}
	p.Roles, e = json.Marshal(req.Config)
	if e != nil {
		return Prepared{}, e
	}
	p.Selection, e = resolveSelection(ctx, root, id, req.Upstream, req.Options, []string{})
	if e != nil {
		return Prepared{}, e
	}
	if len(p.Selection.Invocation.Agent) > 0 {
		return Prepared{}, fmt.Errorf("native agent controls on the deterministic PR-fix process are unsupported; configure leaf controls")
	}
	if e = expand(&p, req.Role); e != nil {
		return Prepared{}, e
	}
	p.Snapshot, e = capture(ctx, root, provider, p)
	if e != nil {
		return Prepared{}, e
	}
	current, e := resolveSelection(ctx, root, id, req.Upstream, req.Options, p.Snapshot.Paths)
	oldSelection := p.Selection
	oldSelection.Preferences = nil
	comparison := current
	comparison.Preferences = nil
	if e == nil && digest(oldSelection) != digest(comparison) {
		return Prepared{}, fmt.Errorf("configuration changed during preparation; retry the read")
	}
	p.Selection = current
	if e != nil {
		return Prepared{}, e
	}
	// Compilation validates every selected identity, including optional work. It
	// consumes no claims or budgets and never starts a provider or remote effect.
	if e = expand(&p, req.Role); e != nil {
		return Prepared{}, e
	}
	active := map[string]bool{"completion": true}
	byID := map[string]Occurrence{}
	for _, o := range p.Occurrences {
		byID[o.ID] = o
	}
	hasReview := false
	for _, phase := range []string{"tasks", "finalizers"} {
		for _, group := range p.Groups[phase] {
			for _, id := range group {
				active[id] = true
				hasReview = hasReview || byID[id].Review
			}
		}
	}
	if hasReview {
		for _, phase := range []string{"before_cycle", "after_cycle"} {
			for _, group := range p.Groups[phase] {
				for _, id := range group {
					active[id] = true
				}
			}
		}
	}
	selectedCount := len(active) // recovery, verification and audience work are conditional
	if selectedCount > workflows.DefaultWorkerLimits().MaxWorkers {
		return Prepared{}, fmt.Errorf("first pass needs at least %d workers; engine limit is 64", selectedCount)
	}
	roleCounts := map[string]int{}
	for _, o := range p.Occurrences {
		if active[o.ID] {
			roleCounts[o.Pass.Role]++
		}
	}
	for role, n := range roleCounts {
		limit := req.Config.Agents[role].Limits.MaxWorkers
		if limit > 0 && n > limit {
			return Prepared{}, fmt.Errorf("role %s first-pass worker count %d exceeds limit %d", role, n, limit)
		}
	}
	runner := &clirunner.Runner{Config: req.Config, Workspace: root}
	used := map[string]bool{}
	for _, o := range p.Occurrences {
		role := o.Pass.Role
		if used[role] {
			continue
		}
		used[role] = true
		if _, ok := req.Config.Agents[role]; !ok {
			return Prepared{}, fmt.Errorf("selected managed role %s is unavailable", role)
		}
		capabilities := req.Capabilities
		if capabilities == nil {
			capabilities = runner.Capabilities
		}
		caps, e := capabilities(role)
		if e != nil {
			return Prepared{}, e
		}
		if !caps.Sessions || !caps.Resume || !caps.ManagedPolicy {
			return Prepared{}, fmt.Errorf("role %s lacks managed sessions/resume/policy required by checklist follow-ups", role)
		}
		agent := req.Config.Agents[role]
		if agent.Limits.MaxTurns > 0 && agent.Limits.MaxTurns < 5 {
			return Prepared{}, fmt.Errorf("role %s turn limit cannot support admitted checklist turns", role)
		}
		for _, tool := range agent.Tools {
			if strings.Contains(strings.ToLower(tool), "worker") {
				return Prepared{}, fmt.Errorf("leaf role %s may not coordinate workers", role)
			}
		}
		for _, grant := range agent.MCPGrants {
			if strings.Contains(strings.ToLower(fmt.Sprint(grant)), "worker") {
				return Prepared{}, fmt.Errorf("leaf role %s must not receive worker coordination grants", role)
			}
		}
	}
	for _, o := range p.Occurrences {
		if v, ok := o.Call.Invocation.Agent["model"].(string); ok && req.Config.Agents[o.Pass.Role].Model != v {
			return Prepared{}, fmt.Errorf("adapter model control does not match resolved role %s", o.Pass.Role)
		}
	}
	if req.Capabilities == nil {
		if e = runner.Preflight(ctx, sortedKeys(used)); e != nil {
			return Prepared{}, e
		}
	}
	l, e := OpenLedger(root)
	if e != nil {
		return Prepared{}, e
	}
	defer l.Close()
	if p.LogicalRun != "" {
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p.LogicalRun) {
			return Prepared{}, fmt.Errorf("invalid logical run ID")
		}
		saved, e := l.Plan(ctx, p.LogicalRun)
		if e != nil {
			return Prepared{}, e
		}
		if saved.Identity != p.Identity {
			return Prepared{}, fmt.Errorf("logical run belongs to another PR")
		}
	}
	if p.LogicalRun == "" {
		p.LogicalRun, e = l.Active(ctx, id.URL)
		if errors.Is(e, sql.ErrNoRows) {
			status, e := gitStatus(ctx, root)
			if e != nil {
				return Prepared{}, e
			}
			if status != "" {
				return Prepared{}, fmt.Errorf("new PR-fix runs require a clean worktree; unrelated user files were preserved")
			}
			p.LogicalRun = newID()
		} else if e != nil {
			return Prepared{}, e
		}
	}
	templates := map[string]string{}
	entries, e := workflowassets.Examples.ReadDir("workflows/pr-fix/v1")
	if e != nil {
		return Prepared{}, e
	}
	for _, entry := range entries {
		b, e := workflowassets.Examples.ReadFile("workflows/pr-fix/v1/" + entry.Name())
		if e != nil {
			return Prepared{}, e
		}
		templates[entry.Name()] = digest(string(b))
	}
	p.Sources["templates"] = digest(templates)
	reader, e := source.ReadFile("read_settings.py")
	if e != nil {
		return Prepared{}, e
	}
	p.Sources["reader"] = digest(string(reader))
	p.Fingerprint = digest(p)
	old, e := l.Plan(ctx, p.LogicalRun)
	if e == nil && old.Fingerprint != p.Fingerprint {
		if e = reconcileReplacement(ctx, l, provider, old); e != nil {
			return Prepared{}, e
		}
	} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return Prepared{}, e
	}
	dir := "workflows/.pr-fix/" + p.LogicalRun + "/" + p.Fingerprint
	p.Ref = dir + "/main.yml"
	// Put immutable plan artifacts before generating definitions. Engine actions
	// bind this path rather than a mutable latest-plan pointer.
	planPath, e := artifact(root, p)
	if e != nil {
		return Prepared{}, e
	}
	definitions, e := generate(p, planPath, dir)
	if e != nil {
		return Prepared{}, e
	}
	if len(definitions) > 128 {
		return Prepared{}, fmt.Errorf("plan exceeds engine closure limit of 128 definitions (%d)", len(definitions))
	}
	for ref, b := range definitions {
		w, e := workflows.Parse(b)
		if e != nil {
			return Prepared{}, fmt.Errorf("generated %s: %w", ref, e)
		}
		if e = workflows.Validate(w); e != nil {
			return Prepared{}, fmt.Errorf("generated %s: %w", ref, e)
		}
		if e = writeImmutable(filepath.Join(root, filepath.FromSlash(ref)), b); e != nil {
			return Prepared{}, e
		}
	}
	if _, e = l.SavePlan(ctx, p); e != nil {
		return Prepared{}, e
	}
	admissionRequest := req.RequestID
	if req.Resume && admissionRequest == "" && old.Fingerprint == p.Fingerprint {
		previous, _, err := l.LatestExecution(ctx, p.LogicalRun)
		if err == nil {
			admissionRequest = previous
		} else if !errors.Is(err, sql.ErrNoRows) {
			return Prepared{}, err
		}
	}
	requestID, e := l.Admission(ctx, p.LogicalRun, planPath, p.Fingerprint, admissionRequest)
	if e != nil {
		return Prepared{}, e
	}
	warnings := []string{}
	worst := selectedCount * p.Selection.Limits().ReviewCycles
	if worst > 64 {
		warnings = append(warnings, fmt.Sprintf("%d workers would be required if all selected leaves rerun in every review cycle; the unchanged engine limit is 64 per execution", worst))
	}
	return Prepared{LogicalRun: p.LogicalRun, Workflow: p.Ref, Plan: planPath, Roles: p.Roles, Inputs: map[string]any{"plan": planPath}, RequestID: requestID, Omitted: p.Omitted, EngineLimits: map[string]int{"worker_limit": 64, "first_pass_workers": selectedCount, "all_cycle_selected_workers": worst, "closure_limit": 128, "definitions": len(definitions)}, Warnings: warnings}, nil
}
func expand(p *Plan, role string) error {
	p.Occurrences = nil
	p.Groups = map[string][][]string{}
	p.Omitted = nil
	repeats := map[string]int{}
	type invocationContext struct {
		Instructions, Resources []string
		Required                bool
	}
	var visit func(Resolved, string, bool, []string, bool, invocationContext) ([][]string, error)
	visit = func(call Resolved, phase string, review bool, ancestors []string, policyProvider bool, inherited invocationContext) ([][]string, error) {
		id := call.Invocation.Skill
		if id == "" {
			id = "instructions"
		}
		a, ok := p.Adapters[id]

		if !ok {
			return nil, fmt.Errorf("selected identity %s requires an available deterministic adapter", id)
		}
		if contains(ancestors, id) && !(policyProvider && id == "dk-workflows:dk-commit-split" && call.Invocation.Options["policy"] == nil) {
			return nil, fmt.Errorf("recursive selection %s", id)
		}
		if e := validateAdapter(a); e != nil {
			return nil, e
		}
		if e := supportedOptions(a, call); e != nil {
			return nil, e
		}
		options := call.Invocation.Options
		if call.Configuration != nil {
			options = call.Configuration.Options
		}
		if a.Kind == "ci" {
			if e := validateCIOptions(options); e != nil {
				return nil, e
			}
		}
		children := childGroups(p.Selection, id, options, "reviewers")
		if policyProvider && (len(children) > 0 || options["policy"] != nil) {
			return nil, fmt.Errorf("commit-policy provider cannot coordinate reviewers or another policy")
		}
		if a.Kind == "review_group" && len(call.Invocation.Agent) > 0 {
			return nil, fmt.Errorf("review groups have no native coordinator for agent controls; configure child adapter controls instead")
		}
		if a.Kind == "review_group" && len(children) == 0 {
			return nil, fmt.Errorf("missing reviewer configuration blocks coverage; dk-workflows 5.2.0 selects no reviewers by default")
		}
		groups := [][]string{}
		policyIDs := []string{}
		for _, policyGroup := range childGroups(p.Selection, id, options, "policy") {
			for _, policy := range policyGroup {
				policyGroups, e := visit(policy, phase, false, append(append([]string{}, ancestors...), id), true, invocationContext{Required: true})
				if e != nil {
					return nil, e
				}
				for _, g := range policyGroups {
					for _, providerID := range g {
						policyIDs = append(policyIDs, providerID)
						for i := range p.Occurrences {
							o := &p.Occurrences[i]
							if o.ID == providerID {
								if o.Review || len(o.PolicyProviders) > 0 {
									return nil, fmt.Errorf("commit policy cannot select reviewers or recursive policies")
								}
								o.Kind = "commit_policy"
								o.Required, o.History, o.HeadSensitive = true, true, true
								o.Pass.Assignment = "Define grouping and message instructions only. Do not edit, rewrite, publish, delegate, or invoke cleanup. Return nonempty policy_prompt in the original complete report. " + o.Pass.Assignment
							}
						}
					}
				}
				groups = append(groups, policyGroups...)
			}
		}
		if len(children) > 0 {
			var parallel [][]Resolved
			execution, _ := options["execution"].(string)
			if execution != "" && execution != "parallel" && execution != "sequential" {
				return nil, fmt.Errorf("unsupported reviewer execution control %q", execution)
			}
			maxParallel := 100000
			if value := options["max_parallel"]; value != nil {
				n, ok := value.(float64)
				if !ok || n < 1 || math.Trunc(n) != n {
					return nil, fmt.Errorf("reviewer max_parallel must be a positive integer or null")
				}
				if n < 100000 {
					maxParallel = int(n)
				}
			}
			if execution == "sequential" {
				maxParallel = 1
			}

			// The pinned reviewer schema declares a flat invocation list. Its explicit
			// execution control owns batching; task/finalizer parallel groups elsewhere
			// retain their original sequential barriers.
			if execution != "sequential" {
				merged := []Resolved{}
				for _, g := range children {
					merged = append(merged, g...)
				}
				children = [][]Resolved{merged}
			}
			for _, g := range children {
				for len(g) > 0 {
					n := min(len(g), maxParallel)
					parallel = append(parallel, g[:n])
					g = g[n:]
				}
			}
			for _, group := range parallel {
				layers := [][]string{}
				for _, child := range group {
					parentContext := invocationContext{Instructions: append([]string{}, inherited.Instructions...), Resources: append(append([]string{}, inherited.Resources...), a.Resources...), Required: inherited.Required && call.Invocation.IsRequired()}
					if call.Invocation.Instructions != "" {
						parentContext.Instructions = append(parentContext.Instructions, call.Invocation.Instructions)
					}
					gs, e := visit(child, phase, true, append(append([]string{}, ancestors...), id), false, parentContext)
					if e != nil {
						return nil, e
					}
					for n, g := range gs {
						for len(layers) <= n {
							layers = append(layers, []string{})
						}
						layers[n] = append(layers[n], g...)
					}
				}
				groups = append(groups, layers...)
			}
		}
		if a.Kind == "review_group" {
			return groups, nil
		}
		occurrenceKey := phase + ":" + digest(call.Invocation)
		ordinal := repeats[occurrenceKey]
		repeats[occurrenceKey]++
		for _, passGroup := range a.Procedure {
			group := []string{}
			for _, pass := range passGroup {
				occID := "o_" + digest([]any{phase, call.Invocation, ordinal, pass.ID})[:24]
				kind := a.Kind
				review = review || a.Kind == "review"
				if review {
					kind = "review"
				}
				if (review || kind == "verification") && len(a.Writes) > 0 {
					return nil, fmt.Errorf("review and verification adapters must be read-only; use a separate writable occurrence")
				}
				o := Occurrence{InheritedResources: append([]string{}, inherited.Resources...), InheritedInstructions: append([]string{}, inherited.Instructions...), ID: occID, Kind: kind, Call: call, Adapter: a, Pass: pass, Required: inherited.Required && call.Invocation.IsRequired(), Review: review, History: a.History, HeadSensitive: a.HeadSensitive, Cacheable: a.Cacheable, MaxChecks: 3}
				o.PolicyProviders = append([]string{}, policyIDs...)
				o.Checklist = checklist(kind, pass.Evidence)
				p.Occurrences = append(p.Occurrences, o)
				group = append(group, occID)
			}
			groups = append(groups, group)
		}
		return groups, nil
	}
	for _, phase := range []string{"tasks", "finalizers", "before_cycle", "after_cycle"} {
		var configured [][]Resolved
		switch phase {
		case "tasks":
			configured = p.Selection.Tasks
		case "finalizers":
			configured = p.Selection.Finalizers
		default:
			configured = p.Selection.Hooks[phase]
		}
		for _, parallel := range configured {
			layers := [][]string{}
			for _, call := range parallel {
				groups, e := visit(call, phase, false, nil, false, invocationContext{Required: true})
				if e != nil {
					return e
				}
				for n, g := range groups {
					for len(layers) <= n {
						layers = append(layers, []string{})
					}
					layers[n] = append(layers[n], g...)
				}
			}
			p.Groups[phase] = append(p.Groups[phase], layers...)
		}
	}
	specials := map[string]string{"audience": "audience", "completion": "completion", "ci_repair": "ci_repair", "finding_verification": "verification", "reply_recovery": "reply"}
	for _, id := range sortedKeys(specials) {
		kind := specials[id]
		assignment := ""
		switch id {
		case "reply_recovery":
			assignment = "Reassess the exact inspected historical reply against current code and discussion. Prepare a separately authorized linked replacement only when still required. Return an operation with replaces containing the exact original run/id, inspection path/fingerprint, concrete replacement_required reason and sources. Preserve the historical receipt; never replay it or publish."
		case "audience":
			assignment = "Review this exact actual publication payload for audience suitability and authority. Public text must contain no private workflow reports, diagnostics, handles, or receipts. Return explicit evidence for the payload hash."
		case "completion":
			assignment = "Perform the managed live completion checklist for this exact selected workflow, including empty plans. Read retained original reports and sourced dispositions, current PR evidence and unresolved effects. Report selected completion and PR readiness separately; enumerate omitted duties. Do not expand permissions or silently restore disabled finalizers."
		case "finding_verification":
			assignment = "Independently verify every candidate in original_result_references. Preserve every original source, cover every candidate ID, and return sourced confirmed/dismissed/duplicate dispositions with reasons. Do not edit, publish, or coordinate agents."
		case "ci_repair":
			assignment = "Repair only the evidenced CI failure within the retained repair budget. Use applicable implementation preferences, original logs and relevant checks. Do not publish or coordinate agents. Return sourced dispositions."
		}
		pass := Pass{ID: id, Role: role, Assignment: assignment, Evidence: []string{"original-output", "current-source-evidence"}}
		a := Adapter{Identity: "prfix:" + id, Version: Implementation, Kind: kind, Procedure: [][]Pass{{pass}}}
		p.Occurrences = append(p.Occurrences, Occurrence{ID: id, Kind: kind, Pass: pass, Adapter: a, Required: true, MaxChecks: 3, HeadSensitive: true, Checklist: checklist(kind, pass.Evidence)})
	}
	defaults := []string{"dk-commit-split", "dk-pr-decisions", "dk-pr-description", "dk-pr-feedback-reply", "dk-ci", "dk-pr-publish"}
	duties := map[string]string{"dk-commit-split": "cleanup", "dk-pr-decisions": "decisions", "dk-pr-description": "description", "dk-pr-feedback-reply": "reply", "dk-ci": "ci", "dk-pr-publish": "publication"}
	for _, name := range defaults {
		found := false
		for _, group := range p.Selection.Finalizers {
			for _, c := range group {
				found = found || c.Invocation.Skill == "dk-workflows:"+name
				for _, o := range p.Occurrences {
					found = found || (o.Call.Invocation.Skill == c.Invocation.Skill && o.Kind == duties[name])
				}
			}
		}
		if !found {
			p.Omitted = append(p.Omitted, name)
		}
	}
	return checkParallelConflicts(*p)
}
func expr(s string) string { return "${{ " + s + " }}" }
func callJob(ref string, needs []string, with map[string]any) workflows.Job {
	return workflows.Job{Uses: ref, Needs: needs, With: with}
}
func generate(p Plan, planPath, dir string) (map[string][]byte, error) {
	defs := map[string][]byte{}
	// Copy templates into this immutable closure and rewrite only local refs.
	entries, e := workflowassets.Examples.ReadDir("workflows/pr-fix/v1")
	if e != nil {
		return nil, e
	}
	for _, entry := range entries {
		b, e := workflowassets.Examples.ReadFile("workflows/pr-fix/v1/" + entry.Name())
		if e != nil {
			return nil, e
		}
		defs[dir+"/"+entry.Name()] = []byte(strings.ReplaceAll(string(b), "workflows/pr-fix/v1/", dir+"/"))
	}
	inputs := map[string]workflows.Input{"plan": {Type: "string", Required: true}, "cycle": {Type: "string", Required: true}}
	phase := workflows.Workflow{Name: "PR fix selected tasks and finalizers", On: workflows.WorkflowTriggers{WorkflowCall: &workflows.WorkflowCall{Inputs: inputs, Outputs: map[string]workflows.Output{"complete": {Value: expr("jobs.completion.outputs.complete")}, "ready": {Value: expr("jobs.completion.outputs.ready")}, "report": {Value: expr("jobs.completion.outputs.report")}}}}, Jobs: map[string]workflows.Job{}}
	byID := map[string]Occurrence{}
	for _, o := range p.Occurrences {
		byID[o.ID] = o
	}
	previous := []string{}
	counter := 0
	hookGate := false
	addGroups := func(groups [][]string) {
		for _, group := range groups {
			join := []string{}
			for _, id := range group {
				counter++
				jobID := fmt.Sprintf("stage_%03d", counter)
				o := byID[id]
				ref := dir + "/occurrence.yml"
				switch o.Kind {
				case "ci":
					ref = dir + "/ci.yml"
				case "review":
					ref = dir + "/review.yml"
				case "inspection":
					ref = dir + "/inspection.yml"
				case "scope":
					ref = dir + "/scope.yml"
				case "fix":
					ref = dir + "/feedback.yml"
				case "cleanup", "description", "decisions", "reply", "publication":
					ref = dir + "/finalization.yml"
				}
				j := callJob(ref, previous, map[string]any{"plan": expr("inputs.plan"), "cycle": expr("inputs.cycle"), "occurrence": id})
				if hookGate {
					j.Needs = append(append([]string{}, j.Needs...), "review_admission")
					j.If = expr("needs.review_admission.outputs.invoke")
				}
				phase.Jobs[jobID] = j
				join = append(join, jobID)
				if o.Kind == "publication" {
					listID := jobID + "_intents"
					phase.Jobs[listID] = workflows.Job{Needs: []string{jobID}, Steps: []workflows.Step{{ID: "intents", Uses: "action/prfix_intents", With: map[string]any{"plan": expr("inputs.plan")}}}, Outputs: map[string]string{"items": expr("steps.intents.outputs.items")}}
					pubID := jobID + "_publish"
					phase.Jobs[pubID] = workflows.Job{Needs: []string{listID}, Uses: dir + "/publication.yml", ForEach: &workflows.ForEach{Items: expr("needs." + listID + ".outputs.items"), Key: expr("item.id")}, With: map[string]any{"plan": expr("inputs.plan"), "cycle": expr("inputs.cycle"), "intent": expr("item")}}
					join[len(join)-1] = pubID
				}
			}
			previous = join
		}
	}
	// Every selected review block, including finalizers, is followed by a
	// verification barrier before later configured work can consume its findings.
	before, pendingReview := false, false
	verifications := 0
	verifyPending := func() {
		if !pendingReview {
			return
		}
		verifications++
		id := "verify_findings"
		if verifications > 1 {
			id = fmt.Sprintf("verify_findings_%d", verifications)
		}
		phase.Jobs[id] = callJob(dir+"/verification.yml", previous, map[string]any{"plan": expr("inputs.plan"), "cycle": expr("inputs.cycle")})
		previous = []string{id}
		pendingReview = false
	}
	emitGroup := func(group []string) {
		isReview := false
		for _, id := range group {
			isReview = isReview || byID[id].Review
		}
		if isReview && !before {
			phase.Jobs["review_admission"] = workflows.Job{Needs: previous, Steps: []workflows.Step{{ID: "admit", Uses: "action/prfix_review_admit", With: map[string]any{"plan": expr("inputs.plan"), "cycle": expr("inputs.cycle")}}}, Outputs: map[string]string{"invoke": expr("steps.admit.outputs.invoke")}}
			previous = []string{"review_admission"}
			hookGate = true
			addGroups(p.Groups["before_cycle"])
			hookGate = false
			before = true
		}
		if !isReview {
			verifyPending()
		}
		addGroups([][]string{group})
		if isReview {
			pendingReview = true
		}
	}
	for _, group := range p.Groups["tasks"] {
		emitGroup(group)
	}
	verifyPending()
	finalReviews := false
	for _, group := range p.Groups["finalizers"] {
		for _, id := range group {
			finalReviews = finalReviews || byID[id].Review
		}
	}
	afterHooks := func() {
		if before {
			hookGate = true
			addGroups(p.Groups["after_cycle"])
			hookGate = false
		}
	}
	if !finalReviews {
		afterHooks()
	}
	for _, group := range p.Groups["finalizers"] {
		emitGroup(group)
	}
	verifyPending()
	if finalReviews {
		afterHooks()
	}

	phase.Jobs["completion"] = callJob(dir+"/completion.yml", previous, map[string]any{"plan": expr("inputs.plan"), "cycle": expr("inputs.cycle")})
	phaseBytes, e := yaml.Marshal(phase)
	if e != nil {
		return nil, e
	}
	defs[dir+"/pass.yml"] = phaseBytes
	main := workflows.Workflow{Name: "dk-pr-fix 5.2.0 deterministic YAML", On: workflows.WorkflowTriggers{WorkflowCall: &workflows.WorkflowCall{Inputs: map[string]workflows.Input{"plan": {Type: "string", Default: planPath}}, Outputs: map[string]workflows.Output{"selected_complete": {Value: expr("jobs.iterate.outputs.complete")}, "pr_ready": {Value: expr("jobs.iterate.outputs.ready")}, "private_report": {Value: expr("jobs.iterate.outputs.report")}}}}, Jobs: map[string]workflows.Job{}}
	main.Jobs["iterate"] = callJob(dir+"/cycle.yml", nil, map[string]any{"plan": expr("inputs.plan")})
	j := main.Jobs["iterate"]
	j.Repeat = &workflows.Repeat{MaxAttempts: 100, Until: expr("iteration.outputs.complete")}
	main.Jobs["iterate"] = j
	main.Jobs["merge_intents"] = workflows.Job{Needs: []string{"iterate"}, Steps: []workflows.Step{{ID: "intents", Uses: "action/prfix_intents", With: map[string]any{"plan": expr("inputs.plan"), "kind": "merge"}}}, Outputs: map[string]string{"items": expr("steps.intents.outputs.items")}}
	main.Jobs["merge"] = workflows.Job{Needs: []string{"iterate", "merge_intents"}, Uses: dir + "/publication.yml", ForEach: &workflows.ForEach{Items: expr("needs.merge_intents.outputs.items"), Key: expr("item.id")}, With: map[string]any{"plan": expr("inputs.plan"), "cycle": "completion", "intent": expr("item")}}
	main.Jobs["close"] = workflows.Job{Needs: []string{"merge"}, Steps: []workflows.Step{{ID: "close", Uses: "action/prfix_close", With: map[string]any{"plan": expr("inputs.plan")}}}, Outputs: map[string]string{"report": expr("steps.close.outputs.report")}}
	main.On.WorkflowCall.Outputs["publication_report"] = workflows.Output{Value: expr("jobs.close.outputs.report")}
	cycle := workflows.Workflow{Name: "PR fix retained cycle", On: workflows.WorkflowTriggers{WorkflowCall: &workflows.WorkflowCall{Inputs: map[string]workflows.Input{"plan": {Type: "string", Required: true}}, Outputs: map[string]workflows.Output{"complete": {Value: expr("jobs.pass.outputs.complete")}, "ready": {Value: expr("jobs.pass.outputs.ready")}, "report": {Value: expr("jobs.pass.outputs.report")}}}}, Jobs: map[string]workflows.Job{"pin": {Steps: []workflows.Step{{ID: "cycle", Uses: "action/prfix_cycle", With: map[string]any{"plan": expr("inputs.plan")}}}, Outputs: map[string]string{"cycle": expr("steps.cycle.outputs.cycle")}}, "pass": callJob(dir+"/pass.yml", []string{"pin"}, map[string]any{"plan": expr("inputs.plan"), "cycle": expr("needs.pin.outputs.cycle")})}}
	for ref, w := range map[string]workflows.Workflow{p.Ref: main, dir + "/cycle.yml": cycle} {
		b, e := yaml.Marshal(w)
		if e != nil {
			return nil, e
		}
		defs[ref] = b
	}
	return defs, nil
}
func ReadPlan(path string) (Plan, error) {
	var p Plan
	e := readJSON(path, &p)
	if e != nil {
		return p, e
	}
	if p.Version != Version || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(p.LogicalRun) {
		return p, fmt.Errorf("unsupported PR-fix plan")
	}
	return p, nil
}
func (p Plan) Occurrence(id string) (Occurrence, error) {
	for _, o := range p.Occurrences {
		if o.ID == id {
			return o, nil
		}
	}
	return Occurrence{}, fmt.Errorf("occurrence %q outside pinned plan", id)
}
func sortedKeys[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResumeRequest rebuilds preparation inputs without importing plugin histories or
// granting permissions. The caller may apply explicit current invocation edits.
func ResumeRequest(p Plan, cfg clirunner.Config) PrepareRequest {
	role := ""
	if o, e := p.Occurrence("completion"); e == nil {
		role = o.Pass.Role
	}
	custom := map[string]Adapter{}
	bundled := BuiltinAdapters(role)
	for id, a := range p.Adapters {
		if _, ok := bundled[id]; !ok {
			custom[id] = a
		}
	}
	return PrepareRequest{Resume: true, PR: p.Identity.URL, Run: p.LogicalRun, Instructions: p.Instructions, Upstream: p.Upstream, Options: p.Options, Adapters: custom, Role: role, Config: cfg, ConfigPath: p.ConfigPath, MaintainOther: p.MaintainOther, Authorizations: p.Authorizations}
}
