package workflows

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

type skillChildFixture struct {
	Session      string                  `json:"session"`
	Prompt       string                  `json:"prompt"`
	Description  string                  `json:"description"`
	Observations []SkillChildObservation `json:"observations"`
}

func childLedgerFixture(t *testing.T, name string) (*SkillInvocation, *SkillEvidence, skillChildFixture) {
	t.Helper()
	data, err := os.ReadFile("testdata/skill-child-sdk-0.3.322.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SDK   string                       `json:"sdk"`
		CLI   string                       `json:"cli"`
		Cases map[string]skillChildFixture `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SDK != "0.3.322" || fixture.CLI != "2.1.322" {
		t.Fatal("wrong pinned native fixture")
	}
	f := fixture.Cases[name]
	plan := &SkillInvocation{ID: "original-plan", Binding: SkillBinding{Name: "probe"}, Arguments: "exact", ResolvedEffort: "high", Model: "claude-sonnet-4-6", ContinuationRequiresEvidence: true, Children: []SkillChild{{Name: "review", Type: "admitted-child", Prompt: f.Prompt, Description: f.Description, Model: "claude-sonnet-4-6", Effort: "high", MaxTurns: 2, MaxCalls: 1, Tools: []string{"Read"}}}}
	ledger := &SkillEvidence{}
	for _, kind := range []string{"invocation", "result"} {
		if err := ledger.Observe(plan, SkillObservation{Key: kind + ":skill", Kind: kind, SessionID: f.Session, CallID: "skill", Name: "probe", Arguments: "exact", Raw: json.RawMessage(`{"original_native_skill":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	return plan, ledger, f
}

func TestSkillChildOriginalPublicEvidenceJoinsAcrossArrivalOrder(t *testing.T) {
	plan, ledger, f := childLedgerFixture(t, "selected-child")
	// Public child messages retain their own stream order; hook/task/result
	// delivery can race and is joined by original identities, not arrival time.
	var messages, other []SkillChildObservation
	for _, observation := range f.Observations {
		if observation.Kind == "message" {
			messages = append(messages, observation)
		} else {
			other = append(other, observation)
		}
	}
	slices.Reverse(other)
	for _, observation := range append(other, messages...) {
		if err := ledger.ObserveChild(plan, observation); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.Complete(plan); err != nil {
		t.Fatal("original full child evidence incomplete", err)
	}
	if err := ledger.Continuation(plan, f.Session); err != nil {
		t.Fatal("original child receipt did not survive continuation", err)
	}
	original := len(ledger.Children)
	if err := ledger.ObserveChild(plan, f.Observations[0]); err != nil || len(ledger.Children) != original {
		t.Fatal("repeated original receipt was not idempotent", err)
	}
	changed := f.Observations[0]
	changed.Raw = json.RawMessage(strings.Replace(string(changed.Raw), "Admitted deterministic child", "Changed admitted child", 1))
	if ledger.ObserveChild(plan, changed) == nil {
		t.Fatal("conflicting original permit accepted")
	}
}

func TestSkillChildStatusAndSummaryCannotCompleteTurnExhaustion(t *testing.T) {
	plan, ledger, f := childLedgerFixture(t, "exhaustion")
	for _, observation := range f.Observations {
		if err := ledger.ObserveChild(plan, observation); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.InvocationComplete(plan); err != nil {
		t.Fatal(err)
	}
	if ledger.Complete(plan) == nil || ledger.Continuation(plan, f.Session) == nil {
		t.Fatal("completed statuses counted an exhausted child as original completion")
	}
}

func TestSkillChildMissingOrMismatchedOriginalEvidenceRemainsIncomplete(t *testing.T) {
	for _, variant := range []string{"permit", "start", "stop", "message", "task-start", "task-end", "result", "parent_terminal", "original-output", "model", "effort", "read-output"} {
		t.Run(variant, func(t *testing.T) {
			plan, ledger, f := childLedgerFixture(t, "selected-child")
			for _, observation := range f.Observations {
				raw := childRaw(observation)
				if observation.Kind == variant || variant == "task-start" && raw["subtype"] == "task_started" || variant == "task-end" && raw["subtype"] == "task_notification" {
					continue
				}
				if variant == "read-output" && observation.Kind == "message" && raw["type"] == "user" && len(childArray(childObject(raw["message"])["content"])) > 0 {
					continue
				}
				if variant == "original-output" && observation.Kind == "result" {
					childObject(raw["tool_use_result"])["content"] = []any{map[string]any{"type": "text", "text": "report-only summary"}}
				}
				if variant == "model" && observation.Kind == "message" && raw["type"] == "assistant" {
					childObject(raw["message"])["model"] = "foreign-model"
				}
				if variant == "effort" && observation.Kind == "stop" {
					childObject(raw["effort"])["level"] = "low"
				}
				observation.Raw, _ = json.Marshal(raw)
				if err := ledger.ObserveChild(plan, observation); err != nil {
					t.Fatal(err)
				}
			}
			if ledger.Complete(plan) == nil {
				t.Fatal("missing/mismatched original child evidence completed", variant)
			}
		})
	}
}

func TestSkillChildForeignParentAndSessionRejected(t *testing.T) {
	for _, variant := range []string{"parent", "session", "child", "type", "background", "override", "missing-parent", "missing-session", "missing-original-message", "changed-original-message", "foreign-original-model"} {
		t.Run(variant, func(t *testing.T) {
			plan, ledger, f := childLedgerFixture(t, "selected-child")
			observation := f.Observations[0]
			raw := childRaw(observation)
			switch variant {
			case "parent":
				observation.ParentCallID = "foreign-parent"
			case "session":
				observation.SessionID = "foreign-session"
			case "child":
				observation = f.Observations[1]
				observation.ChildID = "foreign-child"
			case "type":
				observation.Type = "general-purpose"
			case "background":
				childObject(raw["tool_input"])["run_in_background"] = true
				observation.Raw, _ = json.Marshal(raw)
			case "override":
				childObject(raw["tool_input"])["model"] = "haiku"
				observation.Raw, _ = json.Marshal(raw)
			case "missing-parent":
				observation.ParentCallID = ""
			case "missing-session":
				observation.SessionID = ""
			case "missing-original-message":
				observation.ParentMessage = nil
			case "changed-original-message", "foreign-original-model":
				var parent map[string]any
				_ = json.Unmarshal(observation.ParentMessage, &parent)
				if variant == "foreign-original-model" {
					childObject(parent["message"])["model"] = "foreign-model"
				} else {
					for _, v := range childArray(childObject(parent["message"])["content"]) {
						item := childObject(v)
						if item["type"] == "tool_use" {
							childObject(item["input"])["resume"] = "foreign-child"
						}
					}
				}
				observation.ParentMessage, _ = json.Marshal(parent)
			}
			if ledger.ObserveChild(plan, observation) == nil {
				t.Fatal("foreign child permission/evidence accepted", variant)
			}
		})
	}
}

func TestSkillChildAdmissionPinsControlsAndParentResourceSubset(t *testing.T) {
	for _, variant := range []string{"valid", "background-missing", "background-true", "calls", "turns", "model", "tools", "resource", "evidence", "late-prompt"} {
		t.Run(variant, func(t *testing.T) {
			foreground := false
			selection := &SkillSelection{Children: []SkillChildSelection{{Name: "review", Prompt: "Review ${{ inputs.target }}.", Description: "Review admitted reference", Tools: []string{"Read"}, Resources: []string{"reference.txt"}, Model: "inherit", MaxTurns: 2, MaxCalls: 1, Background: &foreground}}}
			selection.Evidence.Children = []string{"review"}
			plan := &SkillInvocation{Model: "claude-sonnet-4-6", Resources: []string{"reference.txt"}, Binding: SkillBinding{Entry: "skills/probe/SKILL.md", Assets: &AssetBundle{Entries: []AssetEntry{{Path: "skills/probe/reference.txt", Kind: "file"}}}}}
			switch variant {
			case "background-missing":
				selection.Children[0].Background = nil
			case "background-true":
				background := true
				selection.Children[0].Background = &background
			case "calls":
				selection.Children[0].MaxCalls = 2
			case "turns":
				selection.Children[0].MaxTurns = 3
			case "model":
				selection.Children[0].Model = "haiku"
			case "tools":
				selection.Children[0].Tools = []string{"Bash"}
			case "resource":
				selection.Children[0].Resources = []string{"foreign.txt"}
			case "evidence":
				selection.Evidence.Children = []string{"foreign"}
			case "late-prompt":
				selection.Children[0].Prompt = "${{ steps.foreign.outputs.data }}"
			}
			err := admitSkillChildren(plan, selection, expressionContext{Inputs: map[string]any{"target": "original target"}})
			if variant == "valid" {
				if err != nil || len(plan.Children) != 1 || plan.Children[0].Prompt != "Review original target." || plan.Children[0].Type != "aw-child-review" || plan.Children[0].Model != plan.Model {
					t.Fatal(plan, err)
				}
			} else if err == nil {
				t.Fatal("broader or late child authority admitted", variant)
			}
		})
	}
}
