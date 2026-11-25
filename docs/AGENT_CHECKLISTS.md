# Agent checklists

`agent/run` can verify its work with an ordered `with.checklist`. The initial
task runs first. Each stage then runs until it passes or exhausts a retry limit.
All prompt stages and command repairs continue the original native agent session.
Lifecycle hooks remain at step scope and run after checklist execution.

```yaml
name: Implement and verify
on:
  manual: {}
  workflow_call:
    inputs:
      task: {type: string, required: true}
jobs:
  implement:
    steps:
      - id: developer
        uses: agent/run
        with:
          agent: agents/developer.yaml
          prompt: 'Implement this task: ${{ inputs.task }}'
          checklist:
            max-retries: 6
            stages:
              - prompt: |
                  Verify every requirement in the original task.
                  Inspect the actual changes and finish anything missing.
                max-retries: 2
              - argv: [go, test, ./...]
                timeout: 5m
                max-retries: 3
              - shell: sh
                run: git diff --check
                timeout: 1m
                max-retries: 2
        onFailure:
          - argv: [echo, Implementation or checklist failed]
        finally:
          - argv: [echo, Step finished]
```

The definition path resolves relative to the owning workflow. For this example,
`workflows/agents/developer.yaml` can contain:

```yaml
name: "developer"
description: "Implement tasks and complete their verification stages."
instructions: "Follow repository instructions, preserve unrelated changes, run relevant deterministic checks, and report unfinished work accurately. Do not commit, push, or publish."
```

Use the normal configured `codex-app-server` runtime. Checklist turns share the
agent's admitted turn allowance and the execution deadline; retry settings do
not replenish either allowance. No additional provider or permissions are selected.

## Retry limits

The required checklist `max-retries` is a shared budget across all stages. An
optional stage `max-retries` imposes an additional local limit. Both accept literal
integers from 0 to 100. Zero allows the initial check but no retries. The checklist
must contain 1–100 stages. A stage may be a plain prompt string, for example:

```yaml
checklist:
  max-retries: 3
  stages:
    - Verify the requirements and finish missing work.
    - Review the final diff and correct mistakes.
```

Initial stage attempts consume no retries. Each subsequent attempt consumes one
retry from the shared budget and one from that stage's allowance. Moving to the
next stage resets only the local counter. An omitted local limit leaves the stage
subject to the remaining shared budget. Passing on the final allowed retry is
successful. An unfinished stage fails the step when either allowance runs out;
later stages do not start. For example, if two stages use five of six global
retries, the next stage has one retry available even if its local limit is two.

## Prompt and command stages

Each stage declares exactly one form: `prompt`, `argv`, or `run` with an explicit
`shell`. Command stages can also set `working-directory` and a positive `timeout`.
They use ordinary workflow command execution, capacity and containment policies.
Their default directory is the agent task's workspace, or the workflow workspace
when the task does not specify one. Prompt and command strings support ordinary
workflow expressions; retry limits and stage structure are literal policy. Stage
text must be valid UTF-8. Binary-tagged YAML string values are decoded; quote
date-like text to keep it a string.

A prompt stage asks the original agent to verify the whole stage and finish
missing work. The controller requires this JSON response:

```json
{"complete": false, "evidence": ["Inspected the parser changes"], "remaining": ["Handle empty input"]}
```

A valid `complete: false` response causes a bounded follow-up with the original
stage prompt and prior report as typed input. Prompt completion is the agent's
assessment; use a command stage when a deterministic check is available.

A command stage passes only on exit code zero. An ordinary nonzero exit sends
the command, exit code and diagnostics to the original agent for repair. The
controller then reruns the command as the next attempt. The repair response alone
cannot pass the stage. Diagnostic streams in repair input are truncated to 32 KiB
each; full streams remain in the command's artifacts. Scripts run through the
controller, so the agent's native tool restrictions do not replace the configured
workflow command policy.

Invalid structured responses, provider failures, timeouts, cancellation, and
cleanup, storage or ownership failures stop execution rather than entering the
ordinary incomplete-stage retry loop. Recovery retains the normal framework
guards for interrupted or uncertain effects.

## Results, hooks and recovery

The step retains the initial task's ordinary outputs, including `agent`, `text`
and `structured`. `outputs.checklist` records overall completion, global retries
used, and ordered stage summaries with local retries, attempt IDs and outcomes.
Each task turn, stage check and command repair also has its own durable attempt.
Recovery reconstructs stage position and both counters from these receipts;
completed checks and repairs are reused. Retry exhaustion remains terminal on
resume, including `--retry-uncertain`.

The checklist executes before `onResult`, `onComplete`, `onIncomplete`, `onError`,
`onFailure` and `finally`. Completion hooks still require the initial task's
declared completion schema. Their handler deadline begins after checklist work.
`onFailure` and `finally` observe ordinary checklist exhaustion, subject to the
existing unsafe-failure guards. An `onError` replacement cannot bypass required
checklist verification. Existing workflows without a checklist retain their
execution behavior.

The editor exposes **Checklist (YAML)** on native agent steps, validates stage
forms and retry limits, and preserves them through export and Undo/Redo.
