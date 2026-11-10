package prfix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// A handoff contains the actual accepted provider instructions and immutable
// context, resource and engine references, not just a configured identity.
type CommitPolicyHandoff struct {
	Prompt        *string  `json:"policy_prompt"`
	Configuration any      `json:"configuration"`
	Context       string   `json:"context"`
	Providers     []string `json:"providers"`
}

func (r *Runtime) commitPolicy(ctx context.Context, p Plan, o Occurrence, s Snapshot) (*CommitPolicyHandoff, error) {
	opts := o.Call.Invocation.Options
	if o.Call.Configuration != nil {
		opts = o.Call.Configuration.Options
	}
	policy := opts["policy"]
	handoff := &CommitPolicyHandoff{Configuration: policy, Context: occurrenceFingerprint(s, o), Providers: []string{}}
	if policy == nil {
		return handoff, nil
	}
	value, ok := policy.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("malformed commit policy")
	}
	if prompt, ok := value["prompt"].(string); ok && validText(prompt) {
		handoff.Prompt = &prompt
		return handoff, nil
	}
	if len(o.PolicyProviders) == 0 {
		return nil, fmt.Errorf("commit policy provider was not materialized")
	}
	reports, err := r.reports(ctx, p)
	if err != nil {
		return nil, err
	}
	prompts := []string{}
	for _, id := range o.PolicyProviders {
		var latest *retainedReport
		for i := range reports {
			if reports[i].Occurrence.ID == id {
				latest = &reports[i]
			}
		}
		if latest == nil || latest.Claim.State != "complete" || !validText(latest.Report.PolicyPrompt) {
			return nil, fmt.Errorf("commit policy %s requires accepted original instructions", id)
		}
		var initial Snapshot
		if err = readJSON(latest.Claim.Snapshot, &initial); err != nil {
			return nil, err
		}
		if occurrenceFingerprint(initial, latest.Occurrence) != occurrenceFingerprint(s, latest.Occurrence) {
			return nil, fmt.Errorf("commit policy %s governing inputs stale", id)
		}
		handoff.Providers = append(handoff.Providers, latest.Reference)
		prompts = append(prompts, latest.Report.PolicyPrompt)
	}
	prompt := strings.Join(prompts, "\n\n")
	handoff.Prompt = &prompt
	return handoff, nil
}

// Accepted cleanup may deliberately change history. Preserve its starting
// policy context only for the verified resulting head/tree; other changes stale it.
func cleanupContextCurrent(initial, live Snapshot, o Occurrence, cleanup *Cleanup) bool {
	if cleanup == nil || !contains([]string{"no_op", "published"}, cleanup.Outcome) || cleanup.FinalHead != live.Head || cleanup.FinalTree != live.LocalTree || initial.LocalTree != live.LocalTree {
		return false
	}
	initial.Head = live.Head
	initial.LocalHead = live.LocalHead
	initial.History = live.History
	return occurrenceFingerprint(initial, o) == occurrenceFingerprint(live, o)
}
func (r *Runtime) policyConsumedByCleanup(provider retainedReport, reports []retainedReport, live Snapshot) (bool, error) {
	for _, rr := range reports {
		if rr.Claim.State != "complete" || rr.Occurrence.Kind != "cleanup" {
			continue
		}
		var initial Snapshot
		if err := readJSON(rr.Claim.Snapshot, &initial); err != nil {
			return false, err
		}
		if !cleanupContextCurrent(initial, live, rr.Occurrence, rr.Report.Cleanup) {
			continue
		}
		var assignment struct {
			Policy *CommitPolicyHandoff `json:"commit_policy"`
		}
		// Assignments include other fields; decode just this verified immutable input.
		raw, err := os.ReadFile(rr.Claim.Assignment)
		if err != nil {
			return false, err
		}
		if err = json.Unmarshal(raw, &assignment); err != nil {
			return false, err
		}
		if assignment.Policy != nil && assignment.Policy.Context == occurrenceFingerprint(initial, rr.Occurrence) && contains(assignment.Policy.Providers, provider.Reference) {
			return true, nil
		}
	}
	return false, nil
}
