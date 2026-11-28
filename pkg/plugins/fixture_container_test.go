package plugins

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func fixtureHelperWorkflow(docker bool, effect string) (string, string) {
	container, command, uses := "    container: unresolvable-fixture-image\n", "        argv: [never-execute-helper]", "action-v1"
	if docker {
		container, command, uses = "", "        uses: docker://unresolvable-fixture-image\n        with: {entrypoint: never-execute-helper}", "docker://unresolvable-fixture-image"
	}
	return fmt.Sprintf(`on:
  workflow_call:
    outputs: {result: {value: '${{ jobs.main.outputs.result }}'}}
schemas:
  Data: {type: object, required: [value], properties: {value: {type: string}}, additionalProperties: false}
jobs:
  main:
%s    outputs: {result: '${{ steps.helper.outputs.value }}'}
    steps:
      - id: helper
%s
        protocol: action-v1
        effect: %s
        input-schema: Data
        output-schema: Data
        input: {value: submitted}
`, container, command, effect), uses
}

func TestPluginFixtureContainerHelpersUseJournalWithoutDiscovery(t *testing.T) {
	// No executable lookup or container endpoint can succeed in these cases.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///fixture-must-not-discover.sock")
	for _, docker := range []bool{false, true} {
		for _, effect := range []string{"read_only", "external_mutation"} {
			t.Run(fmt.Sprintf("docker=%v/%s", docker, effect), func(t *testing.T) {
				workflow, uses := fixtureHelperWorkflow(docker, effect)
				root := fixtureProject(t, workflow, fmt.Sprintf(`schema-version: 1
cases:
  - name: helper-recovery
    skill: sample
    operations:
      - {uses: '%[1]s', phase: bindings, job: main, step: helper, call: 1, result: {target: fixture}}
      - {uses: '%[1]s', phase: execute, job: main, step: helper, call: 1, result: {value: scripted}, evidence: {candidate: fixture}, receipt: {id: fixture}}
      - {uses: '%[1]s', phase: validate_checkpoint, job: main, step: helper, call: 1}
      - {uses: '%[1]s', phase: bindings, job: main, step: helper, call: 2, result: {target: fixture}}
    controls:
      - {action: reopen, invocation-count: 1}
      - {action: resume, expect-state: succeeded, invocation-count: 1}
    expect: {state: succeeded, outputs: {result: scripted}, invocation-count: 1}
`, uses))
				r := fixtureReport(t, root)
				calls := r.Cases[0].Invocations
				if len(calls) != 4 || calls[0].Phase != "bindings" || calls[1].Phase != "execute" || calls[3].Phase != "validate_checkpoint" || calls[2].Call != 2 {
					t.Fatalf("missing durable recovery transcript: %+v", calls)
				}
			})
		}
	}
}

func TestPluginFixtureContainerHelperSchemasAndEffects(t *testing.T) {
	for _, test := range []struct {
		name, old, replacement, result, expected string
		calls                                    int
	}{
		{"output", "", "", "{value: 42}", "has type", 1},
		{"input", "input: {value: submitted}", "input: {value: 42}", "{value: good}", "input schema", 0},
		{"effect", "effect: read_only", "effect: unknown", "{value: good}", "explicit read_only or external_mutation", 0},
		{"schema", "output-schema: Data", "output-schema: Missing", "{value: good}", "unknown workflow-local schema", 0},
		{"container", "container: unresolvable-fixture-image", "container: '${{ inputs.image }}'", "{value: good}", "static image", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, uses := fixtureHelperWorkflow(false, "read_only")
			if test.old != "" {
				workflow = strings.Replace(workflow, test.old, test.replacement, 1)
			}
			operations := ""
			if test.calls > 0 {
				operations = fmt.Sprintf("    operations:\n      - {uses: '%s', phase: bindings, result: {}}\n      - {uses: '%s', result: %s}\n", uses, uses, test.result)
			}
			root := fixtureProject(t, workflow, fmt.Sprintf("schema-version: 1\ncases:\n  - name: invalid\n    skill: sample\n%s    expect: {error: '%s', invocation-count: %d}\n", operations, test.expected, test.calls))
			report := Test(t.Context(), root)
			if test.name == "output" || test.name == "input" {
				if !report.Passed {
					t.Fatalf("expected schema failure was not observed: %+v", report)
				}
			} else if report.Passed || len(report.Diagnostics) == 0 || !strings.Contains(report.Diagnostics[0].Reason, test.expected) {
				t.Fatalf("invalid declaration admitted: %+v", report)
			}
		})
	}
}

func TestPluginFixtureContainerMissingResponseFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, docker := range []bool{false, true} {
		workflow, uses := fixtureHelperWorkflow(docker, "external_mutation")
		root := fixtureProject(t, workflow, fmt.Sprintf(`schema-version: 1
cases:
  - name: missing-execute
    skill: sample
    operations:
      - {uses: '%s', phase: bindings, result: {}}
    expect: {error: matched 0 operations, invocation-count: 0}
`, uses))
		r := Test(t.Context(), root)
		if r.Passed || len(r.Cases) != 1 || len(r.Cases[0].Invocations) != 1 || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Reason, "matched 0 operations") {
			t.Fatalf("missing helper response escaped: %+v", r)
		}
	}
}

func TestPluginFixtureContainerCommandsAndDockerSelectors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	root := fixtureProject(t, `on:
  workflow_call:
    outputs: {result: {value: '${{ jobs.main.outputs.result }}'}}
jobs:
  main:
    container: unresolvable-fixture-image
    outputs: {result: '${{ steps.docker.outputs.stdout }}'}
    steps:
      - id: command
        run: never-run-this
      - id: docker
        uses: docker://unresolvable-fixture-image
        with: {args: never-run-this}
`, `schema-version: 1
cases:
  - name: container-commands
    skill: sample
    operations:
      - {uses: command/run, job: main, step: command, result: {stdout: scripted command}}
      - {uses: 'docker://unresolvable-fixture-image', job: main, step: docker, result: {stdout: scripted docker}}
    controls:
      - {action: reopen}
      - {action: resume, expect-state: succeeded, invocation-count: 2}
    expect: {state: succeeded, invocation-count: 2, outputs: {result: scripted docker}}
`)
	r := fixtureReport(t, root)
	if r.Cases[0].Invocations[0].Command.Argv[0] != "sh" || r.Cases[0].Invocations[1].Uses != "docker://unresolvable-fixture-image" {
		t.Fatal(r.Cases)
	}
}

func TestPluginFixtureContainerHelperRejectsStaleCheckpoint(t *testing.T) {
	workflow, uses := fixtureHelperWorkflow(false, "external_mutation")
	root := fixtureProject(t, workflow, fmt.Sprintf(`schema-version: 1
cases:
  - name: stale-helper
    skill: sample
    operations:
      - {uses: '%[1]s', phase: bindings, call: 1, result: {target: fixture}}
      - {uses: '%[1]s', result: {value: issued}, evidence: {candidate: fixture}}
      - {uses: '%[1]s', phase: bindings, call: 2, result: {target: fixture}}
      - {uses: '%[1]s', phase: validate_checkpoint, error: stale helper evidence}
    controls:
      - {action: reopen}
      - {action: resume, expect-error: stale helper evidence, invocation-count: 1}
    expect: {state: succeeded, error: stale helper evidence, invocation-count: 1}
`, uses))
	fixtureReport(t, root)
}

func TestPluginFixtureContainerHelperReconcilesWithoutReplay(t *testing.T) {
	workflow, uses := fixtureHelperWorkflow(true, "external_mutation")
	root := fixtureProject(t, workflow, fmt.Sprintf(`schema-version: 1
cases:
  - name: reconciled-helper
    skill: sample
    operations:
      - {uses: '%[1]s', phase: bindings, call: 1, result: {target: fixture}}
      - {uses: '%[1]s', result: {value: issued}, error: lost helper response}
      - {uses: '%[1]s', phase: bindings, call: 2, result: {target: fixture}}
      - {uses: '%[1]s', phase: bindings, call: 3, result: {target: fixture}}
      - {uses: '%[1]s', phase: reconcile, outcome: completed, result: {value: recovered}, evidence: {candidate: fixture}, receipt: {id: fixture}}
      - {uses: '%[1]s', phase: validate_checkpoint}
    controls:
      - {action: reopen}
      - {action: resume, retry: true, expect-state: succeeded, invocation-count: 1}
    expect: {state: succeeded, outputs: {result: recovered}, invocation-count: 1}
`, uses))
	r := fixtureReport(t, root)
	calls := r.Cases[0].Invocations
	if len(calls) != 6 || calls[4].Phase != "reconcile" || calls[5].Phase != "validate_checkpoint" {
		t.Fatal(calls)
	}
}

func TestPluginFixtureContainerHelperMatchesJobStepAndCall(t *testing.T) {
	workflow, uses := fixtureHelperWorkflow(false, "read_only")
	workflow += `      - id: second
        argv: [never-execute-helper]
        protocol: action-v1
        effect: read_only
        input-schema: Data
        output-schema: Data
        input: {value: '${{ steps.helper.outputs.value }}'}
`
	workflow = strings.Replace(workflow, "steps.helper.outputs.value }}'}", "steps.second.outputs.value }}'}", 1)
	fixtures := fmt.Sprintf(`schema-version: 1
cases:
  - name: helper-selectors
    skill: sample
    operations:
      - {uses: '%[1]s', phase: bindings, job: main, step: second, call: 2, result: {}}
      - {uses: '%[1]s', phase: execute, job: main, step: second, call: 2, result: {value: second}}
      - {uses: '%[1]s', phase: execute, job: main, step: helper, call: 1, result: {value: first}}
      - {uses: '%[1]s', phase: bindings, job: main, step: helper, call: 1, result: {}}
    expect: {state: succeeded, outputs: {result: second}, invocation-count: 2}
`, uses)
	r := fixtureReport(t, fixtureProject(t, workflow, fixtures))
	calls := r.Cases[0].Invocations
	if len(calls) != 4 || string(calls[3].Input) != `{"value":"first"}` || calls[3].Effect != "read_only" {
		t.Fatal(calls)
	}
	for _, replacement := range [][2]string{{"job: main, step: second, call: 2, result: {value: second}", "job: other, step: second, call: 2, result: {value: second}"}, {"step: second, call: 2, result: {value: second}", "step: wrong, call: 2, result: {value: second}"}, {"step: second, call: 2, result: {value: second}", "step: second, call: 3, result: {value: second}"}} {
		r := Test(t.Context(), fixtureProject(t, workflow, strings.Replace(fixtures, replacement[0], replacement[1], 1)))
		if r.Passed || len(r.Diagnostics) == 0 || !strings.Contains(r.Diagnostics[0].Reason, "matched 0 operations") {
			t.Fatalf("selector mismatch accepted: %+v", r)
		}
	}
}

func TestPluginFixtureDockerDeclarationsRejectWithoutDiscovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, test := range []struct{ with, expected string }{
		{"{entrypoint: ''}", "Docker entrypoint cannot be empty"},
		{`{args: "'unfinished"}`, "unfinished quote"},
	} {
		e, _, ports := boundaryExecutor(t, "jobs: {main: {steps: [{uses: 'docker://fixture-image', with: "+test.with+"}]}}\n")
		if _, err := e.Prepare(t.Context(), workflows.RunRequest{Ref: "workflows/main.yml"}); err == nil || !strings.Contains(err.Error(), test.expected) {
			t.Fatal(err)
		}
		if ports.count() != 0 {
			t.Fatal("invalid Docker declaration reached execution")
		}
	}
}
