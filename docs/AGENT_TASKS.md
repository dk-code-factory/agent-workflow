# Pipeline agent tasks

Native tasks can declare an ordered [verification checklist](AGENT_CHECKLISTS.md)
under `with.checklist`, with shared and per-stage retry limits, prompt stages,
and command checks whose failures return to the original agent for repair.

Use `agent/run` with a shared YAML agent file or a directory containing
exactly one YAML file. Paths resolve from the workflow that owns the step.
Admission captures the file bytes and effective settings; resume uses that
snapshot even if the file changes.

`with.agent` also accepts `{select, definitions}`. `definitions` is a catalog of
1–32 literal names mapped to literal file or directory paths, resolved relative
to this workflow. `select` is a declared name or a whole expression returning
one. Admission captures every definition, including unselected entries, through
the same native runtime checks as a single definition. Runtime never loads a
path from expression data. An unknown name or a non-string selection fails
before agent dispatch. For example:

```yaml
agent:
  select: ${{ inputs.role }}
  definitions:
    review: agents/reviewer.yaml
    fix: agents/fixer.yaml
```

The selected definition creates a fresh session. Continue it with the returned
opaque agent reference, using the ordinary string-expression form. Catalog
selection and string session references remain distinct. The editor accepts the
catalog form and exposes **Agent definition selection (YAML)** for editing it.

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
    agent: ../.codex/agents/reviewer.yaml
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
        agent: ../.codex/agents/recovery.yaml
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

The shared YAML agent definition requires `name`, `description` and
`instructions`, with optional `model` and `reasoning-effort` (`none`, `minimal`,
`low`, `medium`, `high`, or `xhigh`). Unknown settings are rejected, including
tool, provider, authentication, sandbox, permission, MCP and execution overrides.
Starter definitions omit model and reasoning settings for portability:

```yaml
name: reviewer
description: Review changes for correctness.
instructions: Inspect the changes and report concrete findings.
```

New runs accept YAML sources only. Convert native definitions offline with:

```sh
agent-workflow agent convert --from codex --input reviewer.toml --output reviewer.yaml
agent-workflow agent convert --from claude --input reviewer.md --output reviewer.yaml
```

Conversion preserves compatible metadata and instruction text, reports omitted
options and required workspace runtime-policy changes, and creates no output
until each omitted field is explicitly acknowledged with repeatable
`--acknowledge-loss FIELD` flags. Hooks are never executed. Sources and existing
outputs are never overwritten. Previously admitted TOML snapshots retain their
original bytes, hashes, identities and Codex bindings without reopening sources;
this is a source-format change for new runs, not a snapshot migration.

Managed `codex-app-server` and `claude-sdk` sessions exclude installed,
repository and bundled skills from the model's skill catalog and explicit skill
invocation. Codex disables discovered skill paths in its private state before
starting or resuming a thread, suppresses the catalog, and disables plugins.
Claude disables builtin plugins through inline settings, retains its empty skill
allowlist and disabled Skill/Agent/Task tools, and rejects an init plugin set
that is not empty. This also applies when the prompt mentions `$skill-name`.
Ordinary native tools retain their role policy.

Claude's init validation occurs when the SDK emits `system/init`; it is not a
pre-model API admission barrier. Inline plugin suppression is the startup
control, and native tool dispatch requires verified initialization.

An explicitly supplied skill uses a declared `type: skill` input and a
`with.skill` selection on one literal agent definition:

```yaml
on:
  workflow_call:
    inputs:
      selected: {type: skill, required: true}
jobs:
  review:
    steps:
      - uses: agent/run
        with:
          agent: reviewer.yaml
          prompt: Review the supplied target.
          skill:
            input: selected
            arguments: exact arguments
            resources: [reference.txt]
            evidence: {invocation: required}
```

Supply `selected` as `{directory: /absolute/path/to/skill}`, or
`{package: {root: /absolute/local/package, digest: <64-hex-digest>, skill: name}}`.
The application resolves the local package through its captured manifest; no
registry or installed-skill lookup occurs. The skill directory contains a
`SKILL.md` with only `name` and `description` frontmatter. Authority-changing
metadata, symlinks and additional skill entries are rejected. The instruction entry is limited to 64 KiB after Go JSON string escaping, reserving bounded ledger space for original provider envelopes and outputs. Resource paths
are literal paths inside that skill directory. Package resource declarations
outside the selected skill directory reject admission in this profile. The
application capture binds both complete package provenance and selected entry
content; changed captured bytes cannot retain an idempotency identity. Admission captures exact bytes,
source modes and empty directories. Each selected occurrence receives only its
entry and explicitly selected resources in an engine-owned plugin; independent
siblings retain the empty skill configuration.

Arguments and model overrides can use literals and admitted `inputs` expressions.
Admission pins these values, resources, model and capability profile before run
claims. Resume uses the retained assets, original session and original turn
allowance even after source removal; it does not recapture or replenish them.

The Claude profiles use the locked SDK 0.3.322 local runtime,
with a pinned model, no broker grants, only existing native Read authority,
and no user reasoning-effort override. Before releasing the
prompt, the adapter requires the initialize acknowledgement and the selected
qualified command. The acknowledgement alone does not establish successful
loading. The later `system/init` event must report exactly the admitted plugin;
this complete plugin-set validation still occurs after prompt release and is
not a pre-model barrier. Native Skill calls require the exact qualified name
and admitted arguments. Original provider invocation and successful tool-result
events, linked by call and session identity, are retained separately from
redacted progress. A terminal report without those events remains incomplete;
observed invocation does not establish model adherence or task correctness.

The constrained Claude child profile accepts one explicitly required custom
child. The parent definition must select `claude-sonnet-4-6` and omit
`reasoning-effort`; the profile separately pins resolved parent and child effort
to HIGH. This resolved control is part of the retained invocation identity.
The workspace role must already grant native Read; the skill grants no tools.
Add the following fields inside `with.skill`:

```yaml
evidence: {invocation: required, children: [review]}
children:
  - name: review
    prompt: Review the retained reference and return the full result.
    description: Review retained reference
    tools: [Read]
    resources: [reference.txt]
    model: inherit
    max-turns: 2
    max-calls: 1
    background: false
```

Child resources must be literal files within the parent's selected resources.
The SDK child receives only Read, the exact inherited model and resolved HIGH
effort, and at most two child model turns. The model's actual Agent call must
match the original declared subtype, prompt and description and explicitly set
`run_in_background: false`. Original public assistant input and the permission
hook must agree before the engine commits a dispatch permit. Unknown/builtin
subtypes, overrides, nested delegation and access to other resources are denied.
The SDK's init catalog can still name builtin child types; those names grant no
dispatch authority. The pinned init event calls the model-facing Agent tool
`Task`; that alias is accepted only for this exact selected profile.

The one child allowance is consumed durably before dispatch and is shared by
the entire original invocation, including checklist retries and fresh-process
continuations. A lost permit acknowledgement cannot start another child.
Completion requires the original parent/child identities, task lifecycle,
forwarded full output, successful original tool results, SubagentStop and parent
terminal. An exhausted child can report `status: completed` while lacking a
final output; it remains incomplete. Summaries and parent success alone cannot
complete required evidence. The original SDK parent usage and cumulative
modelUsage map are retained; the verified same-session aggregate charges both
parent and child calls without resetting or double-charging the original usage
baseline. Missing or conflicting accounting retains known charges and prevents
completion. A malformed cumulative receipt cannot lower already retained parent and child usage floors; those floors are added only above the original session baseline. Resource paths use the same canonical relative identity for parent and child admission. The engine holds its execution lease throughout this synchronous
profile; it makes no broker-capacity release or general quiescence claim.

The stock Codex 0.167.1 local profile requires a pinned model, empty skill
arguments, the tested default/read-only sandbox setting and no reasoning-effort
override. The sandbox setting does not establish nonmutating native execution.
Each process registers the retained
root and confirms that only the admitted entry remains enabled before a turn.
Native delegation is disabled in both tool specifications and dispatch. The
initial public native-consumption fragment must carry the selected name, path,
exact retained bytes, current turn and session, and native skill provenance.
This records native instruction consumption, not model adherence or task success.
The pin exposes that fragment on a new thread, but not from a fresh process
resuming a thread. Continuation therefore requires the original invocation's
retained receipt and session; it re-registers the original retained root without
inventing another observed invocation. A new occurrence cannot borrow that
receipt. Missing original evidence after a possible effect blocks recovery before
claiming another turn, preserving the original uncertainty and allowance.
Selected-skill JSON output must set `output.repair-attempts: 0`. The tool-free
repair path is not proved for retained skills, so implicit or positive repair
allowances reject admission. Ordinary tasks retain their existing repair behavior.

Selected Claude turns set native retries to zero and disable the nonstreaming
fallback. A public API retry, subagent retry or assistant API-error envelope
retains an incomplete-attempt receipt and stops the session. In the pinned SDK,
a failed child request can reach a parent follow-up before that envelope reaches
the consumer; this is not a barrier before every subsequent API request. Known
child usage is committed with its original evidence before a parent terminal
exists. After an API error the bridge interrupts and drains only the same original
query for up to five seconds to retain any native terminal/accounting; even a
later SDK success remains incomplete. Missing terminal data keeps the reported
lower bounds rather than inferring unreported tokens or cost. The original
allowance remains consumed.
Selected Codex turns use an engine-owned OpenAI descriptor with zero request and
stream retries, no WebSocket transport/fallback and no unbounded connection
retries. Admission and retained recovery run a bounded private native control probe: initialize and config/read only, without credentials, login, threads or model requests. The effective descriptor is checked before run claims and asset projection; the selected workspace is pinned. The adapter rechecks the effective descriptor and thread provider before each turn so later configuration drift also fails closed. It rejects inherited credential, header and endpoint overrides; an
explicit application command may supply its endpoint only through the verified
session-flags layer. A matching public retry notification retains uncertainty and
cancels the original turn even if a later terminal would report success.
These checks do not establish model adherence or reconcile unknown native effects.

For every selected provider profile, a turn's uncertainty is committed before
provider dispatch and cleared only with its original successful terminal and
worker-message completion. Complete initial consumption evidence, a nonwritable
workspace label, process cleanup and an explicit uncertain-retry request cannot
authorize replacement work after an unknown turn. A lost local completion for
the original Claude child can be reconciled from its complete retained native
terminal, outputs and accounting before generic tool uncertainty is evaluated.
A failed turn or a later unknown continuation cannot borrow that older terminal.

Codex native children, broader Claude child shapes, containers, reusable
workflow selections, unproved tool/permission profiles and unsupported effort
or argument controls reject admission. They do not fall back to prompt imports
or host skills. These profiles establish native loading, observed consumption
and the specified controls; deterministic fake-gateway evidence does not prove
model adherence or task quality.
The editor exposes skill input mappings and **Skill invocation (YAML)**;
host admission remains responsible for source and provider capability checks.

Workspace `agents.yml` selects the configured role for every native task with
`agent-runtime`. Omit it to use `codex`. The selected role must use
`codex-app-server` or `claude-sdk`; this selection is independent of the YAML
definition's name. For example:

```yaml
agent-runtime: task
agents:
  task:
    provider: claude-sdk
    bridge: bridge/claude/bridge.mjs
    model: claude-sonnet-4-6
    tools: []
    authentication:
      mode: native
      credential-env: CLAUDE_CODE_OAUTH_TOKEN
```

Install the locked bridge dependencies before selecting Claude SDK. Native
Claude authentication requires an explicit token environment reference. Gateway
authentication instead requires `mode: gateway`, an `endpoint` and a
`credential-env` API-key reference. Native tasks do not log in or import host
settings, hooks or plugins. Claude-only workspaces need no Codex binary.

Admission retains the selected role, model, instructions, credential references,
execution policy, limits and tool grants. Definitions cannot expand these
boundaries. Changing current configuration cannot switch an admitted task or its
continued session to another provider. Snapshots admitted before runtime
selection retain Codex and their original definition bytes without migration.

Definitions can override the model with a string accepted by the chosen
provider or gateway. Claude SDK rejects definition `reasoning-effort`; use a
portable definition without that field when switching providers. Codex managed tool subsets and tool-free output repair retain
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

An explicit subsequent step can read `steps.task.failure_class` and
`steps.task.unsafe_failure_class`, including through
`needs.job.steps.task` or workflow output `jobs.job.steps.task` paths. These
strings are engine metadata and are empty when no classification was retained.
They take precedence over identically named business fields; those remain
available through `outputs.structured`. The legacy whole-step metadata object
keeps its existing shape. Reading classifications grants no retry permission:
the workflow must reserve its own retained allowance and explicitly select
eligible failures, and engine cleanup and failure-observation guards still apply.

For a task with `continue-on-error: true`, recovering a lost step completion
returns the original terminal failure and its observation receipt to downstream
YAML. Recovery preserves the original message, failure classification, partial
output and acknowledgement generation; it does not retry that continued failure
before downstream disposition runs. A retry selected by the workflow uses a
subsequent `agent/run` coordinate with the original `outputs.agent` reference and
the application's retained allowance. Explicit resume retries of tasks without
`continue-on-error`, and original-session recovery of interrupted or canceled
turns, retain their existing behavior.
