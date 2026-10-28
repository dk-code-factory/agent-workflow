# Durable external input

Engines advertising `durable-external-input-v1` can suspend a workflow for
correlated JSON input. Go's `workflows.Capabilities()` and controller `/health`
report this contract. Hosts should check it and report a dependency diagnostic
before offering answer controls. Existing stores remain compatible; definitions
using waits require the optional `ExternalInputJournal` before admission.

```yaml
jobs:
  decision:
    steps:
      - id: question
        uses: input/wait
        with:
          prompt: Which policy should apply?
          evidence-id: review-evidence-v1
          payload: {choices: [strict, permissive]}
      - uses: agent/reviewer
        with:
          prompt: 'Use the selected policy: ${{ steps.question.outputs.answer }}'
```

`prompt` and `evidence-id` are required nonempty strings, including expressions
that resolve to strings. `payload` is optional JSON data. Accepted answers appear
in `answer`, `answer_id`, `gate_id`, and `evidence_id` step outputs. The host owns
application-specific value validation. Questions have no expiration.

A gate commits before inspection exposes it. The step releases execution capacity
and blocks dependents. Already admitted independent work drains through its
normal checkpoints and cleanup. Only then does the originating run become
`waiting` and release controller capacity. Reusable workflows, repeats and ordered
items propagate waiting; continuation resumes the same child/item and reuses
completed steps and actions. Waiting is nonterminal and has no finish timestamp.

## Inspect, answer, interrupt and resume

```sh
agent-workflow inputs RUN_ID
agent-workflow answer --gate GATE_ID --evidence-id EVIDENCE_ID \
  --answer-id DELIVERY_ID --answer-file answer.json RUN_ID
# --answer-file - reads a single JSON value from stdin.
agent-workflow interrupt RUN_ID
agent-workflow resume RUN_ID
agent-workflow cancel RUN_ID
```

A synchronous `run` or `resume` reaching a wait prints the durable run handle and
exits with status **2**. Detached admission returns immediately. `logs --follow`
continues through waiting and interrupted states until the run terminates.

The controller exposes `GET /runs/{id}/inputs`,
`POST /runs/{id}/inputs/{gate}/answer`, and `POST /runs/{id}/interrupt`.
An answer request is `{"answer_id":"delivery-1","evidence_id":"evidence-v1","value":false}`.
Every answer requires a unique identity, the exact pinned evidence identity and
one non-null JSON value. False, zero, empty strings, arrays and objects are valid.
Duplicate delivery, reused answer identities, stale evidence and invalidated gates
are conflicts. Inspection includes the accepted value and lifecycle history, so
a client can recover after losing an acceptance response without submitting twice.

Acceptance records continuation intent in the same transaction. The controller
queues one continuation when every outstanding question is answered and execution
cleanup has finished. Intent survives shutdown; answer delivery during shutdown
does not launch a competing owner. The Go executor returns waiting to its caller;
embedded hosts schedule durable intent with `QueueInputContinuations` or Resume.

Interrupt commits a pause before stopping an active owner. Questions and accepted
answers remain inspectable, while automatic continuation stays paused. Resume
clears the pause; unanswered questions remain waiting. Cancel invalidates pending
and accepted questions; continuing a canceled input workflow requires a new run.
Uncertain commands, provider turns and external effects still require the existing
reconciliation and explicit retry acknowledgement safeguards.

## Preparation, wait, re-preparation

Use **preparation → wait → re-preparation** in the application. A registered Go
action prepares immutable evidence and returns its evidence identity. The question
pins that identity. Before consuming the answer or starting dependent work, the
engine verifies admitted snapshots, action contracts, current application bindings
and retained action evidence through `ValidateCheckpoint`. Drift produces
`needs_attention` and preserves the accepted answer unconsumed; start a new run
with refreshed evidence. A later application action can prepare a revised artifact
using the answer. The evidence ID correlates delivery; application hooks validate
the evidence itself. Generic answers do not change application policy by themselves.

## Recovery and storage

SQLite schema v10 retains gates, physical and originating execution identities,
job/step/iteration identity, immutable requests, evidence, answers and lifecycle
history. Upgrades use exclusive workspace ownership and retain `state.pre-v10.db`.
Recovery reconstructs safe waiting states after a lost checkpoint without replaying
completed effects. Uncertain effects stay in attention for reconciliation.

Waiting and interrupted runs cannot be retained away or deleted as terminal
history. Consistent backup includes requests and answers. Restore preserves them
and quarantines execution with `restore_uncertainty`; inspect effects and explicitly
acknowledge retry before continuation. It never silently replays a restored answer.
