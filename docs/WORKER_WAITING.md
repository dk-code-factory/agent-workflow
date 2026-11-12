# Internal durable message waiting

`WorkerManager.WaitMessage` provides the controller-internal foundation for future
pipeline agent-task integration. It adds no public worker operation or YAML wait
syntax and is not yet called by pipeline agent tasks. Each wait binds an originating
run, logical operation, exact worker and message, and absolute deadline. Registration and terminal observations commit to
SQLite schema 11 before the caller can admit a dependent effect. Reattachment
retains the original deadline and rejects changed message bindings.

Pending waits hold no execution slot and start no helper container or provider
process. Each wait occupies its caller's goroutine. Pipeline integration must retain
the caller's bounded workflow controller reservation. Independent waiters can
observe the same message; unrelated newer mailbox entries cannot satisfy a wait.

Completion and worker-state commits notify subscribers. Subscribers register before
reading durable state. The worker scheduler's shared one-second sweep repairs missed
or coalesced notifications without one polling timer per waiter. Terminal receipts
recorded before subscription are immediately visible. Closed or stopped producers
with unresolved messages fail instead of waiting indefinitely.

Cancellation detaches the in-memory waiter and preserves mailbox evidence and the
original deadline for explicit recovery. Origin ownership, cancellation and the
finalization barrier are checked transactionally before returning a retained result.
Controller finalization still stops and joins all originating-run workers before
publishing terminal completion. Waits never resend provider work.
