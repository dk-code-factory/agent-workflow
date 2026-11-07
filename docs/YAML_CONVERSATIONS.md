# Bounded YAML conversations

A `conversation` step runs a bounded exchange among registered agent roles.
Each participant retains a provider session across its turns. Messages determine
which participant runs next; only the designated `finishBy` participant can
complete the step.

```yaml
on:
  workflow_call:
    inputs:
      task: {type: string, required: true}
schemas:
  Decision:
    type: object
    required: [approved, summary]
    additionalProperties: false
    properties:
      approved: {type: boolean}
      summary: {type: string}
jobs:
  solve:
    steps:
      - id: solution
        conversation: [builder, reviewer]
        start: builder
        finishBy: reviewer
        maxTurns: 8
        maxMessages: 32
        timeout: 2m
        prompt: |
          Prepare the requested solution and ask the reviewer to check it.
          Discuss corrections, then let the reviewer finish with a decision.
          Task: ${{ inputs.task }}
        with:
          output: {schema: Decision}
      - agent: reporter
        prompt: 'Decision: ${{ steps.solution.approved }}; ${{ steps.solution.summary }}'
```

Declare the roles in the host's agent configuration as for ordinary agent
steps. Participant names are literal identifiers, not expressions. `prompt`
uses the existing YAML expression rules and can reference inputs, declared
dependencies, and completed preceding steps.

## Declaration and limits

`conversation` declares between two and sixteen distinct roles. `start` defaults
to its first role; `finishBy` is required. Both must belong to the declared list.
The conversation is a leaf operation and can appear inside existing `parallel`
or `sequence` blocks. It cannot combine with an agent call, command, action,
or structural block on the same step.

`maxTurns` limits total participant calls across the entire conversation,
including resumed execution. It defaults to eight and accepts 1–100.
`maxMessages` limits accepted participant messages, defaults to 32, and accepts
1–256. `timeout` bounds elapsed conversation time, defaults to two minutes,
and accepts positive Go durations up to 24 hours. An enclosing execution
deadline can stop it sooner. Exhaustion or an empty delivery queue without a
validated completion fails explicitly.

The optional `with.output` field uses an object-shaped inline or workflow-local
named JSON output contract. Following steps consume the validated result
through `steps.<id>.<field>` or `steps.<id>.outputs.structured` as with ordinary
agent results. Without a declared contract, completion requires an object with
a string `text` field. Outputs also retain `raw_json`, `valid`, `turns`, and
`messages`; the `text` output contains the JSON completion text.

## Messaging and permissions

The controller supplies `send(to, message)` and `finish(result)` tools under
the `workflow-conversation` server. `finish` takes the result object itself as
its arguments. The controller derives
the sender identity from the active turn and restricts recipients to declared
peers. A participant cannot impersonate another role or finish on behalf of
the designated finisher. Completion must satisfy the declared output contract;
invalid completion observed by the controller is terminal and does not start an
output-repair call, including malformed or oversized arguments.

Participants use resumable `SessionRunner` adapters with managed-tool policy
support. Admission rejects unsupported capabilities before workflow effects.
The built-in Claude SDK adapter supports these capabilities; CLI compatibility
adapters and the current Codex adapter do not satisfy this admission contract.
Validation applies to the arguments exposed by the adapter. The pinned Claude
SDK normalizes model input before invoking managed tools: it removes
`__proto__` properties, converts arrays and scalar strings to object properties,
and converts null to an empty object. The controller validates that exposed
object, so it cannot diagnose information already removed by the SDK. Explicit
completion contracts declaring literal `__proto__` properties are rejected
before workflow effects. Contracts are checked again after expression rendering,
before any participant runs; preceding steps may already have executed when a
rendered contract is rejected. This restriction also applies to nested properties
and required-property declarations. Use ordinary object arguments for completion;
the adapter does not promise fidelity to the model's original JSON.
The controller uses the existing invocation, role, capacity, usage, cancellation,
and redaction boundaries. A conversation grants no additional filesystem,
native tool, credential, publishing, or participant-spawn permissions.

## Persistence and recovery

Durable execution retains the admitted definition, participant sessions,
ordered messages and deliveries, consumed budgets, deadline, and completion
receipts. Completed participant turns are reused on resume. A lost enclosing
step completion can be reconstructed from retained conversation results without
running the participants again.

An admitted turn whose provider outcome is uncertain is never resent, including
after a general `--retry-uncertain` acknowledgement. Inspect the retained
attempts and reconcile effects before starting a new workflow run. Recovery
does not reset consumed budgets or extend the saved deadline. Cancellation
stops active participant work through
the existing execution rules. Participant failure propagates to the step.
Conversations may run beside agent and command steps that use lifecycle handlers,
but cannot own `onError`, `onFailure`, `finally`, or `handlerTimeout`, or appear
inside a handler list. Admission rejects these combinations before workflow
effects; conversation receipts and handler receipts use separate recovery protocols.
