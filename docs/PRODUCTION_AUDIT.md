# Production-readiness implementation and evidence audit

The stable signed-tag release pipeline and its current blockers are documented in
[RELEASE_PIPELINE.md](RELEASE_PIPELINE.md). That runbook supersedes older manual-only
publication instructions below; historical evidence is not current certification.

Audited against the eight-phase production-readiness plan on 2026-10-16.
This is an implementation candidate, not a production certification. The
working tree is not a released commit. The evidence below distinguishes local
regression coverage from provider/platform certification.

## Implementation map

| Plan requirement | Implementation | Deterministic evidence |
| --- | --- | --- |
| Durable resume, request deduplication, immutable snapshots, generation cancellation | `pkg/workflows/executions.go`, `ownership.go`; controller admission | `TestResumeGenerationsAreAtomicAndDeduplicated`, `TestQueuedResumeSurvivesReopenAndRecovery`, controller resume/cancellation tests |
| Conditional claims, task writes and final outcomes | SQLite generation/owner comparisons and context fences | `TestRecoveryRetainsClaimedOwnerAndRejectsStaleCompletion` |
| Shared new/resume owner and cleanup before terminal status | `pkg/controller/execution.go`, executor finalization; schema-7 finalization barrier | `TestResumedExecutionStaysRunningDuringFinalCleanup`, `TestStopRunJoinsWorkersAndPreservesCleanupUncertainty`, `TestFinalizationBarrierRejectsLateWorkerAdmission` |
| Resumed credential/preflight failures retained | Execution owner records prerequisites and failure class | `TestResumeAdmissionRecordsUnavailableCredentials` |
| Ordered transactional upgrades, exclusive owner, preserved snapshots and pre-upgrade backup | Migrations 1–7, `OpenSQLiteInspection`, ownership lock | Migration preservation/rollback/newer-schema tests; replacement-connection durability test |
| Bounded indexed run/worker/message scheduling and wakeups | SQL admission/message eligibility, origin column, batches of 100, reconciliation poll | Queued terminal-history and completed-mailbox-history tests |
| Paginated compact inspection; legacy methods/shapes | `pagination.go`, CLI and opt-in HTTP pages, scoped cursors, limits 100/1,000 | `TestInspectionPaginationAndReadOnlyOpen` |
| Named external MCP servers and exact role grants | `mcp_servers.go`, stdio/Streamable HTTP, referenced credentials/timeouts | Stdio/HTTP credential-isolation and exact-grant tests |
| Pinned catalogs and inherited child restrictions | Snapshot tool catalogs/fingerprints; native/MCP/workspace/container/limit intersections | Catalog-change test; child permission, scope, container and grant tests |
| Controller identity injection and schema validation | `tool_broker.go`; invocation owner/generation fencing | Cross-run denial, exact-integer, denied-identity tests |
| Durable tool requests, duplicates and idempotent spawn | Tool journal and deterministic child/message identities | Broker deduplication/redaction, stable-spawn reservation tests |
| Uncertain external effects retained without automatic replay | Uncertain journal state blocks new calls in the generation; explicit resume acknowledgement creates a new generation | Broker lost-response test; bridge aborts uncertain turns |
| Acquire/suspend/reacquire/release; bounded suspended turns | Invocation-level leases, maximum 128 suspended, callback handoff barriers | Parallel reacquisition/cancellation test; workflow→child→grandchild at capacity one; actual SDK fake-gateway broker test |
| Claude bidirectional protocol 2, supported SDK tools, stable IDs and startup negotiation | `bridge.mjs`, `bridge_protocol.go`, locked SDK 0.3.311 | Version-mismatch, bridge contract and actual SDK fake-gateway tests |
| Graceful interruption, retained identity/partial usage, separate tool-free repair | SDK interrupt and Codex turn/interrupt; five-second termination fallback; durable usage callbacks | Interruption/partial-usage tests; task/repair checkpoint tests |
| Codex single reader and generated upstream fixtures | `app_rpc.go`, `codex_control.go`, 0.164.0 schema fixtures/fingerprint; native preflight/image version pin and handshake enforcement | Correlated callback, malformed-frame, pinned-version and unknown-request-denial tests |
| Codex dynamic-tool protocol and managed-policy enforcement | Dynamic-tool transport exists; `ManagedPolicy=false`; unsupported subsets/grants fail before admission | Protocol fixture checks and capability rejection. **Upstream policy/quiescence certification remains open.** |
| Engine-owned state separated from native authentication | Per-origin/session state, native Codex file-auth reference; no native config copy; compatibility environment allowlist and private credential-free version probes | State-partitioning, authentication-preservation, token-redaction and version-probe isolation tests |
| Durable invocation launch/cleanup registry | `invocations.go`, `pkg/runtimeexec`, launch intent→create→identity commit→attached start | Lost-create, failed-identity-commit, outage, changed-runtime and foreign-label tests |
| Container PID 1 and host guardian | Heartbeat, non-root/mount checks before effects, descendant termination and durable completion acknowledgement | Guardian/owner-SIGKILL tests; real Linux amd64 rootless Podman shell test |
| Cleanup recovery and exact ownership | Container IDs/runtime fingerprint/labels, no persisted-host-PID signals; retained uncertainty | Cleanup/runtime fixture tests; batch filtering regression prevents live invocations hiding cleanup |
| Shared command/agent policy and macOS guarantee | Shared launcher, pinned execution policy; native macOS process groups; strict cleanup requires containers | Policy-pinning and strict-macOS-policy tests |
| Docker/Podman and VM host/UID handling | Runtime parameter, explicit UID/GID, independent Git administration and guardian mount checks | Structural tests and local Podman. **Genuine Docker/macOS VM and arm64 execution remain unverified.** |
| Durable normalized usage without estimates/budgets | `usage.go`, historical backfill; original task/repair/worker identities | Cache-inclusive Claude input, cumulative session deltas, failed calls, repair/reuse, partial usage, overflow tests |
| Unknown cost/incomplete final usage preserved | Nullable totals and reported-cost subtotal; counter reset/overflow remain incomplete | Accounting and actual SDK executor accounting tests |
| Status/CLI/HTTP/MCP totals | `normalized_usage`, usage command/endpoints/tool | Compatibility and diagnostics tests |
| Liveness/readiness and unavailable-prerequisite admission | Private socket health/ready; storage write, heartbeat, cleanup, state/temp bytes/inodes | Disk-prerequisite rejection while inspection stays usable; private metrics tests |
| Redacted structured logs, rotation, bounded-label metrics | Operational logger, 10 MiB/three files, private metrics | Rotation and prompt-free metric tests |
| Doctor and locked bridge setup without model calls | Provider versions/capabilities, dependencies, authentication prerequisite, runtime/image, state/temp capacity and cleanup policy | Ordinary CLI smoke; preflight fixtures. **Host/provider matrix requires manual evidence.** |
| Partitioned retention, frozen removal intent and resumable boundaries | `retention_journal.go`, development cleanup, tombstone after DB deletion | Interrupted removal/database boundary, symlink refusal, other-origin/native-auth preservation tests |
| Consistent backup/restore and recovery bundle | Exclusive owner; WAL snapshot, integrity/FK/schema checks, pre-restore copy, restored-work quarantine | Backup, unsupported-source, bundle credential-exclusion and maintenance ownership tests |
| Publication exact reconciliation and caller preservation | Durable commit/push/PR intent; changed-base/head checks, two clean reviews/final checks retained | Lost push/create, cancellation, deleted branch, closed PR, hooks, conflicts and fake end-to-end publication tests |
| One build identity and four archive targets | `internal/buildinfo`, release scripts; binary/examples/licenses/bridge lock/manifests | Local Linux/macOS amd64/arm64 archive builds and checksum/content verification; native extracted fake-provider smoke |
| Pinned multi-architecture worker images | Digest-pinned bases, pinned Codex/SDK packages, no credentials, guardian/bridge labels | Local amd64/arm64 OCI builds; amd64 execution only |
| Dependency inventories, checksums and vulnerability reports | Distribution inventory/SBOM and Go/npm audit scripts | Local reports passed; reports are time-dependent, not permanent security certification |
| Deterministic CI across OS/runtime and actual pinned SDK | Linux/macOS jobs, fake SDK gateway, Docker/Podman shell conformance, isolated filesystem job | Configuration is committed in the candidate tree. **Remote CI results are not recorded.** |
| Separate real-provider/publication harness and safety guards | `cmd/workflow-conformance`, named manifests; refuses CI, explicit flags, matching build, private evidence outside Git | Guard/manifest/evidence-location tests. **Real scenarios were not run by this implementation work.** |
| Operations and intentional publication | README, DESIGN, DISTRIBUTION, RELEASE, repository agent instructions | Documentation/source audit; build scripts never publish |

Focused fault injection covers the boundaries named by the tests above. It does
not certify every possible kernel, filesystem, provider or runtime failure. In
particular, the full acceptance matrix's SIGKILL-at-every-boundary,
malformed/duplicate provider frame combinations, macOS runtime/VM restart, and
real publication fault scenarios still need recorded conformance evidence.

## Verification record

Local command results and artifact checks are summarized in
[`conformance/evidence/deterministic-linux-amd64.json`](../conformance/evidence/deterministic-linux-amd64.json).
Generated archives, OCI images, full test logs, dependency inventories and audit
reports remain in ignored local artifact directories. A source fingerprint in
release metadata ties an archive to its exact source; these working-tree builds
are not signed release artifacts.

The isolated byte/inode exhaustion fixture refused to run when this host denied
user-namespace creation. No shared filesystem was deliberately exhausted. Its
separate privileged CI job must pass before that conformance gate can close.

## Certification gates

1. Record passing remote Linux/macOS deterministic CI, isolated filesystem fault
   tests, genuine Docker and Podman runtime conformance.
2. Execute the arm64 worker image and certify macOS VM-backed mounts/UID mapping
   and container owner-loss/runtime restart behavior. Native macOS guarantees
   remain limited to process groups.
3. Obtain upstream Codex managed-policy/quiescence capability or leave it
   unsupported. Keep admission rejection in place; do not substitute a backend
   or weaken policy to close the gate.
4. With explicit named-scenario authorization, record current Claude/Codex native,
   gateway, resume, interruption, discovery, nested tools, external MCP and
   mixed CLI/MCP development evidence for every claimed provider/platform pair.
5. With separate explicit disposable-repository authorization, record publication
   recovery conformance. Historical pilots are not current-build certification.
6. Review all sanitized evidence and dependency reports against the exact clean
   release commit, rebuild/verify distribution artifacts, then intentionally
   publish. Missing or failed checks remain visible; no automatic publication.

Single-user trusted workflows remain the security boundary. Multi-tenant
hosting, automatic merging, cryptographic review attestations, spending budgets,
provider forks and unreported-cost estimates are outside this release.
