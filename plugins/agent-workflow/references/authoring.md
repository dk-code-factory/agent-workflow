# Maintained workflow syntax

This reference targets engine 0.2.2. Validate generated definitions with the
matching packaged engine. Use `exec -- <command>` via the plugin launcher.
Flags precede positional arguments. Engine examples below omit that prefix.

Default paths: `workflows/<name>.yml`, `workflows/agents/<role>.toml` and an
existing repository `agents.yml`. Agent TOML paths resolve from their owning
workflow. Reusable workflow paths resolve from the workspace and stay inside
`workflows/` directories. Preserve repository conventions when they differ.

The runtime configuration must include a `codex` role using `codex-app-server`:

```yaml
agents:
  codex:
    provider: codex-app-server
    authentication: {mode: native}
    sandbox: read-only
```

Use `workspace-write` only for an approved editing task. Native authentication
uses the selected provider-owned route; never copy credentials into workflow
files. Runtime permissions and budgets belong to configuration, not agent TOML.

```toml
name = "reviewer"
description = "Inspect the assigned repository scope."
developer_instructions = "Inspect files without edits or publication. Return the requested structured findings."
```

These three fields are required. Optional fields: `model` and
`model_reasoning_effort` (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`).
Do not add sandbox, provider, tools, authentication or permission fields to TOML.

## Independent jobs with a dependent result

```yaml
name: Parallel audit
on:
  manual: {}
  workflow_call:
    inputs:
      scope: {type: string, required: true}
jobs:
  correctness:
    outputs: {findings: '${{ steps.review.outputs.structured }}'}
    steps:
      - id: review
        uses: agent/run
        with:
          agent: agents/reviewer.toml
          turns: 4
          prompt: 'Review correctness without edits: ${{ inputs.scope }}'
          output:
            format: json
            repair-attempts: 0
            schema:
              type: object
              required: [findings]
              additionalProperties: false
              properties:
                findings: {type: array, items: {type: string}}
  security:
    outputs: {findings: '${{ steps.review.outputs.structured }}'}
    steps:
      - id: review
        uses: agent/run
        with:
          agent: agents/reviewer.toml
          turns: 4
          prompt: 'Review security without edits: ${{ inputs.scope }}'
          output:
            format: json
            repair-attempts: 0
            schema:
              type: object
              required: [findings]
              additionalProperties: false
              properties:
                findings: {type: array, items: {type: string}}
  combine:
    needs: [correctness, security]
    steps:
      - uses: agent/run
        with:
          agent: agents/reviewer.toml
          turns: 4
          prompt: 'Combine findings: ${{ needs.correctness.outputs.findings }}; ${{ needs.security.outputs.findings }}'
```

Jobs without dependencies may overlap up to configured host capacity. For
independent steps within one job, use a `parallel:` list; `sequence:` orders
children. Sibling branches cannot read in-flight results from each other.

## Commands, bounded repair and structured results

Command leaves use `argv: [python3, -m, unittest]`. Shell leaves require both
`shell: sh` and `run: ...`. Use `working-directory` and positive `timeout` values.
Never interpolate untrusted input into shell source. Command outputs include
`exit_code`, `stdout`, `stderr`. Agent outputs include `text`, `structured`,
`agent` (an opaque session handle) and `complete` for completion contracts.

```yaml
jobs:
  repair:
    repeat: {max-attempts: 2, until: '${{ steps.test.outputs.exit_code == 0 }}'}
    steps:
      - id: implement
        uses: agent/run
        with:
          agent: agents/fixer.toml
          turns: 4
          prompt: Repair only the approved fixture. Do not publish.
      - id: test
        argv: [python3, -m, unittest, discover, -s, tests]
        timeout: 1m
        continue-on-error: true
```

`max-attempts` is a hard bound; exhausting it fails. A false `complete` result
does not automatically retry. Resume preserves admitted budgets. Scope test
failure handling to the command whose nonzero exit is intentionally inspected.
Do not catch integrity, ownership or storage failures as ordinary test failures.

Schema supports a bounded JSON subset: object/array/string/boolean/integer/number,
properties, required, additionalProperties, items, enum and supported bounds.
Avoid `$ref` and schema composition. Use `repair-attempts: 0` for native agents
when automatic output repair is unsupported. Cross-job references need a
declared direct dependency; within a job reference preceding completed steps.

Conditions use the words `and` and `or` (with `and` taking precedence), **not**
`&&` or `||`. Comparisons use spaced `==`, `!=`, `<`, `<=`, `>` or `>=`.
For example, `${{ steps.approval.outputs.answer != "approve" and steps.approval.outputs.answer != "decline" }}`.
This expression language is not GitHub Actions or JavaScript. Structural
`validate` does not execute every condition: test branch behavior with local
fixtures, including approval, decline and invalid answers, before claiming it
works. Helper-only tests cannot establish correctness of the surrounding YAML.

## Input gate

```yaml
jobs:
  report:
    steps:
      - id: process
        argv: [python3, scripts/process.py]
        timeout: 1m
      - id: approval
        uses: input/wait
        with:
          prompt: Approve the prepared report?
          evidence-id: '${{ steps.process.outputs.stdout }}'
          payload: {choices: [approve, decline]}
      - id: render
        if: '${{ steps.approval.outputs.answer == "approve" }}'
        argv: [python3, scripts/render.py]
        timeout: 1m
```

The preparation script must return a stable nonsecret evidence identity and
retain the corresponding immutable data. Define application validation of
accepted values and approval semantics explicitly; input/wait accepts JSON,
not an application-specific policy. Completed preparation must not repeat on
controller restart. Read [recovery](recovery.md) for answer correlation.

Test valid/invalid input, contract failures, exhausted limits, declined approval,
stale evidence and recovery. Validate before executing. Models and remote
publication remain separate from deterministic validation.
