# Internal durable message waiting

[`agent/run` tasks](AGENT_TASKS.md) use `WorkerManager.WaitMessage` after creating
or reopening their durable worker and enqueuing the exact task message. Waiting
is controller-owned; workflows do not need a separate `worker/wait` step or a
polling helper. Each wait binds an originating run, logical operation, exact
worker and message, and absolute deadline. Registration and terminal observations
commit to SQLite before the caller can admit a dependent effect. The
`worker_waits` table was introduced in schema 11. Reattachment retains the
original deadline and rejects changed message bindings.

Pending waits hold no execution slot and start no helper container or provider
process. The worker's actual provider turn acquires shared execution capacity, so
a task can dispatch and wait with execution capacity set to one. Each wait
occupies its caller's goroutine. A controller-managed run retains its active-run
reservation under `controller.max-active-runs` (default four); waiting does not
admit another top-level run into that reservation. Independent waiters can
observe the same message; unrelated newer mailbox entries cannot satisfy a wait.

Ordinary worker saves and completed turns notify subscribers after committing.
Subscribers register before reading durable state. Recovery commits can rely on
the worker scheduler's shared one-second sweep, which also repairs missed or
coalesced notifications without one polling timer per waiter. Terminal receipts
recorded before subscription are immediately visible. Closed or stopped producers
with unresolved messages fail instead of waiting indefinitely.

Cancellation detaches the in-memory waiter and preserves mailbox evidence and the
original deadline for explicit recovery. Origin ownership, cancellation and the
finalization barrier are checked transactionally before returning a retained result.
Controller finalization still stops and joins all originating-run workers before
publishing terminal completion. Waits never resend provider work.
