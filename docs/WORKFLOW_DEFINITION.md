# Simple workflow definition proposal

Status: proposed version `0.2`, replacing the verbose `0.1` draft. The current
engine and UI do not implement it. See the [schema](workflow-definition/workflow.schema.json)
and [complete example](workflow-definition/review.workflow.json). Existing
runtime recovery guarantees remain the implementation baseline.

An AI should be able to write an ordinary workflow from this explanation:

> Write JSON with `version: "0.2"` and `steps`. Steps run in order.
> Call agents with `agent` and `prompt`; give a step an `id` to read its result
> as `$.steps.<id>`. Put paths in `{{ ... }}` inside prompts.
> Use `parallel`, `if` with `then`/`else`, `repeat` with `steps`/`until`,
> and `each` with `steps` for control flow.
> Use `conversation` with participant role names when agents need to talk;
> `start`, `finishBy`, and `maxTurns` control who begins, who finishes, and the budget.
> Use `retry` for transient failures, `onError` for fallback steps,
> and `onFailure` or `finally` for notifications and cleanup.
> In a handler, `recover` asks a helper to work with the original agent;
> `maxTurns` bounds their conversation, `breaker` stops repeated failures,
> and `fail` ends the workflow explicitly.
> Load config through `sources`. Add `schemas` and task `output` when a result
> must be structured JSON.

Only `version` and `steps` are required at the root. Tasks, schemas, explicit
binding wrappers, and component references need no declarations for ordinary
text workflows.

## Minimal example

```json
{
  "version": "0.2",
  "steps": [
    {"id":"draft","agent":"writer","prompt":"Write a plan for {{ $.input.task }}"},
    {"id":"review","agent":"reviewer","prompt":"Review {{ $.steps.draft.text }}"}
  ]
}
```

## Steps and results

Exactly one operation determines a step. IDs are optional unless a consumer
needs the result; explicit IDs must be unique across the definition, including
hooks and fallbacks. Loop invocations reuse static IDs with distinct runtime
identities.

| Step | Behavior |
| --- | --- |
| `{"agent":"writer","prompt":"..."}` | Call a registered role; default result is `{"text":"..."}` |
| `{"conversation":["coder","reviewer"],"prompt":"...","finishBy":"reviewer"}` | Create persistent participant sessions and route their messages until the authorized finisher completes |
| `{"action":"audit.record","with":{"runId":"$.run.id"}}` | Call a registered application action with JSON input |
| `{"run":["go","test","./..."]}` | Run an argument array without shell expansion |
| `{"recover":"debugger","maxTurns":4}` | In an agent's error handler, let a helper work with that original agent to recover |
| `{"fail":true}` | End the workflow with the current error, bypassing ancestor fallback handlers |
| `{"value":{"approved":true}}` | Return resolved data without calling an adapter |
| `{"steps":[...]}` | Run sequentially and return the last result |
| `{"parallel":[...]}` | Run branches concurrently; return results in declaration order |
| `{"if":"$.config.enabled","then":[...],"else":[...]}` | Run one branch; absent else returns null |
| `{"repeat":3,"until":"$.steps.review.approved","steps":[...]}` | Run up to three cycles; fail if the condition never becomes true |
| `{"each":"$.input.files","steps":[...]}` | Pin an array, run a block per item, return results in item order |

Agents receive their resolved prompt plus optional `with` data alongside host
role instructions. Without `output`, returned text is never guessed to be JSON.
With `output`, the adapter requests structured JSON and validates it before
consumers run. Actions use registered contracts. Commands receive optional
`with` as JSON stdin and normally return `{exitCode, stdout, stderr}`; a nonzero
exit is an error. With `output`, stdout must contain exactly one JSON document;
parse and validate that document, and return it directly. This lets a script
produce the same replacement result as an agent. Stderr is retained as redacted
diagnostics. A script that only reports an error must be followed by `{"fail":true}`
in the handler, or run as an `onFailure` observer, to preserve failure.
Role permissions, executable access, workspace isolation, and action registration
remain host responsibilities.

Each block's `$.steps` contains completed named results and inherited outer
results. Sequences share their containing scope. Conditional branches export
their names. Parallel branches inherit a snapshot and cannot read each other's
in-flight results; named results merge only after a successful join. Unhandled
failure cancels and joins siblings before propagation. Optional `maxConcurrency`
limits parallel/each execution without widening host limits.

Repeat bodies use fresh local results each cycle. `$.previous` contains the
preceding cycle's named results, or optional `initial` (default `{}`) on the
first cycle. Evaluate `until` after the body. The repeat returns the last body's
final result. Without `until`, run exactly `repeat` cycles.
`$.run.iteration` is zero-based within the nearest repeat.

Each bodies expose `$.item.value` and zero-based `$.item.index`, execute
sequentially unless `maxConcurrency` is set, and default to `maxItems: 1000`.
Oversized arrays fail before execution; empty arrays return `[]`. Reject
ordering-unstable object wildcard selections for iteration planning. Repeat/each
body names remain local; name the composite to read its result outside.
Nested frames use the nearest repeat/each frame of the corresponding kind.

Every block returns its last result. Optional root `return` selects or constructs
a different final value. Optional root `input` and `output` validate invocation
input and final output.

## Agents talking to each other

```json
{
  "id":"solution",
  "conversation":["coder","reviewer"],
  "start":"coder",
  "finishBy":"reviewer",
  "prompt":"Solve {{ $.input.task }}. Coder proposes; reviewer checks and requests fixes.",
  "maxTurns":8
}
```

Each unique participant name is a registered host role and gets its own persistent
session for this conversation invocation. Roles must support brokered messaging
and retained sessions; unsupported adapters reject admission. The shared prompt
is resolved once at entry, alongside each role's host instructions. The engine
also supplies the authenticated participant identity and allowed peer names.

Participants receive `send(to, message)` and `finish(result)` tools. Send delivers
a message only to another declared participant; the engine assigns the sender.
Only `finishBy` may finish. A declared `output` validates the finish result and
returns it directly; otherwise the finish result must be `{"text":"..."}`.
Invalid finish output is an ordinary validation failure, not a reason to replay
the whole conversation. A successful completion closes delivery and discards
unprocessed messages with their disposition retained in the journal.

`start` defaults to the first participant; start and finishBy must be participants.
The engine starts that participant with the shared task, then services pending
messages in durable admission order, with one active participant turn at a time.
All sessions retain their own history. A turn may enqueue multiple messages;
delivery request IDs prevent duplicates. A turn that emits no finish and leaves
no runnable messages raises `conversation.stalled`. Participants do not poll
each other or create unbounded background turns.

`maxTurns` defaults to eight and counts all participant provider calls together,
including failed calls. Exceeding it raises `conversation.exhausted`. An optional
`timeoutMs` bounds the entire conversation (default the remaining workflow
deadline). There is also a fixed default limit of 32 admitted inter-agent messages
per invocation; overflow raises `conversation.messages_exhausted`. Host limits
may lower these budgets. All calls consume the root task-attempt budget and
role circuit breakers. Optional conversation `breaker` applies the same policy
to all participant roles, following the existing policy-consistency rule.
A completed turn producing valid messages is a successful invocation for that
role's breaker, including for a half-open probe. The independent turn budget
bounds a conversation that exchanges valid messages without reaching agreement.

An unhandled participant error pauses scheduling, joins active work, and fails
the conversation. Ordinary `onError` may hand off to another agent or script,
but the uncertain-effect and integrity bypass rules still apply. `$.error`
identifies a failed role when applicable; `$.failed` describes the conversation,
including its resolved prompt and authorized redacted transcript. Group errors
such as budget exhaustion need not name a single failed role. Per-participant
`recover` cannot be attached at this level: this version requires it inside an
individual agent task's handler, as described below.

Conversation hooks apply once to the group. Messages, participant session IDs,
queued deliveries, completion receipts, remaining budgets, and the designated
finisher are journaled and fenced to the execution generation. Resume preserves
completed turns and cannot resend an uncertain turn. Messaging does not grant
new filesystem, tool, credential, or participant-spawn permissions.

The [full conversation example](workflow-definition/conversation.workflow.json)
includes typed completion, a backup agent, breakers, and terminal error reporting
through [report-error.py](workflow-definition/report-error.py). It expects the
repository root as its host workspace; commands resolve there. All agent roles
remain host registrations. The current engine does not execute this proposal.

## Sources and data

```json
{
  "sources": {
    "config": {"file":"./review-config.json"},
    "mode": {"env":"REVIEW_MODE","default":"normal"},
    "guidance": {"value":{"strict":true}},
    "token": {"secret":"review-service-token"}
  }
}
```

Source names become roots such as `$.config.securityReview`. Reserved names are
`input`, `steps`, `previous`, `item`, `error`, `failed`, `event`, `run`, and `breaker`.
Files are JSON, relative to the workflow file. Environment values are strings
unless `parse: "json"` is set; `default` is an already parsed literal used only
when absent. Literal sources are never evaluated as bindings. Optional source
`schema` accepts a schema name or inline JSON Schema. Sources resolve and validate
once at admission; pin nonsecret values for resume. Sources have no dependencies.

In `with`, `value`, `initial`, and `return`, strings beginning with `$` are
JSONPath queries requiring exactly one match. Other strings are literal.
Objects and arrays resolve recursively. An exact `{"literal": ...}` object
escapes resolution; an exact `{"all":"$..."}` returns an array of every match.
A missing value is an error; a matched null is a value. Prompt placeholders
`{{ $.input.task }}` also require one match; strings insert as text, other
values serialize as JSON. No interpolation occurs outside `prompt`.
Conditions and `each` paths are always selectors; no arbitrary script executes.

Secret sources expose opaque references. Task `credentials` maps adapter
credential slots to secret source names. Plaintext cannot enter prompts, ordinary
inputs, outputs, or persisted snapshots. The host controls credential resolution
and redaction.

## Conditions and schemas

Conditions are boolean literals or paths selecting booleans, with no truthiness.
Additional forms are `{"path":"$.steps.review.status","equals":"approved"}`,
`{"all":[...]}`, `{"any":[...]}`, `{"not":...}`, and
`{"path":"$.steps.review.score","matches":{"minimum":8}}`.
All/any short-circuit left to right. Schema mismatch is false; a missing value or
invalid schema is an error.

```json
{
  "schemas": {
    "Review": {
      "type":"object",
      "properties":{"approved":{"type":"boolean"},"feedback":{"type":"string"}},
      "required":["approved","feedback"],
      "additionalProperties":false
    }
  }
}
```

An agent adds `"output":"Review"` to return this JSON shape directly; read
`$.steps.review.approved` without an output envelope. Inline schemas also work.
All schema fields accept a local schema name, schema object, or boolean schema.

Contracts use [JSON Schema 2020-12](https://json-schema.org/draft/2020-12).
Named schemas borrow reuse from [OpenAPI components](https://spec.openapis.org/oas/v3.1.1.html#components-object).
Selectors use [RFC 9535 JSONPath](https://www.rfc-editor.org/rfc/rfc9535.html).
Each inline/named schema is a resource; internal references use local fragments
such as `#/$defs/Finding`. Reject external references, nested `$id` resources,
other dialects, and conflicting resource IDs. Schema defaults never insert values.

## Errors and hooks

```json
{
  "id":"review",
  "agent":"reviewer",
  "prompt":"Review {{ $.steps.draft.text }}",
  "output":"Review",
  "retry":2,
  "onError":[
    {"agent":"backupReviewer","prompt":"{{ $.failed.prompt }}","output":"Review"}
  ]
}
```

Retry counts additional attempts; default zero. Only host-classified transient
errors with proven-safe replay qualify. Delays are 1s, 2s, 4s, capped at 30s.
Output-validation errors after completed work never replay the original task.
Optional `timeoutMs` bounds each task attempt.

After eligible retries, `onError` executes fallback steps with `$.error` and
`$.failed` (resolved original prompt/with and retained raw result when available).
Its last result replaces the failed result and must satisfy the original output
contract if declared. Recovery remains recorded. Fallback failure propagates
with the original retained and cannot re-enter the same handler. Any step and
the workflow can declare `onError`. For failure of a composite, `failed`
describes that composite; a bubbled child error does not make its prompt the
composite's prompt.

Failure means an adapter error, known-state timeout, invalid output, nonzero
command exit, or an explicit `fail` step. Prose such as "I could not do it" is
not parsed as failure. For domain failures, request structured output and use
`if` plus `fail` to interpret it. Errors are ordinary redacted data:

```json
{
  "code":"agent.unavailable",
  "message":"The provider could not complete the request",
  "agent":"writer",
  "stepId":"draft",
  "attempt":1,
  "retryable":true,
  "effectState":"notStarted"
}
```

`code`, `message`, `stepId`, `attempt`, `retryable`, and `effectState` are required;
`agent` exists for agent errors. Unnamed steps use their stable node path as
`stepId`. Effect state is `notStarted`, `completed`, or `unknown`. Retryability
is assigned by the adapter and engine, never by an agent's claim.

No `onError` means propagate the error; without an enclosing handler the workflow
fails. `{"fail":true}` in a handler ends the workflow with the current error,
bypassing ancestor recovery. Outside a handler it creates `workflow.failed`.
`{"fail":"Cannot recover this task"}` supplies a literal failure message.
Failure observers and finally work still run where safe. Explicit fail is a
terminal control signal; observers cannot silently discard it.

## Working with the original agent

```json
{
  "id":"draft",
  "agent":"writer",
  "prompt":"Implement {{ $.input.task }}",
  "onError":[
    {"recover":"debugger","maxTurns":4}
  ]
}
```

`recover` is different from an ordinary fallback agent call. It starts the named
helper with the error, resolved task input, and authorized transcript of the
original agent. The engine gives that helper a brokered channel to the original
agent. The helper can ask questions and supply recovery instructions; the engine
routes turns to the original session, then back to the helper as needed.
Success requires a successful original task result satisfying its original
contract; a helper merely saying "fixed" does not count.

Use `recover` only inside the failed agent's `onError`, including nested steps
that retain that failure context. It requires a valid original-agent handle and
host support for follow-up after failure. A dead/unavailable session produces
`recovery.unavailable`; the engine must not silently replace it with a new
session and call that a conversation with the original. An ordinary fallback
agent or script remains available as an explicit alternative through nested
error handling.

`maxTurns` defaults to four and counts every helper and original-agent provider
call together, including the final completion call. Default total recovery
timeout is two minutes, lowered by `timeoutMs` or enclosing deadlines. Exhaustion
raises `recovery.exhausted`; it does not start another recovery automatically.
All calls also consume the workflow task-attempt budget and their role's breaker.
The helper has only its host-granted permissions. Original-agent calls preserve
the original role, workspace policy, and known-effect replay restrictions.
Admitting a recovery conversation is not permission to replay uncertain work.

## Circuit breaker

```json
{
  "agent":"writer",
  "prompt":"Implement {{ $.input.task }}",
  "breaker":{"failures":3,"cooldownMs":30000},
  "onError":[{"recover":"debugger","maxTurns":4}]
}
```

Retry limits one invocation; the breaker tracks failures across invocations.
The proposal uses the usual closed/open/half-open pattern described by the
[Azure Architecture Center](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker).
Every agent role gets a breaker by default: three consecutive qualifying agent
failures open it for 30 seconds. The inline field overrides these defaults.
Policies are shared by admitted role identity within one run, including repeat,
parallel, helper, and original-agent recovery calls, and persist across resume.
This version does not share breaker state across separate workflow runs.
If one occurrence specifies a policy, other calls to that role inherit it;
conflicting explicit policies for the same role are compilation errors.

- Closed: admit calls; qualifying failure increments the counter, successful task
  completion resets it. A successful diagnostic exchange during recovery does
  not count as completing the original task and cannot clear its failure count.
- Open: reject new calls immediately with `circuit.open`; do not invoke the provider,
  schedule a background retry, or wait out cooldown inside that call.
- Half-open: after cooldown, the next otherwise-permitted call is the single probe.
  Other calls fail fast. Probe success closes and resets; probe failure reopens
  for another cooldown. No new call means no probe.

Definitive provider errors, known-state timeouts, output-validation failures, and
explicit adapter-reported task failures count once per physical call. This does
not make them all retryable. A valid result conveying an unfavorable decision is
ordinary data. Cancellation, integrity errors, and synthetic `circuit.open`
errors do not increment the counter or reset cooldown.
Calls already running are joined normally. Transitions and the single probe lease
are persisted atomically; late results from an earlier breaker state cannot
reset a newer state. Uncertain probe outcomes require attention, not a new probe.
Zero cooldown and disabling the breaker are excluded from this draft.

`circuit.open` bypasses that task's retry loop and may enter its `onError` once.
A different role can diagnose or take over, but neither helper nor script can
reset the breaker or force a blocked original-agent call. Limits and failure
handlers remain bounded even when a provider remains unavailable.

## Agent or script breakers

The breaker can additionally consult an agent or script before each guarded call:

```json
{"breaker":{"agent":"supervisor","prompt":"Decide whether to proceed: {{ $.breaker }}"}}
```

```json
{"breaker":{"run":["python3","breaker.py"]}}
```

Both use the same snapshot: guarded role and invocation, circuit state,
consecutive failures, remaining attempts, and the last redacted error (or null).
The snapshot is available as `$.breaker` to the policy prompt. The script receives
the snapshot as JSON stdin; the policy agent receives it as structured input,
with an optional prompt. Both must return exactly this response shape:

```json
{"allow":false,"reason":"The agent keeps repeating the same failed approach"}
```

`allow` is required and boolean; `reason` is an optional string of at most 1024
characters. Unknown response fields are errors. A false decision opens the circuit
for its configured cooldown and returns `circuit.denied`. A crashed, timed-out,
or malformed checker returns `circuit.policy_failed` and does not launch the
guarded work. An uncertain checker instead requires attention under the existing
hard-failure rules. Only allow=true permits the guarded call.

The static breaker remains mandatory. Optional `failures` and `cooldownMs` on a
custom policy override its normal defaults; the checker cannot reopen a circuit
before its cooldown, clear failure counters, or widen any budget. Policy calls
consume the workflow attempt/deadline budget and, for conversations, the shared
turn budget. Policy evaluation itself uses only the checker's static circuit,
never another custom checker, preventing recursive policy calls. Agent policies
must use a different role from their guarded role. Checkers receive their normal
host permissions; a read-only policy role is appropriate for observation.

Policies default to a ten-second timeout, lowered by optional `timeoutMs` or
enclosing deadlines. A valid decision is journaled before dispatch. On resume,
the engine retains that decision for its bound invocation rather than asking a
different question midway through the same admitted call.

## Lifecycle observers

`onStart`, `onSuccess`, `onFailure`, and `finally` are step lists applying
only to their subject, with no inheritance. Hooks see `$.event` containing phase,
subject ID/path, status, and available result/error. Failure hooks also see
`$.error`. Start failures prevent the body and enter normal error handling.
Outcome/finally hooks are awaited observers; ordinary hook failures are recorded
without replacing the result or re-entering the subject's handler. Recovered
subjects run success hooks. Hook bodies suppress recursive lifecycle hooks.

Cancellation, integrity/ownership failure, and uncertain effects bypass retry and
fallback everywhere, including inside observers. They result in `canceled` or
`needs_attention`. Hooks run only where safe; finally gets a bounded cleanup
context. Fallback cannot start while the original agent might still be running.
Do not claim automatic rollback or exactly-once effects.

## Defaults and validation

Defaults are a 30-minute workflow deadline, 100 total task attempts, four shared
execution slots, a two-minute task-attempt timeout, a ten-second hook deadline,
and a shared 30-second cleanup budget. Host policy may impose tighter limits.
Optional root `limits` can lower `timeoutMs`, `maxTaskAttempts`,
`maxConcurrency`, `hookTimeoutMs`, or `cleanupTimeoutMs`. Retries, fallback,
and hook tasks consume the same attempt budget. Exhaustion is an error; cleanup
that cannot run is explicitly recorded.

JSON Schema validates shape. The compiler additionally validates embedded schemas,
local schema names/references, selectors, placeholders, scope, reserved/unique
names, credentials, messaging/recovery capabilities, conversation membership,
role breaker policy consistency, and
adapter capabilities. Unknown fields are errors.
The outer schema's object/boolean check on contracts does not meta-validate them.
No live providers or publication are needed for validation.

Persist the admitted definition, sources, adapters, ordered item plans, cycle
states, attempts, and hook/fallback receipts. Record attempts before effects and
results before consumers. Invocation identities bind run, node path, and enclosing
cycle/item indices. Resume reuses completed work and never automatically replays
uncertain effects. Authors do not repeat these runtime guarantees in each file.

Recovery stores the original/helper session identities, ordered messages,
remaining turn allowance, and result receipts. Resume never resends an uncertain
turn. The allowed original session and its permissions are pinned at admission;
recovery participants cannot nominate an unrelated session.

The complete example needs host roles `writer`, `reviewer`, `judge`, and
`backupReviewer`, plus the registered idempotent action `audit.record`.
The [failure examples](workflow-definition/failure-cases.json) give independent
definitions for stopping, agent handoff, collaborative recovery, script recovery,
and script reporting followed by failure. Script examples require a caller-supplied
`recover.py` or `report-error.py`; no script is executed by definition validation.
