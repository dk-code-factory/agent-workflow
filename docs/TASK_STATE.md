# Durable task state

The `durable-task-state-v1` service retains logical claims, allowance reservations
and absolute deadlines in the controller's SQLite database. It supplies no
GitHub, provider or publication policy. Step attempts and action checkpoints
continue to describe physical workflow execution; a task group deliberately
spans replacement runs of the same logical work.

Start a run with `agent-workflow run --task-group GROUP ...`. Group identifiers
contain 1–128 ASCII letters, digits, dots, underscores or hyphens. Admission pins
the group in the run snapshot and records membership transactionally. Resume
retains the original group. A replacement run explicitly selects that same
group; starting a process or another run does not refresh a budget or deadline.
An embedding host can use `RunRequest.TaskGroup`; the controller start request
has the corresponding `task_group` field.

## Container boundary

For a grouped run, each `action-v1` hook receives an optional `task_state` object
beside its business `input`, with `socket`, `token` and `group`. The socket is
mounted at `/engine-task-state/rpc.sock` through a read-only directory bind. The
container runtime must support binding the controller host's Unix socket into
the container. The socket exposes only `POST /v1` task-state RPCs; neither the
database nor a general management API is mounted. Ungrouped runs receive no
descriptor or endpoint.

The bearer token is unique to the current hook and carries its captured execution
owner. Requests cannot supply another group, owner or generation. Every store
transaction verifies that the current execution still owns its admitted group,
and writes also check cancellation/finalization. Tokens are revoked and their
in-flight store operations joined before a hook finishes. Requests still reading
their bodies reject the revoked capability before accessing the store. Shared job
containers reuse the endpoint mount through their lifetime; each hook receives a
fresh token.
Standalone helper containers own a temporary endpoint for that hook. Endpoint
tokens are ephemeral: the engine never adds their descriptor to snapshots,
action identities, checkpoints or recovery bundles. Helpers must not echo or
log the received token or include it in their business output/evidence.

`bindings`, `validate_checkpoint` and `reconcile` hooks may call only `get_claim`,
`get_budget` and `get_deadline`. Creation, completion, reservations, continuation,
task reconciliation and group closure require the `execute` hook. Inspection
does not initialize missing records. Ordinary container commands receive no
token. Shared-container siblings share an OS security boundary: scripts running
in that container must be trusted as peers, as with its other transient state.

The [Python client](../examples/task-state/workflows/client.py) uses only the standard
library. Construct `TaskState(request)` inside a helper and call
`state.call(OPERATION, **fields)`. Each request is one JSON object, with a
64 KiB application limit; the RPC response limit is 1 MiB. JSON integers remain
integers. The transport rejects unknown request fields, trailing documents,
inactive capabilities and writes during read-only hooks. The client sends once:
a timeout or disconnected response does not prove that a transaction failed.

## Operations

| Operation | Required business fields | Behavior |
| --- | --- | --- |
| `claim` | `task`, `fingerprint`, `operation_id` | Atomically claim logical work; only a newly committed claim returns `dispatch: true`. |
| `get_claim` | `task`, `fingerprint`; optional `operation_id`, `evidence_fingerprint` | Observe the selected claim without creating it. |
| `complete` | Claim identity, `result_reference`, `evidence_fingerprint` | Complete a claim owned by this execution; conflicting completion is rejected. |
| `reconcile` | Claim identity, `request_id`, `outcome`; authoritative `proof` for `completed`/`no_effect` | Retain reconciliation; completion also requires the result and evidence identities. |
| `budget` | `name`, nonnegative initial `limit` | Initialize an allowance without reserving it, including a zero allowance to extend explicitly. |
| `reserve` | `name`, `operation_id`, initial `limit`, positive `amount` | Reserve an allowance exactly once; replay returns the current retained budget. |
| `get_budget` | `name` | Return current `limit` and `used`. |
| `extend_budget` | `name`, `request_id`, positive `amount`, `instruction`, `source` | Record explicit continuation provenance and add allowance without resetting consumption. |
| `deadline` | `name`, `operation_id`, absolute RFC 3339 `deadline` | Create a retained deadline once; repeat the same operation identity and payload after response loss. |
| `get_deadline` | `name` | Return original/current deadlines and consumed extension count. |
| `extend_deadline` | `name`, `request_id`, positive `amount` in seconds, `instruction`, `source` | Retain an idempotent extension of the absolute deadline. |
| `close_group` | `request_id`, `instruction`, `source` | Permanently close a resolved group after other active members have stopped. |

Task names, fingerprints, operation/request identities and references are bounded
nonempty strings of at most 512 bytes. Reusing a reservation, deadline,
reconciliation or continuation identity with changed content is an error. The
initial budget limit is immutable; use an explicit continuation to extend it.
Continuation `instruction` and `source` preserve the caller's authorization
provenance; the helper remains responsible for checking that the instruction
actually grants the requested continuation.

## Reuse and uncertain effects

A replayed claim never returns a second dispatch grant. An existing completion
returns `reusable: true` only when the caller presents its current nonempty
`evidence_fingerprint` matching the retained completion for that exact task/input
fingerprint. A task name alone cannot authorize reuse. The helper must inspect
the current input and evidence before providing those identities; the generic
service cannot validate a provider receipt or an application artifact's contents.
The ordinary action checkpoint hooks remain independently required.

For example:

```python
identity = dict(task="review", fingerprint=current_input_hash,
                evidence_fingerprint=current_evidence_hash)
claim = state.call("claim", operation_id=logical_operation, **identity)
if claim["dispatch"]:
    # Retain any domain-specific dispatch intent and receipt as required.
    result_ref = perform_task()
    state.call("complete", operation_id=logical_operation,
               result_reference=result_ref, **identity)
elif claim["reusable"]:
    result_ref = claim["claim"]["result_reference"]
else:
    raise RuntimeError("inspect and reconcile the original operation")
```

Loss of a claim acknowledgement conservatively forfeits the dispatch grant:
retrying observes the committed claim and must reconcile it. Claims have no
expiration and cannot be stolen after a process exits. A stale owner cannot
complete the old claim. A new owner retains an `unknown` observation or presents
authoritative proof of `completed` or `no_effect`, with a stable reconciliation
request identity. An active original owner blocks competing reconciliation.
Only committed `no_effect` permits a subsequent claim under a new operation
identity for that task/input. Neither a timeout, an empty lookup nor an engine
`--retry-uncertain` acknowledgement establishes no effect.

Domain-specific publication intents, receipts and unknown-effect reconciliation
remain application responsibilities. This API does not bypass the action journal
or turn an uncertain external mutation into a successful workflow checkpoint.
The [pure capture example](../examples/task-state/README.md) needs no provider
access; its deterministic test uses a fake container API.

## Inspection, recovery and retention

`agent-workflow task-state RUN_ID` reads the group's claims, budgets, deadlines
and retained request/provenance records. It can inspect an interrupted run
without dispatching work. Database backup and recovery bundles include task-state
tables. Schema upgrades require existing exclusive workspace ownership and retain
the consistent pre-upgrade database backup.

Individual run cleanup does not reset a shared logical group. Retain its state
while it remains open, including when no run currently references it. Close a
group explicitly only after resolving its claims and stopping other active
members. Owned retention can remove a closed group's detail records when its
final member is cleaned; it retains a closed tombstone so the same group cannot
be reused to reset its allowances. Resuming an existing successful run can still
validate its checkpoints against the closed group's retained state; validation
does not reopen the group or permit task-state writes. Application-owned result
artifacts and provider receipts need their own retention policy; the service
stores references, not their bytes.

Deterministic tests use disposable SQLite databases and fake runtime transport
to cover concurrency, lost acknowledgements, restart, owner fencing, phase
restrictions and shared/standalone mounts. They make no live provider calls or
GitHub publication conformance requests.
