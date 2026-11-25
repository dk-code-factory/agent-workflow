package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureProject(t *testing.T, workflow, fixtures string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sample")
	if err := Init(root, "sample", "1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	if workflow != "" {
		if err := os.WriteFile(filepath.Join(root, "workflows/main.yml"), []byte(workflow), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if fixtures != "" {
		if err := os.WriteFile(filepath.Join(root, "fixtures/example.yaml"), []byte(fixtures), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func fixtureReport(t *testing.T, root string) TestReport {
	t.Helper()
	r := Test(t.Context(), root)
	if !r.Passed {
		b, _ := json.MarshalIndent(r, "", "  ")
		t.Fatalf("fixture failed: %s", b)
	}
	return r
}
func TestPluginFixtureScaffoldUsesInterpreterForBothRuntimes(t *testing.T) {
	for _, provider := range []string{"codex-app-server", "claude-sdk"} {
		t.Run(provider, func(t *testing.T) {
			root := fixtureProject(t, "", fmt.Sprintf(`schema-version: 1
cases:
  - name: success
    skill: sample
    runtime: %s
    inputs: {task: Say hello}
    operations:
      - uses: agent/run
        prompt: Say hello
        result: {text: Hello}
    expect:
      state: succeeded
      outputs: {result: Hello}
      invocation-count: 1
      worker-remaining: [31]
      sessions: 1
`, provider))
			r := fixtureReport(t, root)
			if r.Cases[0].Invocations[0].Runtime != provider {
				t.Fatal(r.Cases)
			}
		})
	}
}
func TestPluginFixtureGateRecoveryRetainsDispatchSessionAndBudget(t *testing.T) {
	workflow := `on:
  workflow_call:
    outputs:
      result: {value: '${{ jobs.main.outputs.result }}'}
jobs:
  main:
    outputs: {result: '${{ steps.next.outputs.text }}'}
    steps:
      - id: before
        uses: agent/run
        with: {agent: agents/task.yaml, prompt: before, turns: 3}
      - id: approval
        uses: input/wait
        with: {prompt: "Approve?", evidence-id: current-evidence}
      - id: next
        if: '${{ steps.approval.outputs.answer.approved }}'
        uses: agent/run
        with: {agent: '${{ steps.before.outputs.agent }}', prompt: after}
`
	for _, provider := range []string{"codex-app-server", "claude-sdk"} {
		t.Run(provider, func(t *testing.T) {
			other := "claude-sdk"
			if provider == "claude-sdk" {
				other = "codex-app-server"
			}
			root := fixtureProject(t, workflow, fmt.Sprintf(`schema-version: 1
cases:
  - name: recovered
    skill: sample
    runtime: %s
    operations:
      - {uses: agent/run, prompt: before, result: {text: first}}
      - {uses: agent/run, prompt: after, result: {text: finished}}
    controls:
      - {action: answer, step: approval, evidence: stale-evidence, value: {approved: true}, expect-error: stale, invocation-count: 1}
      - {action: answer, step: approval, value: null, expect-error: non-null, invocation-count: 1}
      - {action: interrupt, expect-state: interrupted, invocation-count: 1}
      - {action: reopen, invocation-count: 1}
      - {action: configure-runtime, runtime: %s, invocation-count: 1}
      - {action: answer, step: approval, value: {approved: true}, invocation-count: 1}
      - {action: resume, invocation-count: 2, expect-state: succeeded}
      - {action: reopen, invocation-count: 2}
      - {action: resume, invocation-count: 2, expect-state: succeeded}
    expect:
      state: succeeded
      outputs: {result: finished}
      invocation-count: 2
      worker-remaining: [1]
      sessions: 1
`, provider, other))
			r := fixtureReport(t, root)
			calls := r.Cases[0].Invocations
			if len(calls) != 2 || calls[0].Session != calls[1].Session || calls[0].Worker != calls[1].Worker || calls[1].Runtime != provider {
				t.Fatalf("identity/runtime changed: %+v", calls)
			}
		})
	}
}
func TestPluginFixtureDeclineSkipsEffect(t *testing.T) {
	root := fixtureProject(t, `jobs:
  main:
    steps:
      - id: approval
        uses: input/wait
        with: {prompt: "Approve?", evidence-id: proof}
      - id: publish
        if: '${{ steps.approval.outputs.answer.approved }}'
        uses: github/pr
        with: {title: must not publish, path: managed, commit: fake-commit}
`, `schema-version: 1
cases:
  - name: declined
    skill: sample
    controls:
      - {action: answer, step: approval, value: {approved: false}}
      - {action: resume, expect-state: succeeded, invocation-count: 0}
    expect: {state: succeeded, invocation-count: 0}
`)
	fixtureReport(t, root)
}
func TestPluginFixtureRealInputsRejectBeforeDispatch(t *testing.T) {
	root := fixtureProject(t, "", `schema-version: 1
cases:
  - name: missing
    skill: sample
    expect: {error: required input, invocation-count: 0}
  - name: invalid
    skill: sample
    inputs: {task: 42}
    expect: {error: must have type string, invocation-count: 0}
  - name: unknown
    skill: sample
    inputs: {task: yes, extra: value}
    expect: {error: unknown input, invocation-count: 0}
`)
	fixtureReport(t, root)
}
func TestPluginFixtureRejectedAdmissionChecksSessions(t *testing.T) {
	for _, sessions := range []int{0, 1} {
		t.Run(fmt.Sprint(sessions), func(t *testing.T) {
			root := fixtureProject(t, "", fmt.Sprintf(`schema-version: 1
cases:
  - name: rejected
    skill: sample
    expect: {error: required input, invocation-count: 0, sessions: %d}
`, sessions))
			report := Test(t.Context(), root)
			if sessions == 0 {
				if !report.Passed {
					t.Fatal(report)
				}
			} else if report.Passed || len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Reason, "expected 1 retained sessions; got 0") {
				t.Fatalf("rejected admission ignored a retained-session mismatch: %+v", report)
			}
		})
	}
}
func TestPluginFixtureInvalidDiagnosticLocation(t *testing.T) {
	for _, test := range []struct {
		name, source, caseName string
	}{
		{"syntax", "schema-version: [", ""},
		{"schema", "schema-version: 2\ncases: []\n", ""},
		{"case", "schema-version: 1\ncases:\n  - name: bad-state\n    skill: sample\n    expect: {state: completed, invocation-count: 0}\n", "bad-state"},
		{"duplicate", "schema-version: 1\ncases:\n  - {name: repeated, skill: sample, expect: {state: succeeded, invocation-count: 0}}\n  - {name: repeated, skill: sample, expect: {state: succeeded, invocation-count: 0}}\n", "repeated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := Test(t.Context(), fixtureProject(t, "", test.source))
			if report.Passed || len(report.Diagnostics) != 1 {
				t.Fatal(report)
			}
			d := report.Diagnostics[0]
			if d.Code != "fixture.invalid" || d.File != "fixtures/example.yaml" || d.Case != test.caseName {
				t.Fatalf("lost fixture location: %+v", d)
			}
		})
	}
}
func TestPluginFixtureBoundedRepeatAndFailurePropagation(t *testing.T) {
	root := fixtureProject(t, `jobs:
  retry:
    repeat: {max-attempts: 2, until: '${{ steps.work.outputs.done }}'}
    steps:
      - id: work
        argv: [must-not-execute, 'attempt-${{ iteration.number }}']
  downstream:
    needs: retry
    steps:
      - uses: github/pr
        with: {title: must not publish, path: managed, commit: fake-commit}
`, `schema-version: 1
cases:
  - name: exhausted
    skill: sample
    operations:
      - {uses: command/run, call: 1, result: {done: false}}
      - {uses: command/run, call: 2, result: {done: false}}
    controls:
      - {action: reopen, invocation-count: 2}
      - {action: resume, expect-error: repeat exhausted, invocation-count: 2}
    expect: {state: failed, error: repeat exhausted, invocation-count: 2}
`)
	r := fixtureReport(t, root)
	if r.Cases[0].Invocations[1].Command.Argv[1] != "attempt-2" {
		t.Fatal(r.Cases)
	}
}
func TestPluginFixtureCommandsRenderWithoutSpawning(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "must-not-exist")
	root := fixtureProject(t, fmt.Sprintf(`on:
  workflow_call:
    inputs: {task: {type: string, required: true}}
    outputs: {text: {value: '${{ jobs.main.outputs.text }}'}}
jobs:
  main:
    outputs: {text: '${{ steps.command.outputs.stdout }}'}
    steps:
      - id: command
        run: touch %s
        shell: sh
        env: {FIXTURE_TASK: '${{ inputs.task }}'}
        with: {input: {task: '${{ inputs.task }}'}}
`, sentinel), `schema-version: 1
cases:
  - name: rendered
    skill: sample
    inputs: {task: rendered value}
    operations:
      - uses: command/run
        result: {stdout: scripted}
    expect: {state: succeeded, outputs: {text: scripted}, invocation-count: 1}
`)
	r := fixtureReport(t, root)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("developer command executed", err)
	}
	q := r.Cases[0].Invocations[0].Command
	if len(q.Environment) != 1 || q.Environment[0] != "FIXTURE_TASK=rendered value" || string(q.Input) != "{\"task\":\"rendered value\"}\n" {
		t.Fatalf("not rendered: %+v", q)
	}
}
func TestPluginFixtureMissingAmbiguousAndUnusedNeverPassAsExpectedFailure(t *testing.T) {
	for name, operations := range map[string]string{
		"missing":   "[]",
		"ambiguous": "[{uses: agent/run, result: {text: a}}, {uses: agent/run, result: {text: b}}]",
		"unused":    "[{uses: agent/run, result: {text: a}}, {uses: github/pr, result: {url: unused}}]",
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureProject(t, "", fmt.Sprintf("schema-version: 1\ncases:\n  - name: invalid\n    skill: sample\n    inputs: {task: work}\n    operations: %s\n    expect: {error: fixture response, invocation-count: 0}\n", operations))
			r := Test(t.Context(), root)
			if r.Passed || len(r.Diagnostics) == 0 {
				t.Fatal("invalid coverage passed", r)
			}
		})
	}
}
func TestPluginFixtureStrictContractAndEmptyCoverage(t *testing.T) {
	for _, source := range []string{
		"schema-version: 1\ncases: []\n",
		"schema-version: 1\nunknown: true\ncases: []\n",
		"schema-version: 1\ncases: [{name: a, skill: sample, expect: {state: completed, invocation-count: 0}}]\n",
		"schema-version: 1\ncases: [{name: a, skill: sample, expect: {state: succeeded}}]\n",
		"schema-version: 1\ncases: [{name: a, skill: sample, operations: [{uses: arbitrary/live}], expect: {state: succeeded, invocation-count: 0}}]\n",
	} {
		t.Run(source, func(t *testing.T) {
			root := fixtureProject(t, "", source)
			if r := Test(t.Context(), root); r.Passed || r.Diagnostics[0].Code != "fixture.invalid" {
				t.Fatal(r)
			}
		})
	}
}
func TestPluginFixtureNativeOptionsUseRealAdmission(t *testing.T) {
	root := fixtureProject(t, "", `schema-version: 1
cases:
  - name: incompatible
    skill: sample
    runtime: claude-sdk
    inputs: {task: work}
    expect: {error: reasoning-effort, invocation-count: 0}
`)
	p := filepath.Join(root, "workflows/agents/task.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, append(b, []byte("reasoning-effort: high\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureReport(t, root)
}
func TestPluginFixtureFailClosedContainersIncludingHandlersAndChildren(t *testing.T) {
	for name, workflow := range map[string]string{
		"container": "jobs: {main: {container: alpine:latest, steps: [{run: echo unsafe, shell: sh}]}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureProject(t, workflow, `schema-version: 1
cases:
  - name: denied
    skill: sample
    expect: {error: fixture, invocation-count: 0}
`)
			if check := Check(root); !check.Passed {
				t.Fatal("invalid negative fixture package", check)
			}
			r := Test(context.Background(), root)
			if r.Passed {
				t.Fatal("unsupported container accepted")
			}
			if len(r.Cases) > 0 && len(r.Cases[0].Invocations) != 0 {
				t.Fatal("container reached effects", r)
			}
		})
	}
}
func TestPluginCheckSharedClosureRejectsNativeFormatsAndPreservesWorkspace(t *testing.T) {
	root := fixtureProject(t, "", "")
	before, err := os.ReadFile(filepath.Join(root, "plugin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := filepath.Join(root, "workflows/main.yml")
	b, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(workflow, []byte(strings.ReplaceAll(string(b), "agents/task.yaml", "agents/task.toml")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "workflows/agents/task.toml"), []byte("name='task'"), 0600); err != nil {
		t.Fatal(err)
	}
	report := Check(root)
	if report.Passed || !strings.Contains(report.Diagnostics[0].Reason, "convert") {
		t.Fatal(report)
	}
	after, err := os.ReadFile(filepath.Join(root, "plugin.yaml"))
	if err != nil || string(before) != string(after) {
		t.Fatal("check changed source", err)
	}
}

func TestPluginFixtureRegisteredActionCheckpointValidationWithoutReplay(t *testing.T) {
	root := fixtureProject(t, `on:
  workflow_call:
    outputs: {receipt: {value: '${{ jobs.main.outputs.receipt }}'}}
jobs:
  main:
    outputs: {receipt: '${{ steps.publish.outputs.receipt }}'}
    steps:
      - id: publish
        uses: action/publish
        with: {title: candidate}
`, `schema-version: 1
cases:
  - name: retained-receipt
    skill: sample
    actions: {publish: external_mutation}
    operations:
      - {uses: action/publish, phase: bindings, call: 1, result: {destination: fixture}}
      - {uses: action/publish, phase: execute, result: {receipt: issued}, evidence: {candidate: fixture-candidate}, receipt: {id: fixture-receipt}}
      - {uses: action/publish, phase: validate, call: 1}
      - {uses: action/publish, phase: bindings, call: 2, result: {destination: fixture}}
    controls:
      - {action: reopen, invocation-count: 1}
      - {action: resume, expect-state: succeeded, invocation-count: 1}
    expect: {state: succeeded, outputs: {receipt: issued}, invocation-count: 1}
`)
	fixtureReport(t, root)
}

func TestPluginFixtureInvalidRetainedEvidenceFailsRecovery(t *testing.T) {
	root := fixtureProject(t, `jobs:
  main:
    steps:
      - id: publish
        uses: action/publish
        with: {title: candidate}
`, `schema-version: 1
cases:
  - name: invalid-evidence
    skill: sample
    actions: {publish: external_mutation}
    operations:
      - {uses: action/publish, phase: bindings, call: 1, result: {destination: fixture}}
      - {uses: action/publish, result: {receipt: issued}, evidence: {candidate: fixture-candidate}, receipt: {id: fixture-receipt}}
      - {uses: action/publish, phase: bindings, call: 2, result: {destination: fixture}}
      - {uses: action/publish, phase: validate, error: stale checkpoint evidence}
    controls:
      - {action: reopen, invocation-count: 1}
      - {action: resume, expect-error: stale checkpoint evidence, invocation-count: 1}
    expect: {state: succeeded, error: stale checkpoint evidence, invocation-count: 1}
`)
	fixtureReport(t, root)
}

func TestPluginCheckRejectsGeneratedPathCollisions(t *testing.T) {
	root := fixtureProject(t, "", "")
	descriptor := filepath.Join(root, "plugin.yaml")
	data, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(descriptor, append(data, []byte("resources: [compatibility.json]\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "compatibility.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	report := Check(root)
	if report.Passed || report.Diagnostics[0].File != "compatibility.json" || !strings.Contains(report.Diagnostics[0].Reason, "generated package path collides") {
		t.Fatal(report)
	}
}
func TestPluginFixtureNativeConvertersFeedCanonicalAdmission(t *testing.T) {
	for _, from := range []string{"codex", "claude"} {
		t.Run(from, func(t *testing.T) {
			root := fixtureProject(t, "", "")
			source := filepath.Join(t.TempDir(), "source")
			text := "name = 'task'\ndescription = 'Complete task'\ndeveloper_instructions = 'Work offline.'\n"
			if from == "claude" {
				text = "---\nname: task\ndescription: Complete task\n---\nWork offline.\n"
			}
			if err := os.WriteFile(source, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "workflows/agents/task.yaml")
			if err := os.Remove(output); err != nil {
				t.Fatal(err)
			}
			if _, err := workflows.ConvertAgent(from, source, output, nil); err != nil {
				t.Fatal(err)
			}
			fixtureReport(t, root)
			original, err := os.ReadFile(source)
			if err != nil || string(original) != text {
				t.Fatal("converter changed source", err)
			}
		})
	}
}

func TestPluginFixtureUnconfiguredFinallyCommandFailsClosed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "unsafe")
	root := fixtureProject(t, fmt.Sprintf(`jobs:
  main:
    steps:
      - uses: agent/run
        with: {agent: agents/task.yaml, prompt: work}
        finally:
          - argv: [touch, '%s']
`, marker), `schema-version: 1
cases:
  - name: denied-handler
    skill: sample
    operations: [{uses: agent/run, result: {text: done}}]
    expect: {state: failed, error: fixture response, invocation-count: 1}
`)
	report := Test(t.Context(), root)
	if report.Passed || len(report.Cases) != 1 || report.Cases[0].InvocationCount != 1 || !strings.Contains(report.Diagnostics[0].Reason, "command/run") {
		t.Fatal(report)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("finally handler executed", err)
	}
}

func TestPluginFixtureClaudeOnlyEnvironmentDoesNotDiscoverExecutables(t *testing.T) {
	tools := t.TempDir()
	marker := filepath.Join(t.TempDir(), "must-not-launch-claude")
	if err := os.WriteFile(filepath.Join(tools, "claude"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 90\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-read-fixture-credential")
	root := fixtureProject(t, "", `schema-version: 1
cases:
  - name: claude-only
    skill: sample
    runtime: claude-sdk
    inputs: {task: work}
    operations: [{uses: agent/run, result: {text: Hello}}]
    expect: {state: succeeded, outputs: {result: Hello}, invocation-count: 1}
`)
	report := fixtureReport(t, root)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("fixture launched host executable", err)
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "must-not-read-fixture-credential") {
		t.Fatal("fixture read provider credentials")
	}
}
