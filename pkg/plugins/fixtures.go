package plugins

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

const maxFixtureCases = 100
const maxFixtureOperations = 1000

// FixtureSuite is a strict versioned description of responses and assertions.
// It describes boundary data; the existing engine interprets workflow YAML.
type FixtureSuite struct {
	SchemaVersion int           `yaml:"schema-version"`
	Cases         []FixtureCase `yaml:"cases"`
}
type FixtureCase struct {
	Name       string             `yaml:"name"`
	Skill      string             `yaml:"skill"`
	Runtime    string             `yaml:"runtime,omitempty"`
	Inputs     map[string]any     `yaml:"inputs,omitempty"`
	Actions    map[string]string  `yaml:"actions,omitempty"`
	Operations []FixtureOperation `yaml:"operations,omitempty"`
	Controls   []FixtureControl   `yaml:"controls,omitempty"`
	Expect     FixtureExpect      `yaml:"expect"`
}
type FixtureOperation struct {
	Uses     string         `yaml:"uses"`
	Phase    string         `yaml:"phase,omitempty"`
	Call     int            `yaml:"call,omitempty"`
	Job      string         `yaml:"job,omitempty"`
	Step     string         `yaml:"step,omitempty"`
	Prompt   string         `yaml:"prompt,omitempty"`
	Role     string         `yaml:"role,omitempty"`
	Result   map[string]any `yaml:"result,omitempty"`
	Error    string         `yaml:"error,omitempty"`
	Evidence map[string]any `yaml:"evidence,omitempty"`
	Receipt  map[string]any `yaml:"receipt,omitempty"`
	Outcome  string         `yaml:"outcome,omitempty"`
}
type FixtureControl struct {
	Action          string `yaml:"action"`
	Step            string `yaml:"step,omitempty"`
	Value           any    `yaml:"value,omitempty"`
	Evidence        string `yaml:"evidence,omitempty"`
	AnswerID        string `yaml:"answer-id,omitempty"`
	Retry           bool   `yaml:"retry,omitempty"`
	Runtime         string `yaml:"runtime,omitempty"`
	ExpectError     string `yaml:"expect-error,omitempty"`
	ExpectState     string `yaml:"expect-state,omitempty"`
	InvocationCount *int   `yaml:"invocation-count,omitempty"`
}
type FixtureExpect struct {
	State           string         `yaml:"state,omitempty"`
	Error           string         `yaml:"error,omitempty"`
	InvocationCount *int           `yaml:"invocation-count"`
	Outputs         map[string]any `yaml:"outputs,omitempty"`
	WorkerRemaining []int          `yaml:"worker-remaining,omitempty"`
	Sessions        *int           `yaml:"sessions,omitempty"`
}
type fixtureSource struct {
	file string
	test FixtureCase
}

func fixtureState(value string) bool {
	switch value {
	case workflows.RunStatusSucceeded, workflows.RunStatusFailed, workflows.RunStatusSkipped, workflows.RunStatusCanceled, workflows.RunStatusNeedsAttention, workflows.RunStatusWaiting, workflows.RunStatusInterrupted:
		return true
	}
	return false
}
func fixtureRuntime(value string) bool {
	return value == "" || value == "codex-app-server" || value == "claude-sdk"
}

func readFixtures(root string, captured *CapturedPackage) ([]fixtureSource, error) {
	c := newCollector(root)
	if err := c.resource("fixtures"); err != nil {
		return nil, fmt.Errorf("fixtures: %w", err)
	}
	paths := make([]string, 0)
	for name, f := range c.files {
		if !f.mode.IsDir() && (path.Ext(name) == ".yaml" || path.Ext(name) == ".yml") {
			paths = append(paths, name)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 || len(paths) > 256 {
		return nil, fmt.Errorf("fixtures: requires 1–256 YAML fixture files")
	}
	var out []fixtureSource
	names, coverage := map[string]bool{}, map[string]bool{}
	for _, file := range paths {
		data := c.files[file].data
		if len(data) > 1<<20 {
			return nil, fmt.Errorf("%s: fixture exceeds 1 MiB", file)
		}
		var suite FixtureSuite
		if err := strictYAML(data, &suite); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if suite.SchemaVersion != 1 || len(suite.Cases) == 0 {
			return nil, fmt.Errorf("%s: schema-version 1 and nonempty cases are required; author behavior for imported workflows", file)
		}
		for _, test := range suite.Cases {
			if err := validateFixture(test, captured); err != nil {
				return nil, fmt.Errorf("%s case %q: %w", file, test.Name, err)
			}
			if names[test.Name] {
				return nil, fmt.Errorf("%s: duplicate case name %q", file, test.Name)
			}
			names[test.Name], coverage[test.Skill] = true, true
			out = append(out, fixtureSource{file, test})
			if len(out) > maxFixtureCases {
				return nil, fmt.Errorf("fixtures: exceeds %d cases", maxFixtureCases)
			}
		}
	}
	for _, skill := range captured.Descriptor().Skills {
		if !coverage[skill.Name] {
			return nil, fmt.Errorf("fixtures: skill %q has no behavioral cases", skill.Name)
		}
	}
	return out, nil
}
func validateFixture(c FixtureCase, captured *CapturedPackage) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if _, err := captured.Binding(c.Skill); err != nil {
		return err
	}
	if !fixtureRuntime(c.Runtime) {
		return fmt.Errorf("runtime must be codex-app-server or claude-sdk")
	}
	if c.Expect.State != "" && !fixtureState(c.Expect.State) {
		return fmt.Errorf("expect.state must use an engine state, such as succeeded; got %q", c.Expect.State)
	}
	if c.Expect.State == "" && c.Expect.Error == "" {
		return fmt.Errorf("expect requires state or error")
	}
	if c.Expect.InvocationCount == nil || *c.Expect.InvocationCount < 0 {
		return fmt.Errorf("expect.invocation-count must be a nonnegative integer")
	}
	if c.Expect.Sessions != nil && *c.Expect.Sessions < 0 {
		return fmt.Errorf("expect.sessions must be nonnegative")
	}
	for _, remaining := range c.Expect.WorkerRemaining {
		if remaining < 0 {
			return fmt.Errorf("worker-remaining values must be nonnegative")
		}
	}
	if len(c.Operations) > maxFixtureOperations || len(c.Controls) > 128 {
		return fmt.Errorf("case exceeds 1000 operations or 128 controls")
	}
	for name, effect := range c.Actions {
		if strings.TrimSpace(name) == "" || (effect != string(workflows.ActionReadOnly) && effect != string(workflows.ActionExternalMutation)) {
			return fmt.Errorf("actions requires valid names and read_only or external_mutation effects")
		}
	}
	for _, op := range c.Operations {
		application := strings.HasPrefix(op.Uses, "action/")
		if !application && op.Phase != "" && op.Phase != "execute" {
			return fmt.Errorf("only registered fixture actions support bindings/validate/reconcile phases")
		}
		if op.Uses == "agent/run" && (op.Job != "" || op.Step != "") {
			return fmt.Errorf("native fixture operations use prompt/role/call selectors, not job/step")
		}
		if op.Uses != "agent/run" && (op.Role != "" || op.Prompt != "") {
			return fmt.Errorf("prompt and role selectors require agent/run")
		}
		if !application && (op.Evidence != nil || op.Receipt != nil || op.Outcome != "") {
			return fmt.Errorf("evidence/receipt/outcome require registered fixture actions")
		}
		if op.Phase == "validate" && (op.Result != nil || op.Evidence != nil || op.Receipt != nil || op.Outcome != "") {
			return fmt.Errorf("validate responses support selectors and error only")
		}
		if op.Phase == "bindings" && (op.Evidence != nil || op.Receipt != nil || op.Outcome != "") {
			return fmt.Errorf("bindings responses support result/error only")
		}
		if op.Outcome != "" && op.Phase != "reconcile" {
			return fmt.Errorf("outcome requires the reconcile phase")
		}
		if op.Uses == "" || op.Call < 0 {
			return fmt.Errorf("operations require uses and a nonnegative call selector")
		}
		if op.Phase != "" && op.Phase != "execute" && op.Phase != "bindings" && op.Phase != "validate" && op.Phase != "reconcile" {
			return fmt.Errorf("unsupported operation phase %q", op.Phase)
		}
		if strings.HasPrefix(op.Uses, "action/") {
			if _, ok := c.Actions[strings.TrimPrefix(op.Uses, "action/")]; !ok {
				return fmt.Errorf("%s requires an explicit fixture actions contract", op.Uses)
			}
		} else if op.Uses != "agent/run" && op.Uses != "command/run" && !workflows.IsControllerAction(op.Uses) {
			return fmt.Errorf("unsupported fixture operation %q", op.Uses)
		}
	}
	for _, control := range c.Controls {
		if control.Action == "answer" && control.Step == "" {
			return fmt.Errorf("answer controls require a gate step")
		}
		if control.Action != "answer" && (control.Step != "" || control.Value != nil || control.Evidence != "" || control.AnswerID != "") {
			return fmt.Errorf("step/value/evidence/answer-id require an answer control")
		}
		if control.Retry && control.Action != "resume" {
			return fmt.Errorf("retry requires a resume control")
		}
		if (control.Runtime != "") != (control.Action == "configure-runtime") {
			return fmt.Errorf("runtime is required only for configure-runtime controls")
		}
		switch control.Action {
		case "answer", "resume", "interrupt", "reopen", "configure-runtime":
		default:
			return fmt.Errorf("unsupported control %q", control.Action)
		}
		if control.ExpectState != "" && !fixtureState(control.ExpectState) {
			return fmt.Errorf("control expect-state must use an engine state")
		}
		if !fixtureRuntime(control.Runtime) {
			return fmt.Errorf("unsupported control runtime")
		}
		if control.InvocationCount != nil && *control.InvocationCount < 0 {
			return fmt.Errorf("control invocation-count must be nonnegative")
		}
	}
	return nil
}
