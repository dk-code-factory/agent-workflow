# Release evidence and gates

This is an implementation candidate. The production-readiness declaration in the
requested plan is gated on the remaining items below; implemented API surfaces
are not a substitute for conformance evidence.

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
  detached descendant stops after its leader completes. macOS currently enforces
  process-group lifetime; detached-process-tree termination still needs work.
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

1. Complete provider-internal worker MCP integration and configurable per-role
   MCP grants (the bridge currently denies all MCP servers), including a waiting
   provider parent's capacity handoff to its children. External MCP polling is
   bounded and capacity-free, but does not certify this scenario.
2. Certify suppression of native Codex skills/plugins/memory/role discovery
   while retaining provider managed restrictions. The transport has native
   session support, but reports `managed_policy: false` and rejects live worker
   creation until this is certified.
3. Refresh native Claude authentication through its provider-owned login flow,
   then repeat the live native probe. Configure and test the explicit Claude
   gateway when the user's deferral is lifted. Complete a mixed real-provider
   development pilot through both CLI and MCP with usage/cost accounting.
4. Broaden doctor/preflight conformance across providers and container hosts.
   Doctor now checks backend versions/capabilities, cached native status, gateway
   models endpoint status, container runtime/image availability, Git baseline,
   and state permission. Native cached status cannot certify model acceptance;
   the observed Claude 401 demonstrates why the budgeted live gate is separate.
5. Finish named-container recovery during runtime/daemon failures and test
   process/container owner loss, disk full, failures at every durable transition,
   competing controllers, and unavailable credentials on Linux/macOS. The
   guardian handles ordinary cancellation/owner loss; cleanup failures must not
   be treated as proof a container stopped. Native provider interruption is
   currently supervised process termination with retained session IDs, rather
   than a fully certified graceful provider interruption handshake.
6. Broaden retention fault tests across interruptions during removal. Terminal
   group cleanup now reconciles/removes worktrees and independent clones before
   deleting originating-run and reusable-child records, and refuses uncertain
   work. Native provider-owned transcript retention is not controlled here.
7. Expand GitHub recovery tests against the disposable repository. The guarded
   non-force push now checks the SHA advertised during actual negotiation,
   preserves original pre-push hooks, and has deterministic race coverage.
   Publication gating trusts configured workflow job reports; stronger
   check/review attestations would be additional hardening.
8. Finish cost accounting and package/container distribution. Run/event/bridge
   envelopes are versioned; provider terminal status, provider version, queue
   timing, and failure classes are reported. Legacy text/response/usage meanings
   remain intact. Native subscription usage does not report a dollar cost.

A host `/tmp` inode exhaustion was encountered during verification. Subsequent
build/test temporary files were redirected to this checkout's ignored
`.agent-workflow/build-tmp`. This is an environment workaround, not a claim of a
completed disk-full fault-injection suite.
