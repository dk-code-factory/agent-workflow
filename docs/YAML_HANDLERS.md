# YAML step failure handlers

Agent and command steps may declare ordered `onError`, `onFailure`, and `finally`
step lists. These fields are supported at step scope only; workflow and job
hooks are rejected. Handler children can themselves declare handlers. Registered
application actions, controller actions, and `input/wait` cannot own or appear
inside handlers: their reconciliation and suspension protocols remain separate.
Agent and command leaves inside `parallel` or `sequence` blocks may own handlers.
Structural blocks cannot own handlers or appear inside a handler list. A handler
inherits its leaf's incoming results, excluding concurrent sibling branches;
completed preceding handler siblings remain local to that handler list.

```yaml
jobs:
  review:
    outputs:
      text: ${{ steps.review.outputs.text }}
    steps:
      - id: review
        agent: reviewer
        prompt: Review the change.
        handlerTimeout: 30s
        onError:
          - id: backup
            agent: backupReviewer
            prompt: '${{ failed.prompt }} Failure: ${{ error.message }}'
        onFailure:
          - agent: reporter
            prompt: 'Review failed: ${{ error.class }}'
        finally:
          - argv: [echo, review finished]
```

After an ordinary task failure, `onError` runs once. Its last successfully executed
child supplies the original step's replacement outputs; a list containing only
skipped children does not recover. An agent's replacement must include a
`structured` value satisfying the original declared JSON output schema, if any.
The original step ID exposes replacement results to downstream consumers.

If failure remains, `onFailure` reports it without recovering it. `finally` then
runs after success or failure when admission is safe. A nested child's handlers
finish before its parent's next child or observer. Each list stops on its first
unhandled error; `continue-on-error` can continue ordinary child failures but
never bypasses hard failures. A failed fallback never re-enters its owner's
`onError`. Failures from fallback, reporting, and finalization are appended in that
order to the original error. A failing finalizer turns a successful task or
recovery into failure. The original failure remains in the step's
`lifecycle.original_error` and `lifecycle.original_class` even after recovery.

Cancellation, uncertain effects, integrity/ownership, prerequisite, and storage
failures bypass handler effects. A task's own command timeout may enter fallback
if the enclosing execution is still active. Cancellation or an enclosing deadline
never starts detached cleanup. Declared lists that cannot run record a reason
under `lifecycle.skipped`; executed children are retained under
`lifecycle.handlers`. Process and invocation cleanup still follows the existing
runtime rules. Handlers grant no additional permissions or credentials and use
the same execution admission, roles, tools, command policy, and host capacity.

`handlerTimeout` bounds the combined handler work for one step, including queue
waits and nested calls. It defaults to 30 seconds and must be positive and at most
5 minutes. Its deadline begins after the task completes and is retained across
restart. An ancestor's or execution's earlier deadline also applies. At most 8
handler nesting levels and 100 statically declared handler children per job are
admitted. A handler deadline failure remains failure and records unrun lists.

Inside a handler, `failed.id`, `failed.uses`, and `failed.prompt` describe its
owner; `error.message` and `error.class` describe that owner's original failure
(empty after a successful task). Prompt and message values are redacted with the
execution's credential redactor before being exposed. Nested owner IDs include
the handler ancestry. Existing input, secret, event, and dependency expressions
remain available. `steps` contains completed preceding outer steps and preceding
siblings in the current list; a sibling shadows an outer step of the same ID once
it completes. IDs are unique within each list. Results from different hook lists
are private and do not become ordinary job step IDs.

With SQLite, the original task completion and an absolute handler deadline are
journaled before handler effects. Each child has a durable qualified attempt ID
and completion receipt. Resume reuses completed task and child receipts,
including completed failures; it does not retry terminal lifecycle failures.
Admitted children without a completion receipt retain the existing explicit
uncertain-retry guard. A restart can therefore continue missing safe work without
rerunning a completed fallback or original task. Checkpoint-only stores retain
the existing observational, non-replayable behavior.
