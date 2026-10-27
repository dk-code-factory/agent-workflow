# Workflow runtime design

## Ownership and persistence

One controller holds an advisory workspace ownership lock and a private Unix
HTTP socket. Local clients and stdio MCP are handles onto that owner. Queued work
is admitted to SQLite before a handle is returned; the HTTP request context is
not its execution context. An owner supervises active processes until they stop
before releasing the lock. Explicit foreground execution uses the same lock.

SQLite uses WAL, synchronous FULL, foreign keys, and a single connection per
store. Runs contain immutable admitted snapshots and observational result
records. Attempts, events, artifacts, admission request IDs, cancellation
requests, workers, messages, and external-action intents are separate records.
Schema migrations require a live workspace ownership lock, run transactionally,
and retain a consistent pre-upgrade database backup. Inspection uses a separate
read-only open path and never migrates. Worker and external-intent tables are
part of the schema rather than runtime initialization.

Each originating execution has a persisted generation. Resume atomically
validates eligibility, records its request identity and retry acknowledgement,
and queues a new generation without changing the admitted snapshot or prior
attempts. Cancellation observations remain in generation history; a new resume
does not inherit an earlier cancellation. Delayed cancellation requests are
conditional on the generation observed by that request. Claims and run writes
compare generation and owner; terminal writes invalidate that owner. Execution
contexts also fence step admission/completion and reusable-child writes.

Conditional run claims prevent duplicate resume admission. Step admission is
committed before effects and completion is committed before dependencies run.

The controller bounds run admission and shared execution capacity. The DAG
scheduler admits ready jobs, while agent and command operations acquire shared
slots. Reusable workflow controllers do not hold a slot while their child jobs
execute. Their deterministic child run IDs and the shared originating-run
workspace ID preserve recovery and ownership across nested definitions.

Run scheduling reads at most 100 queued candidates in indexed admission order.
Worker scheduling reads at most 100 idle workers with an eligible queued message
and running origin, stopping when capacity is occupied. The next message is
fetched directly through its eligibility index. Committed admission, enqueue,
and completion wake schedulers, with a one-second reconciliation poll. Origin
membership is materialized and indexed. Compact inspection pages default to 100
items (maximum 1,000), with scoped opaque continuation cursors; compatibility
list methods remain available.

## State transitions and uncertainty

Runs transition queued -> running -> succeeded/failed/canceled. Owner loss moves
running records to needs_attention (or canceled when a durable cancellation
request exists). Recovery never starts an uncertain effect automatically.
Explicit resume durably queues the original snapshot, retains successful
jobs, and reconstructs incomplete jobs from successful step attempts. A prior
running/failed/canceled attempt requires `retry-uncertain` before another effect.

Structured-output tasks checkpoint their completed provider result before
validation or formatting. Repair admission and results have separate durable
phases and retain the originating task attempt ID. Schema-only resume validates
cached results or makes bounded tool-free formatting calls; it does not repeat
the task, even when a terminal checkpoint was lost. Explicit resume starts a new
bounded repair allowance while retaining the complete earlier repair history.

New admission and resume share one controller execution-owner routine. It owns
the deadline, error recording, worker shutdown, and active-map/wait-group
completion. Schema 7 persists a finalization barrier before cleanup. It closes worker
spawn/send/recovery, turn claims, broker calls and invocation launch before
joining work. Finalization cancels and joins active turns,
then publishes the terminal result. Cancellation during this cleanup is reflected
in the final outcome; cleanup uncertainty and worker failures require attention.
Prerequisite failures before a claim are retained as execution-generation
outcomes. Deadline release and cleanup precede active ownership removal.

Job repeats retain every iteration. Completed earlier iterations reuse their
step attempts when reconstructing a resumed repeat. Reusable children reuse
stable IDs and admitted records. Exhausted conditions fail the workflow.

Known credentials are redacted from result data, errors, events, and artifacts;
execution IDs/statuses are never rewritten by redaction. Snapshots containing
credential literals are rejected rather than silently changed. Credential
references are resolved again on resume. Legacy JSON records remain read-only
and cannot acquire replay semantics.

## Process and container lifetime

Each invocation runs under a separate guardian process. A private pipe owned by
the controller determines lifetime. Closing the writer, cancellation, or owner
SIGKILL causes the guardian to stop the tool's process group. Normal completion
also stops background descendants. Leaders are observed before reaping so process-group IDs cannot be reused during
cleanup. Linux additionally adopts and stops detached descendants through a
subreaper. macOS detached-process-tree termination remains a release gate.
No orphan decision relies on a stored PID.

Every supervised invocation has an authoritative registry entry containing the
origin, execution generation, logical/physical IDs, runtime identity, ownership
labels, created container ID, and cleanup state. Intent precedes runtime create;
the inert object identity is committed before attached start. Host guardians
write durable completion acknowledgements after stopping owned work. Native
startup recovery uses these acknowledgements and never a stored host PID.

Containers have a non-root Linux PID 1 guardian, a private read-only controller
lifetime mount and bounded resources. A changing heartbeat is required before
effects; a frozen record cannot replay work after VM recovery. Heartbeat loss
stops the workload independently of host runtime availability. PID 1 checks state
and workspace mount permissions before provider/command execution. SDK container
dependencies are Linux image-owned, rather than mounted host native binaries.

Cleanup verifies the runtime endpoint/store fingerprint and every expected
ownership label, then removes by verified container ID. Failed inspect requires
an independently successful exact listing before absence is accepted. Runtime
failure, changed identity, and foreign labels preserve uncertainty and block
conflicting admission. Background reconciliation is bounded and separate from
scheduling. Cancellation and inspection remain usable during an outage.

Command steps and agent roles share execution policy, launch supervision, and
invocation ownership. The command policy is pinned in the admitted snapshot.
Strict cleanup on macOS requires containers; native macOS provides process-group
cleanup. Runtime/image/UID/VM conformance is a separate release evidence gate.

## Providers and workers

AgentRunner remains the workflow compatibility interface. SessionRunner adds
capability reporting, resumable turn IDs, streamed normalized events, and
terminal provider status. CLI compatibility adapters report no resumable session
support. Claude SDK uses a pinned Node bridge with explicit role instructions,
tools, gateway auth, and suppressed native discovery. Codex app-server speaks
bounded newline-framed JSON-RPC and rejects unconfigured interactive requests.
Its managed-discovery policy is not certified, so live worker admission fails
closed for that backend.

Workers and durable ordered messages belong to an originating run. A send is
acknowledged after enqueue commit; duplicate request IDs return the same message
only for identical content. Turn admission decrements the durable grant before
launch. Child grants are reserved from the parent, and tools/depth/count/queue
limits cannot increase. One turn executes per worker. Session IDs are persisted
as they stream, preserving them even if the terminal result is lost. Owner-loss
turns need explicit acknowledgement; they are not silently replayed. Run
cancellation closes workers and stops their active turns.

The external MCP facade has bounded waits and never holds an execution slot.
Provider-internal worker and external MCP tools share a controller-owned broker.
Exact grants and pinned catalogs live in admitted roles; children intersect role
and parent grants. Provider arguments never supply originating/parent identities.
Tool calls journal argument fingerprints and redacted audit data before dispatch.
Identical completed calls return cached results, and deterministic child/message
IDs prevent double creation or grant reservation. Lost external responses remain
uncertain across restart; no idempotency advertisement permits automatic replay.
Unresolved effects prevent terminal success and remain inspectable.

An invocation-wide capacity lease replaces provider ownership of raw slots.
Only an adapter-attested quiescent barrier suspends it, bounded at 128 suspended
invocations per controller. Every result reacquires before provider execution
continues; parallel callbacks share the same lease. The pinned SDK bridge uses
model-stream completion plus native tool permission/completion hooks to certify
its blocked state. A native hook cannot execute after suspension until the
controller restores capacity. Workflow steps and mailbox workers use this same
broker/lease boundary. Canceling or releasing a lease wakes blocked acquisitions.

Claude bridge protocol 2 has startup negotiation, stable upstream tool-use IDs,
bidirectional callbacks/results, progress, partial usage, and interruption.
Codex uses one RPC reader with correlated calls and independent callbacks, plus
graceful turn interruption. Writable provider state is partitioned by origin and
session owner. Native Codex file authentication remains provider-owned through a
reference, with isolated writable configuration. The internal-only external-token
login API is not used. Schema fixtures generated by upstream 0.164.0 are pinned;
managed policy and quiescence remain uncertified, so worker/tool-subset admission
stays closed for that backend.

## Usage and operational readiness

Usage invocations are admitted before provider calls. Durable observations pin
origin, task attempt, repair or worker-turn identity, provider/model and session.
Claude input normalization includes cache reads and creation; raw outputs remain
unchanged. SDK message counters replace observations with the same message ID.
Codex thread totals become transactional deltas against persisted session
counters; the last-request snapshot is not a turn total. Cached task checkpoints
are counted once, while each actual repair and worker turn has its own identity.
Historical records are backfilled transactionally during schema migration.

Provider-reported cost is retained without prices or estimates. Unknown cost is
null, with known reported cost separately visible. Missing terminal usage retains
partial observations and an incomplete flag. Status, usage CLI, HTTP and MCP
share these normalized summaries. Counter regression remains explicitly
incomplete rather than claiming precise totals.

Liveness is separate from readiness. Admission and scheduling check storage
writes, scheduler heartbeat, cleanup prerequisites, private state permissions,
and state/temporary filesystem capacity independently. Defaults are 64 MiB and
128 free inodes, configurable per controller. Cancellation and inspection remain
usable while admission is unavailable. Metrics use the private Unix socket and
bounded state labels, with no run IDs or prompts. Structured logs omit prompt
content and rotate at 10 MiB with three retained files.

## Development actions and publication

The controller pins a Git baseline before admitting development actions. Writers
get detached worktrees, or independent repositories for containers. Reviewers
get fixed independent clones. Commit actions record intent before staging and
commit, and reconcile an action footer after uncertain completion. Integration
serially cherry-picks writer commits, retaining conflicts and unexpected heads.
The caller's branch and dirty files are not used for implementation or moved.

Publication policy fixes repository, base, remote, required check job IDs, and
independent review job IDs. Checks/reviews must refer to the final commit;
reviews must report clean. A managed clean workspace, matching remote, and
run-specific branch are required. Intent precedes push/PR effects. After an
uncertain response, remote SHA and exact PR head/base/repository are reconciled.
A create-request marker prevents a second create when a prior result remains
uncertain. Unexpected remote changes stop publication. There is no merge action.

A controller-owned pre-push guard checks the remote SHA advertised during the
actual push negotiation, then invokes any existing pre-push hook with the same
input. Git receive-pack's ordinary ref comparison catches later races without
force. Configured workflow check/review reports remain a trusted input.


## Retention and database recovery

An exclusive retention journal freezes each origin's removal plan before effects.
Every worktree/directory removal moves through planned/removing/removed, and
record deletion plus the completion tombstone commit atomically. A crash after
filesystem removal can repeat the same step safely. Retiring origins cannot
resume. Owned provider/session and invocation directories are removed without
following native-authentication links; shared legacy provider state and caller
files are preserved.

Backups use SQLite's consistent snapshot mechanism, including committed WAL.
Restore validates integrity, foreign keys and schema, retains pre-restore state,
and atomically replaces the database under exclusive ownership. Restored
non-successful runs and mailboxes are quarantined because post-snapshot effects
cannot be assumed absent. Old queued messages never automatically replay.
Recovery bundles add engine-owned managed workspaces and provider state while
excluding native credential stores; original Git administration and matching
managed paths are still required where worktrees use shared administration.
