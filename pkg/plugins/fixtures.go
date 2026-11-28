package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"gopkg.in/yaml.v3"
)

// Fixture integers must be authored as YAML integers. yaml.v3 otherwise truncates
// floating-point scalars when decoding directly into Go integer fields.
type fixtureInteger int

func (value *fixtureInteger) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return fmt.Errorf("fixture field requires a YAML integer; got %q", node.Value)
	}
	var integer int
	if err := node.Decode(&integer); err != nil {
		return err
	}
	*value = fixtureInteger(integer)
	return nil
}

const maxFixtureCases = 100
const maxFixtureOperations = 1000

// FixtureSuite is a strict versioned description of responses and assertions.
// It describes boundary data; the existing engine interprets workflow YAML.
type FixtureSuite struct {
	SchemaVersion fixtureInteger `yaml:"schema-version"`
	Cases         []FixtureCase  `yaml:"cases"`
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
	Call     fixtureInteger `yaml:"call,omitempty"`
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
	Action          string          `yaml:"action"`
	Step            string          `yaml:"step,omitempty"`
	Value           any             `yaml:"value,omitempty"`
	Evidence        string          `yaml:"evidence,omitempty"`
	AnswerID        string          `yaml:"answer-id,omitempty"`
	Retry           bool            `yaml:"retry,omitempty"`
	Runtime         string          `yaml:"runtime,omitempty"`
	ExpectError     string          `yaml:"expect-error,omitempty"`
	ExpectState     string          `yaml:"expect-state,omitempty"`
	InvocationCount *fixtureInteger `yaml:"invocation-count,omitempty"`
}
type FixtureExpect struct {
	State           string           `yaml:"state,omitempty"`
	Error           string           `yaml:"error,omitempty"`
	InvocationCount *fixtureInteger  `yaml:"invocation-count"`
	Outputs         map[string]any   `yaml:"outputs,omitempty"`
	WorkerRemaining []fixtureInteger `yaml:"worker-remaining,omitempty"`
	Sessions        *fixtureInteger  `yaml:"sessions,omitempty"`
}
type fixtureSource struct {
	file string
	test FixtureCase
}

type fixtureValidationError struct{ Diagnostic }

func (e *fixtureValidationError) Error() string { return e.Reason }

func invalidFixture(file, caseName string, err error) error {
	return &fixtureValidationError{Diagnostic{Code: "fixture.invalid", File: file, Case: caseName, Reason: err.Error()}}
}

// yaml.v3 bypasses custom unmarshalling for null and can drop null sequence
// elements. Inspect only numeric schema positions before typed decoding; JSON
// boundary data retains its own null values and unknown fields stay strict.
// Case, operation and control entries must also survive as authored mappings.
func fixtureSchemaNodes(document *yaml.Node) (err error) {
	caseName := ""
	defer func() {
		if err != nil && caseName != "" {
			err = invalidFixture("", caseName, err)
		}
	}()
	var merges func(*yaml.Node) error
	merges = func(node *yaml.Node) error {
		if node.Tag == "!!merge" {
			return fmt.Errorf("YAML merges are unsupported in fixtures")
		}
		for _, child := range node.Content {
			if err := merges(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := merges(document); err != nil {
		return err
	}
	field := func(node *yaml.Node, key string) *yaml.Node {
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i+1]
			}
		}
		return nil
	}
	integer := func(node *yaml.Node, location string) error {
		if node != nil && (node.Kind != yaml.ScalarNode || node.Tag != "!!int") {
			return fmt.Errorf("%s requires a YAML integer", location)
		}
		return nil
	}
	if len(document.Content) != 1 {
		return nil // The shared decoder reports an invalid document shape.
	}
	root := document.Content[0]
	if err := integer(field(root, "schema-version"), "schema-version"); err != nil {
		return err
	}
	cases := field(root, "cases")
	if cases == nil || cases.Kind != yaml.SequenceNode {
		return nil
	}
	for i, test := range cases.Content {
		caseName = ""
		if name := field(test, "name"); name != nil && name.Tag == "!!str" {
			caseName = name.Value
		}
		if test.Kind != yaml.MappingNode {
			return fmt.Errorf("cases[%d] requires a fixture case mapping", i)
		}
		for _, group := range []struct{ name, number string }{{"operations", "call"}, {"controls", "invocation-count"}} {
			entries := field(test, group.name)
			if entries == nil {
				continue
			}
			if entries.Kind != yaml.SequenceNode {
				return fmt.Errorf("cases[%d].%s requires a sequence", i, group.name)
			}
			for j, entry := range entries.Content {
				if entry.Kind != yaml.MappingNode {
					return fmt.Errorf("cases[%d].%s[%d] requires a mapping", i, group.name, j)
				}
				if err := integer(field(entry, group.number), fmt.Sprintf("cases[%d].%s[%d].%s", i, group.name, j, group.number)); err != nil {
					return err
				}
			}
		}
		expect := field(test, "expect")
		for _, name := range []string{"invocation-count", "sessions"} {
			if err := integer(field(expect, name), fmt.Sprintf("cases[%d].expect.%s", i, name)); err != nil {
				return err
			}
		}
		remaining := field(expect, "worker-remaining")
		if remaining != nil {
			if remaining.Kind != yaml.SequenceNode {
				return fmt.Errorf("cases[%d].expect.worker-remaining requires a sequence of YAML integers", i)
			}
			for j, value := range remaining.Content {
				if err := integer(value, fmt.Sprintf("cases[%d].expect.worker-remaining[%d]", i, j)); err != nil {
					return err
				}
			}
		}
	}
	return nil
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
		return nil, invalidFixture("fixtures", "", fmt.Errorf("fixtures: %w", err))
	}
	paths := make([]string, 0)
	for name, f := range c.files {
		if !f.mode.IsDir() && (path.Ext(name) == ".yaml" || path.Ext(name) == ".yml") {
			paths = append(paths, name)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 || len(paths) > 256 {
		return nil, invalidFixture("fixtures", "", fmt.Errorf("fixtures: requires 1–256 YAML fixture files"))
	}
	var out []fixtureSource
	names, coverage := map[string]bool{}, map[string]bool{}
	for _, file := range paths {
		data := c.files[file].data
		if len(data) > 1<<20 {
			return nil, invalidFixture(file, "", fmt.Errorf("%s: fixture exceeds 1 MiB", file))
		}
		var suite FixtureSuite
		if err := strictYAML(data, &suite, fixtureSchemaNodes); err != nil {
			var invalid *fixtureValidationError
			caseName := ""
			if errors.As(err, &invalid) {
				caseName = invalid.Case
			}
			return nil, invalidFixture(file, caseName, fmt.Errorf("%s: %w", file, err))
		}
		if suite.SchemaVersion != 1 || len(suite.Cases) == 0 {
			return nil, invalidFixture(file, "", fmt.Errorf("%s: schema-version 1 and nonempty cases are required; author behavior for imported workflows", file))
		}
		for _, test := range suite.Cases {
			if err := validateFixture(test, captured); err != nil {
				return nil, invalidFixture(file, test.Name, fmt.Errorf("%s case %q: %w", file, test.Name, err))
			}
			if names[test.Name] {
				return nil, invalidFixture(file, test.Name, fmt.Errorf("%s: duplicate case name %q", file, test.Name))
			}
			names[test.Name], coverage[test.Skill] = true, true
			out = append(out, fixtureSource{file, test})
			if len(out) > maxFixtureCases {
				return nil, invalidFixture(file, test.Name, fmt.Errorf("fixtures: exceeds %d cases", maxFixtureCases))
			}
		}
	}
	for _, skill := range captured.Descriptor().Skills {
		if !coverage[skill.Name] {
			return nil, invalidFixture("fixtures", "", fmt.Errorf("fixtures: skill %q has no behavioral cases", skill.Name))
		}
	}
	return out, nil
}
func validateFixture(c FixtureCase, captured *CapturedPackage) error {
	if err := fixtureUTF8(reflect.ValueOf(c), "case"); err != nil {
		return err
	}
	// All authored boundary values must survive JSON transport unchanged. Check
	// the whole suite before any case executes, including expected negative cases.
	type jsonField struct {
		name  string
		value any
	}
	values := []jsonField{{"inputs", c.Inputs}, {"expect.outputs", c.Expect.Outputs}}
	for i, operation := range c.Operations {
		for _, field := range []jsonField{{"result", operation.Result}, {"evidence", operation.Evidence}, {"receipt", operation.Receipt}} {
			values = append(values, jsonField{fmt.Sprintf("operations[%d].%s", i, field.name), field.value})
		}
	}
	for i, control := range c.Controls {
		values = append(values, jsonField{fmt.Sprintf("controls[%d].value", i), control.Value})
	}
	for _, field := range values {
		if _, err := json.Marshal(field.value); err != nil {
			return fmt.Errorf("%s must be JSON representable: %w", field.name, err)
		}
	}

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
		helper := op.Uses == workflows.ContainerActionV1 || strings.HasPrefix(op.Uses, "docker://")
		structured := application || helper
		if !structured && op.Phase != "" && op.Phase != "execute" {
			return fmt.Errorf("only fixture actions and helpers support recovery phases")
		}
		if op.Uses == "agent/run" && (op.Job != "" || op.Step != "") {
			return fmt.Errorf("native fixture operations use prompt/role/call selectors, not job/step")
		}
		if op.Uses != "agent/run" && (op.Role != "" || op.Prompt != "") {
			return fmt.Errorf("prompt and role selectors require agent/run")
		}
		if !structured && (op.Evidence != nil || op.Receipt != nil || op.Outcome != "") {
			return fmt.Errorf("evidence/receipt/outcome require fixture actions or helpers")
		}
		if (op.Phase == "validate" || op.Phase == "validate_checkpoint") && (op.Result != nil || op.Evidence != nil || op.Receipt != nil || op.Outcome != "") {
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
		if op.Phase != "" && op.Phase != "execute" && op.Phase != "bindings" && op.Phase != "validate" && op.Phase != "validate_checkpoint" && op.Phase != "reconcile" {
			return fmt.Errorf("unsupported operation phase %q", op.Phase)
		}
		if (application && op.Phase == "validate_checkpoint") || (helper && op.Phase == "validate") {
			return fmt.Errorf("registered actions use validate; helpers use validate_checkpoint")
		}
		if strings.HasPrefix(op.Uses, "action/") {
			if _, ok := c.Actions[strings.TrimPrefix(op.Uses, "action/")]; !ok {
				return fmt.Errorf("%s requires an explicit fixture actions contract", op.Uses)
			}
		} else if op.Uses != "agent/run" && op.Uses != "command/run" && !helper && !workflows.IsControllerAction(op.Uses) {
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

// JSON replaces invalid UTF-8 strings and keys without returning an error.
// Inspect decoded fixture data without converting numbers or JSON null values.
func fixtureUTF8(value reflect.Value, location string) error {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("%s contains invalid UTF-8", location)
		}
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			return fixtureUTF8(value.Elem(), location)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if err := fixtureUTF8(value.Field(i), location+"."+name); err != nil {
				return err
			}
		}
	case reflect.Map:
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprintf("%T:%#v", keys[i].Interface(), keys[i].Interface()) < fmt.Sprintf("%T:%#v", keys[j].Interface(), keys[j].Interface())
		})
		for _, key := range keys {
			if err := fixtureUTF8(key, location+".<key>"); err != nil {
				return err
			}
			if err := fixtureUTF8(value.MapIndex(key), location+".<value>"); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := fixtureUTF8(value.Index(i), fmt.Sprintf("%s[%d]", location, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
