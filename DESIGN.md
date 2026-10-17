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
Conditional run claims prevent duplicate resume admission. Step admission is
committed before effects and completion is committed before dependencies run.

The controller bounds run admission and shared execution capacity. The DAG
scheduler admits ready jobs, while agent and command operations acquire shared
slots. Reusable workflow controllers do not hold a slot while their child jobs
execute. Their deterministic child run IDs and the shared originating-run
workspace ID preserve recovery and ownership across nested definitions.

## State transitions and uncertainty

Runs transition queued -> running -> succeeded/failed/canceled. Owner loss moves
running records to needs_attention (or canceled when a durable cancellation
request exists). Recovery never starts an uncertain effect automatically.
Explicit resume atomically claims the original snapshot, retains successful
jobs, and reconstructs incomplete jobs from successful step attempts. A prior
running/failed/canceled attempt requires `retry-uncertain` before another effect.

Structured-output tasks checkpoint their completed provider result before
validation or formatting. Repair admission and results have separate durable
phases and retain the originating task attempt ID. Schema-only resume validates
cached results or makes bounded tool-free formatting calls; it does not repeat
the task, even when a terminal checkpoint was lost. Explicit resume starts a new
bounded repair allowance while retaining the complete earlier repair history.

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

Container clients have a generated name and fixed cleanup command owned by the
guardian. Session containers mount an independent repository rather than shared
host worktree administration, private provider state, and an optional read-only
SDK bridge/native authentication file. Only explicit provider auth environment
names are forwarded. Container cleanup is checked after tool completion.
Runtime outage recovery remains a release gate.

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
Provider-internal nested worker tools and parent slot handoff remain a release
gate, explicitly documented rather than emulated with ephemeral conversations.

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
