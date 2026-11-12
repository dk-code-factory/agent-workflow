# Pipeline agent tasks

Use `agent/run` with a native Codex agent TOML file or a directory containing
exactly one TOML file. Paths resolve from the workflow that owns the step.
Admission captures the file bytes and effective settings; resume uses that
snapshot even if the file changes.

The optional positive integer literal `with.turns` narrows the initial agent's total turn
allowance, up to the configured runtime limit. It cannot change an existing
agent reference's original allowance or replenish turns on continuation or
recovery. Omit it to use the runtime limit. The editor exposes this as **Agent
turn allowance**. The prompt, including structured-output instructions and the
JSON-encoded typed input, must fit the mailbox's 1 MiB limit.

```yaml
- id: review
  uses: agent/run
  with:
    agent: ../.codex/agents/reviewer.toml
    prompt: Review the changes.
    input: {target: main}
    output:
      format: json
      repair-attempts: 0
      schema:
        type: object
        required: [complete, summary]
        properties:
          complete: {type: boolean}
          summary: {type: string}
  onResult:
    - argv: [python3, scripts/verify-review.py]
  onComplete:
    - argv: [python3, scripts/accept-review.py]
  onIncomplete:
    - argv: [python3, scripts/review-feedback.py]
  onError:
    - uses: agent/run
      with:
        agent: ../.codex/agents/recovery.toml
        prompt: Recover the failed review using the supplied event.
        output:
          format: json
          repair-attempts: 0
          schema:
            type: object
            required: [complete, summary]
            properties:
              complete: {type: boolean}
              summary: {type: string}
  onFailure:
    - argv: [python3, scripts/report-review-error.py]
  finally:
    - argv: [python3, scripts/finalize-review.py]
```

The supported [native Codex agent definition](https://learn.chatgpt.com/docs/agent-configuration/subagents)
subset is `name`, `description`, `developer_instructions`, `model`, and
`model_reasoning_effort` (`none`, `minimal`, `low`, `medium`, `high`, or `xhigh`). The first three fields are required. Unknown settings
are rejected, including tool, provider, authentication, sandbox, permission,
MCP, skill and feature overrides. Example:

```toml
name = "reviewer"
description = "Review changes for correctness."
developer_instructions = "Inspect the changes and report concrete findings."
model_reasoning_effort = "high"
```

The configured `codex` runtime must use `codex-app-server`. It supplies
credentials, execution policy, limits and tool grants. Definitions cannot expand
these boundaries. Codex managed tool subsets and tool-free output repair retain
their existing unsupported status. Set `repair-attempts: 0` for contracts that
must fail validation directly into `onError` without requesting an unsupported
repair turn.

A definition at another step creates an independent agent. Passing
`${{ steps.review.outputs.agent }}` as `with.agent` continues the original
session. References are opaque and confined to their originating run, including
agents created by reusable workflows. Logical workflow, job, step and iteration
coordinates determine identity; physical attempts do not create new agents.
Turns on one agent serialize through its mailbox. Independent agents can run in
ordinary jobs or parallel blocks.

`with.input` carries typed JSON data. Hook events have `type`, `agent`, `result`,
`verification`, `error` and `source` fields. `source` retains the original workflow
trigger. Scripts receive exactly one JSON event on stdin, print diagnostics on
stderr and return typed results on stdout. A script may declare `with.output`
using the same JSON schema contract. Never insert event JSON into a shell script.
AI handlers receive the event through `with.input` by default and can use event
expressions to choose explicit inputs.

Completion hooks require an object result schema with a required boolean
`complete`. Every schema-valid candidate reaches `onResult`; its last successful
handler returns a JSON verdict containing boolean `complete` and optional typed
feedback. With no `onResult`, validation supplies an accepting verdict. Effective
completion requires both the candidate declaration and verdict to be true.
Accepted work reaches `onComplete`. Valid unfinished work reaches `onIncomplete`
and retains `outputs.complete: false` without becoming an execution failure.

An explicitly declared AI handler in `onIncomplete` or `onError` can return a
replacement candidate. The original contract and verification apply again.
The owner does not recursively re-enter either handler list. Additional attempts
require an explicit pipeline repetition bound. Invalid JSON and schema mismatches
enter error recovery after the backend's supported bounded repair behavior;
they never become ordinary incomplete work.

`onError` attempts recovery; `onFailure` reports an unresolved error. Reporting
and finalization failures remain failures. Original candidates, errors and handler
receipts remain in lifecycle and attempt history; unrelated handler output is not
merged into task results. Cancellation and unsafe failures can skip application
hooks. Controller cleanup always stops and joins owned work before terminal
completion and retains session state required by recovery and retention policy.

The internal wait mechanism is documented in [durable message waiting](WORKER_WAITING.md).

An ordered pipeline may explicitly continue a classified provider or usage-limit
failure only after its exact native message has a retained observation receipt.
Unclassified guard failures, failed reporting/finalization, and unresolved effects
still block ordered execution. Application status actions decide optionality and
retry allowance; a continued step never establishes successful task coverage.
