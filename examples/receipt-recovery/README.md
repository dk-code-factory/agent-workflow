# Receipt-based recovery fixture

This embedding Go host runs the local fixture proposed in [issue #90](https://github.com/dk-code-factory/agent-workflow/issues/90).
It uses the real workflow controller and registered-action journal, a separate
SQLite fake external counter, and no model credentials, Docker, or GitHub calls.
The fake is deliberately non-idempotent: replaying an increment increments twice.
This is an example of the existing recovery contract, not a LazyMind integration
or a new retry policy.

Run from the repository root with the repository's Go toolchain:

```sh
go run ./examples/receipt-recovery > receipt-evidence.json
go test -race ./examples/receipt-recovery
```

The default command uses disposable temporary storage and removes it on exit.
To retain the databases and evidence for inspection:

```sh
go run ./examples/receipt-recovery -output /tmp/receipt-fixtures > receipt-evidence.json
```

`-output` creates a new `receipt-recovery-*` directory under the supplied parent;
it preserves existing files and prints the owned directory to stderr. Each
scenario has an `engine/` journal, `external.sqlite`, and generated workflow.
`evidence.json` contains the same JSON printed to stdout. Remove only that newly
created fixture directory when finished inspecting it. Never use these synthetic
receipts as evidence of a real external publication.

## What it exercises

| Scenario | Injected failure | Explicit resume result | External counter |
| --- | --- | --- | --- |
| `unknown` | Increment commits, response is lost, reconciliation says unknown | Needs attention with both retry acknowledgements | Stays 1 |
| `lookup_error` | Increment commits, response is lost, receipt lookup fails | Needs attention; the lookup error remains distinct from proof of absence | Stays 1 |
| `completed` | Increment commits, response is lost, authoritative fake receipt is recovered | Output/evidence/receipt pass checkpoint validation; completion commits without another handler call | Stays 1 |
| `no_effect` | Real action admission commits, its acknowledgement is lost before the handler runs | Proof of absence commits before a new physical attempt with the same logical operation key | 0 before resume, 1 after |

Between failure and resume, each scenario stops its controller, closes both
SQLite databases, and reconstructs its registered Go host from durable state.
Each resume uses `POST /runs/<id>/resume` with an explicit `request_id` and
`retry_uncertain` acknowledgement. Repeating the same request returns the same
execution generation and creates exactly one `run.resume_admitted` event.
Changing its acknowledgement returns a conflict. A queued resume acknowledgement
is recorded separately from the eventual execution status; acceptance is not
proof that the mutation completed.

Terminal status may be visible before the controller releases its execution
owner. The fixture retries that specific active-owner admission rejection with
the same resume identity and acknowledgement, bounded by a ten-second deadline.
Other conflicts and request errors fail the scenario immediately.

The evidence includes the run ID, original and subsequent physical action
attempts, logical operation key, execution generations, resume identities and
acknowledgements, reconciliation observations/proofs, and counter values. The
new handler in the `no_effect` case reads the committed original attempt before
incrementing, so an in-memory absence decision alone cannot satisfy the fixture.

For each lost-response case, a separate `negative-control.sqlite` deliberately
performs the same operation twice. The same counter assertion must reject the
result; its rejection is retained in `negative_control`. This keeps each recovered
scenario's `external.sqlite` at one while demonstrating that accidental replay
would fail the checker. A failed scenario exits nonzero.

## Embedding boundary

The command registers `action/increment` through `workflows.RegisterAction` and
runs `controller.Server` with a `controller.Client`. The stock CLI does not load
Go plugins. The admission-loss wrapper delegates the real SQLite admission first
and then injects the acknowledgement error; the remaining journal and controller
operations use the normal implementation.

The fake provider owns its receipt lookup and authoritative absence proof. A
successful lookup, a completed control request, or `retry_uncertain=true` alone
cannot authorize replay of an uncertain registered external mutation. See
[registered Go actions](../../docs/REGISTERED_ACTIONS.md) for the full contract.
Starting a different workflow run is outside this fixture: its run-scoped operation
key does not implement cross-run application deduplication.
