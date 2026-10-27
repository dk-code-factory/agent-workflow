# Release evidence and gates

The stable signed-tag release pipeline and its current blockers are documented in
[RELEASE_PIPELINE.md](RELEASE_PIPELINE.md). That runbook supersedes older manual-only
publication instructions below; historical evidence is not current certification.

This is an implementation candidate. The production-readiness declaration in the
requested plan is gated on the remaining items below; implemented API surfaces
are not a substitute for conformance evidence.

## Production-readiness implementation progress

The candidate implements the eight delivery areas. A requirement/evidence map
and the remaining certification gates are maintained in
[PRODUCTION_AUDIT.md](PRODUCTION_AUDIT.md). Production certification is incomplete;
the current working tree has not been released.

- Schema 3 adds durable tool-call journals and workflow invocation grant
  reservations. Upgrades remain transactional under the ownership lock and retain
  a consistent private `state.pre-v3.db` backup. Inspection never migrates.
- The controller brokers exact role/parent grants for engine worker tools and
  stdio/Streamable HTTP MCP servers. Selected catalogs, descriptions, schemas and
  fingerprints are pinned. Server credentials stay outside provider workers.
  Duplicate completed calls return cached results; child creation/reservation is
  idempotent; lost external responses stay uncertain and prevent terminal success.
- Invocation leases support bounded suspension, cancellation and parallel
  reacquisition without double release. Workflow → child → grandchild execution
  at capacity one passes deterministic coverage.
- Claude protocol 2 uses supported SDK custom tools, stable model call IDs,
  startup version negotiation, bidirectional callbacks and interruption. Actual
  pinned-SDK fake-gateway tests cover multiple custom tools at capacity one,
  resumed context, structured formatting, and suppressed project memory/hooks.
  Model-stream completion and native hook permissions bound capacity handoff.
- Codex has a single correlated RPC reader, independent callbacks, graceful
  interruption with retained identity/partial usage, isolated writable state and
  native file authentication references. Generated 0.164.0 protocol fixtures and
  their schema fingerprints are included. Native preflight, image labels and
  the handshake enforce the upstream version pin before task effects. The internal-only external ChatGPT
  token-login API is deliberately excluded. Managed policy/quiescence remains
  uncertified; unsupported managed tool subsets are rejected before execution.
- Schema 4 adds the invocation registry. Agent, worker, external stdio, and
  command invocations preserve origin/generation ownership. Native guardian
  completion acknowledgements survive owner loss; recovery never signals a
  persisted host PID.
- Containers use inert create, durable identity commit and attached start, with
  exact runtime/label verification before ID-based removal. Linux PID 1 requires
  a live controller heartbeat and validates non-root mounts before effects.
  Unknown cleanup blocks conflicting admission and is reconciled when the
  runtime returns. Command policy is shared and pinned across resume; strict
  macOS cleanup requires containers.
- Deterministic fake-runtime coverage now verifies normal command isolation,
  lost create responses, identity persistence failures before start, runtime
  outages and recovery, changed runtime identities, foreign ownership labels,
  and pinned command policy. Guardian tests verify frozen-heartbeat refusal,
  lifetime-loss termination, and durable completion after owner SIGKILL.
- Schema 5 adds durable invocation/observation usage accounting and migrates
  available historical task/repair/worker usage. Cached-task reuse is deduplicated;
  failed calls and repairs count; Claude cache input is inclusive; Codex session
  snapshots become deltas. Unknown cost and incomplete final usage remain
  explicit. Status, CLI, HTTP and MCP expose normalized totals.
- Readiness checks storage, scheduler heartbeat, state permissions, cleanup and
  independent state/temporary byte/inode capacity. Metrics use the private
  socket without run/prompt labels. Controller logs are structured, redacted,
  rotated at 10 MiB and retain three files. Operational admission settings are
  configurable. Accounting, readiness/inspection availability and rotation tests
  pass, including the actual SDK fake-gateway accounting path.
- A real SQLite-full/inode exhaustion fixture is committed and wired into a
  separate Linux CI job. It verifies a bounded private tmpfs before writing.
  Local execution was denied at user-namespace creation; it did not exhaust a
  shared filesystem. That environment-specific conformance remains unverified
  until the dedicated job records evidence.
- State partitioning by originating run and session owner is implemented.
  Resumable provider-state retention and private recovery bundles are now implemented below.

The durable resume regression cases remain covered: concurrent admissions,
request deduplication, credential failures after admission, generation-scoped
cancellation, stale ownership/effect writes, owner-loss recovery, worker joining,
cleanup uncertainty, private backups, read-only inspection, pagination, and
history-independent queue reads.

Schema 6 adds resumable retention plans and completion tombstones. Cleanup
journals removal boundaries and removes one origin's workspaces/provider state
without following native authentication references. Backup/restore and private
recovery-bundle commands are implemented, including stopped-owner enforcement,
consistent WAL snapshots, integrity/schema validation, pre-restore preservation,
and quarantine of restored runnable work/mailboxes. Interrupted removal and
database-boundary retries, bundle credential exclusion, and CLI ownership tests
pass.

Publication regression coverage now includes lost push responses, cancellation
after create, changed remote base, deleted branches and closed PRs. Durable push
markers prevent repeating an uncertain/deleted push; completed/ambiguous PR
outcomes never silently create another PR.

Distribution tooling now builds Linux/macOS amd64/arm64 archives containing
binaries, examples, licenses, bridge sources/lockfile and the separate manual
conformance executable/manifests. All four archives were built and verified;
the native extracted Linux archive passed init/validate/doctor/fake-provider
execution/accounting. Release metadata records commit, dirty state and a source
fingerprint. Dependency inventories, SBOM and current vulnerability reports are
generated. Audits identified the older MCP SDK/toolchain; MCP Go SDK is now pinned
to patched 1.4.1 and release Go to 1.27.2, with passing Go/npm audits.

Digest-pinned Linux amd64/arm64 worker OCI archives were built locally with no
credentials. The amd64 image passed a real rootless Podman command isolation and
verified-cleanup test. arm64 image execution, genuine Docker, macOS runtime/VM
execution and CI results remain unverified gates. No image/release was published.
The Docker executable on this development host is a Podman alias, so it is not
counted as independent Docker evidence.

The manual conformance harness refuses CI and requires explicit live scenario
authorization. Sanitized manifests cover native providers, gateway structured
output, session resume/interruption, discovery canaries, nested capacity-one
workers, uncertain external MCP responses, containers/owner loss, CLI/MCP mixed
development and explicitly allowlisted disposable publication. Guard and manifest
tests ran; actual real-provider/publication scenarios remain unexecuted.

The implementation/evidence audit is recorded in `PRODUCTION_AUDIT.md`.
Schema 7 adds a durable finalization barrier: worker sends, spawns, recovery,
turn claims, broker calls and invocation launch cannot race final cleanup.
SQLite connection safety settings survive connection replacement; cleanup
reconciliation filters live invocations before applying its batch limit.
Manual provider/platform certification and unsupported upstream Codex managed
policy remain open release gates.

No live-provider calls, authentication refresh, remote publication, or new
provider/platform production certification evidence was produced by this work.

## Recorded verification (2026-10-16)

- Deterministic Go suites cover compatibility, admission deduplication, WAL state,
  ordered mailboxes, inherited grants, interruption, guarded recovery, secret
  redaction, commands, repeat exhaustion, structured repair, Git isolation,
  conflict retention, lost publication responses, unexpected remote changes, and
  schema-only recovery without repeating task actions. A full credential-free
  Go/Git/fake-provider/fake-GitHub regression exercises two development iterations
  and verifies exactly one publication and preserved caller files.
- The actual pinned Claude SDK passed a local fake Messages API test: initial
  turn, resume with prior context, no project memory/hooks, one model request
  per turn, and no provider-internal repetition after a schema mismatch. Native
  auto-title generation was found and disabled with an explicit title. This is
  a credential-free contract test, not the deferred live gateway smoke.
- Linux process guardian test kills an execution owner with SIGKILL and verifies
  its command descendant stops. Linux subreaper coverage also verifies that a
  detached descendant stops after its leader completes. macOS native execution enforces
  process-group lifetime; strict descendant cleanup requires containers.
- Linux rootless Podman container execution test passed using an existing pinned
  local image. The test checks GitHub credential exclusion and socket exclusion;
  structural tests check read-only mounts and reject shared Git administration.
- Deterministic standalone CLI pilot passed through implementation, tests,
  independent review adapters, final checks, commit, push, and one PR:
  https://github.com/dkropachev/agent-workflow-pilot-20261016/pull/1
- Deterministic MCP pilot passed through the same workflow and one separate PR:
  https://github.com/dkropachev/agent-workflow-pilot-20261016/pull/2
- The disposable repository is private and was explicitly requested by the user.
  Both pilots used fake provider executables and made no model calls. Neither PR
  was merged. A prior failed pilot retained its artifacts and exposed a nested
  workspace scope bug, fixed by sharing originating-run workspace ownership.
- Live Codex app-server 0.161.0, native ChatGPT authentication: `SMOKE_OK` passed.
  The later two-turn live session-resume probe also passed, retaining the same
  thread and recalling a public nonce. Total reported tokens across the three
  live Codex turns: 38,469 input and 41 output (12,672 input tokens cached on
  resume). Native ChatGPT authentication reported no dollar-cost figure.
- Native Claude Code 2.1.81 reports a cached authenticated Max session, but the
  actual probe returned HTTP 401, `OAuth access token is invalid`. The direct
  diagnostic reproduction reported `total_cost_usd: 0` and no token usage.
- macOS arm64 cross-compilation passed locally; runtime/race execution on macOS
  remains delegated to the configured CI matrix.
- The user authorized up to $60 total for live checks. Claude probes had a $2
  request cap. No gateway model calls were made. The user explicitly deferred
  the gateway checks after no route/reference was available.

## Required work before production certification

See the explicit gates in [PRODUCTION_AUDIT.md](PRODUCTION_AUDIT.md). Remaining
work is evidence for current provider/platform/runtime combinations, remote CI
and isolated filesystem failures, upstream Codex managed policy, and an exact
clean release commit. Broader destructive/failure scenarios use disposable
fixtures. Real-provider or publication runs require explicit named-scenario
requests under repository instructions; ordinary release preparation does not
authorize them. Native Claude authentication and gateway checks remain
prerequisites for their respective live scenarios; this work does not refresh
provider login.

A host `/tmp` inode exhaustion was encountered during verification. Subsequent
build/test temporary files were redirected to this checkout's ignored
`.agent-workflow/build-tmp`. This is an environment workaround, not a claim of a
completed disk-full fault-injection suite.
