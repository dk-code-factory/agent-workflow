# Agent Workflow

A Go controller for single-user Claude/Codex workflows on Linux and macOS. It
supports durable workflow admission, guarded recovery, command steps, bounded
iterations, worker mailboxes, isolated Git workspaces, and GitHub publication.

Embedding Go applications can also [register typed application actions](docs/REGISTERED_ACTIONS.md)
with durable provenance, checkpoint validation, and receipt-based mutation recovery.

**Release status: implementation candidate, not yet production certified.** See
[release evidence and outstanding gates](docs/RELEASE.md). The deterministic CLI
and MCP publication pilots pass. Codex app-server has passed a native-auth live
probe; live Claude authentication and the gateway gate remain unverified.

## Build and run

Requires Go 1.26+. Install only the providers selected by your roles. The SDK
bridge also requires Node 22+ and `npm ci --prefix bridge/claude`.

```sh
make build
./bin/agent-workflow validate workflows/parallel-review.yml
./bin/agent-workflow run --input task='Review this repository' workflows/parallel-review.yml
```

Flags precede the workflow reference. `--workspace DIR` selects a workspace
containing `agents.yml` and `workflows/`. `--config FILE` selects a configuration
relative to that workspace. Existing workflow syntax and successful `text`,
`response`, and `usage` outputs remain supported. New run/result and event
records carry a version; provider terminal status, version, and queue timing are
recorded without changing those legacy outputs.

`run` starts a workspace controller when necessary, admits the request durably,
and waits for its result. Disconnecting the client or reaching its `--timeout`
leaves admitted work running. Use `--detach` for an immediate JSON handle.
`--request-id ID` makes admission retries return the same run; reusing the ID
with different inputs is rejected. Controller configuration changes require a
controller restart. `--foreground` provides an attached execution path with
exclusive workspace ownership and a whole-run deadline.

For CI, start the controller explicitly and retain its process until work ends:

```sh
agent-workflow controller --workspace .
# In another process:
agent-workflow run --workspace . --detach workflows/parallel-review.yml
```

Default execution capacity is four across jobs, reusable workflows, and live
workers. The controller admits at most 128 queued runs and runs at most four
workflows concurrently. Each controller execution has a 30-minute deadline.
These server limits are also configurable through the Go API. The CLI client's
`--timeout` defaults to 30 minutes; use `0` to disable its wait deadline.

## Roles and authentication

```yaml
agents:
  claude:
    provider: claude-cli
    model: sonnet
    permission-mode: dontAsk
  codex:
    provider: codex-app-server
    sandbox: workspace-write
    authentication:
      mode: native
```

Supported providers:

| Provider | Authentication | Live worker sessions |
| --- | --- | --- |
| `claude-cli` / `claude` | Installed Claude's native login | No; fresh process per call |
| `codex-cli` / `codex` | Installed Codex's native login | No; fresh process per call |
| `claude-sdk` | Explicit gateway URL and credential environment reference | Yes |
| `codex-app-server` | Explicit `native` or `api-key` credential reference | Session transport implemented; managed worker policy is not yet certified |

The native CLI adapters are trusted-host compatibility adapters. They retain
provider-owned authentication and native configuration. They are **not** a
filesystem isolation boundary. Claude `plan`/`dontAsk` permissions do not provide
a Codex filesystem sandbox. The engine rejects live worker creation on a backend
that cannot enforce its managed role policy.

An engine-owned Claude SDK role is configured as follows:

```yaml
agents:
  reviewer:
    provider: claude-sdk
    bridge: bridge/claude/bridge.mjs
    model: sonnet
    instructions: [roles/reviewer.md]
    tools: [Read, Glob, Grep]
    limits:
      max-turns: 16
      max-depth: 4
      max-queued-messages: 16
      turn-timeout: 10m
      max-output-bytes: 1048576
    authentication:
      mode: gateway
      endpoint: https://gateway.example.invalid
      credential-env: WORKFLOW_CLAUDE_KEY
```

Instruction files resolve relative to the configuration file and their contents
are pinned in the admitted snapshot. SDK roles disable filesystem settings,
native skills, plugins, agent discovery, and unapproved MCP servers; provider
managed restrictions remain applicable. Native delegation and discovery tools
are rejected. SDK repair calls receive no tools and no original task prompt. Bridge protocol 2
negotiates the pinned SDK before effects and supports correlated callbacks and
graceful interruption with a five-second guardian fallback.
The bridge assigns its own session title and disables auto-memory/consolidation;
provider-native structured-output attempts are limited to one, leaving repair
under the engine's durable tool-free phase.

For Codex direct credentials, select `authentication.mode: api-key` and provide
`credential-env`. The app-server authenticates through its native login API and
uses engine-owned provider state. Custom Codex endpoints are not implemented.
Native app-server authentication references a provider-owned `auth.json` through
an engine-owned file-store link; set `authentication.credential-file` explicitly
or use the native `CODEX_HOME/auth.json` default. Writable provider state and
`HOME` remain private to the run/session owner. Keychain-only app-server login
is not supported by this route; the native CLI compatibility adapter retains its
provider-owned login behavior. Raw app-server arguments and managed tool subsets
are rejected until their policy is certified for the pinned upstream version.
API keys, gateway credentials, and workflow secrets are never written to the
SQLite snapshot. Native provider authentication stores remain provider-owned.

`command`, `args`, `workspace`, and `model` remain available for CLI adapters.
Agent-level relative workspaces resolve against the configuration directory;
`with.workspace` resolves against the workflow workspace and overrides the role.
`instructions`, `tools`, and `authentication` management apply to session
backends; legacy CLI configuration continues to be provider-owned.

### Containers

Session backends can select:

```yaml
execution:
  mode: container
  runtime: podman             # docker also supported
  image: registry/image@sha256:REPLACE_WITH_DIGEST
  writable: false             # explicitly enable for writers
```

The same policy can be configured at top level as `execution:` for command
steps and roles that omit an execution policy. A role with an explicit execution
mode overrides that default. `cleanup: strict` applies to all inherited roles;
on macOS it requires containers. Native macOS execution provides process-group
cleanup. Native Linux execution additionally supervises detached descendants.

Container images must carry guardian protocol label
`agent-workflow.guardian-protocol=1` and include
`/usr/local/bin/agent-workflow-guardian`. SDK images also carry matching SDK and
bridge-protocol labels and include the Linux bridge/dependencies in the image;
host macOS `node_modules` are never mounted into a Linux VM. Explicit positive
`uid`/`gid` settings can select a non-root mapping; Podman defaults to keep-id.
PID 1 verifies readable/writable mounts and UID access before workload effects.
Image distribution and Docker/Podman host certification remain release gates.

Images must already exist locally and be pinned by digest. No image is pulled
implicitly. Containers receive a read-only root filesystem, bounded resources,
a private temporary filesystem, the selected independent repository, private
provider session state, the SDK bridge if needed, and explicit provider auth.
Host homes, SSH agents, GitHub environment credentials, and Docker sockets are
not mounted. A host lifetime guardian verifies ownership labels before removing the
container. A non-root Linux PID 1 also monitors a private controller heartbeat;
it requires a changing heartbeat before effects and stops work when ownership
ends, including when the host runtime is unavailable. Native Codex container authentication requires an explicit
`authentication.credential-file` referencing its provider-owned `auth.json`.

A host worktree's shared `.git` administration is rejected. Use
`workspace/create` with `independent: true` for container writers. Reviewer
snapshots are independent repositories; configure their container roles with
`writable: false` to enforce read-only access.

## Durable execution and recovery

SQLite at `.agent-workflow/state.db` is authoritative, using WAL and FULL
synchronous durability. The engine records the workflow closure, definition
hashes, resolved roles, nonsecret inputs, Git baseline when needed, and secret
references before admission. It records attempts before effects and completion
before dependents. A workspace lock prevents competing controllers, and
conditional run claims prevent competing resume requests.

```sh
agent-workflow status RUN_ID
agent-workflow list
agent-workflow logs --follow RUN_ID
agent-workflow attempts RUN_ID
agent-workflow cancel RUN_ID
agent-workflow resume --retry-uncertain RUN_ID
agent-workflow resume --request-id recovery-1 --retry-uncertain --detach RUN_ID
agent-workflow export RUN_ID
agent-workflow export --artifact ARTIFACT_ID RUN_ID > stdout.txt
agent-workflow stop
agent-workflow cleanup RUN_ID
```

`status`, `list`, `logs`, and artifact inspection work without the controller.
Each supervised invocation is journaled with its originating generation and
logical/physical identity. Containers are created inert, their identity is
committed, then attached-started with stdin/stdout intact. Startup and background
reconciliation inspect exact runtime identity and ownership labels. Failed
inspection never proves absence. Unresolved cleanup blocks conflicting admission
while cancellation and inspection remain available:

```sh
agent-workflow invocations RUN_ID
```

`GET /runs/RUN_ID/invocations` returns pending records; `?all=true` includes
completed records. Native guardians leave durable completion acknowledgements;
cleanup never signals a stored host PID. A lost create response does not start
the container automatically. Runtime recovery retries cleanup, preserving the
original attempt and uncertainty history.

Resume uses the saved definitions rather than current YAML. Completed steps and
successful jobs are retained. A stopped owner leaves active runs
`needs_attention`; no effect is replayed automatically. Inspect the workspace and
attempts before using `--retry-uncertain`, which explicitly permits uncertain failed or
interrupted effects to run again. A checkpointed completed task awaiting schema
repair is resumed as formatting only and does not require replaying its actions.
Control reusable workflows through their
originating run. Their execution capacity and workspace ownership are shared.

Resume is durably queued before its `202` acknowledgement and uses the same
execution owner as a new run. Status includes `execution_generation` and, on
failure, `failure_class`. Optional `--request-id` deduplicates resume within one
run; reusing it with a different retry acknowledgement is rejected. Each accepted
generation retains its own acknowledgement and cancellation history. Missing
resume credentials or failed preflight become inspectable `needs_attention`
outcomes. Workers are closed and joined before terminal status is published.

CLI `list`, `worker --operation list`, and `worker --operation messages` return
compact `{items, next_cursor}` pages. Use `--limit` (default 100, maximum 1,000)
and pass `next_cursor` back with `--cursor`. Individual status/result commands
retain full results. HTTP list endpoints opt into pagination with `limit` or
`cursor`; calls without these parameters and existing Go list methods retain
their legacy array shapes.

Existing schema-1 through schema-8 databases upgrade transactionally when the controller (or a
foreground execution/cleanup command) holds the workspace lock. The upgrade
retains `.agent-workflow/state.pre-v7.db`, a consistent database-only backup
including committed WAL records. Verify upgraded inspection and deterministic
execution before manually removing this backup. Inspection opens storage read
only and asks for an explicit controller upgrade when the schema is older;
newer unsupported schemas are rejected. Stop the owner before restoring any
database. A database backup alone does not include managed repositories or
provider session state and cannot guarantee session recovery.

Git commits and publication have additional durable intents and reconciliation.
A lost PR-create response is reconciled against the exact head, base, and commit.
If the create result remains uncertain and no matching PR can be found, creation
stops for manual attention rather than issuing another create request.

Legacy `.agent-workflow/runs/*.json` records remain readable through `status`;
they cannot resume. `FileStore` remains available to library callers for JSON
exports/checkpoints, without durable replay semantics.

Use `--secret-env NAME=ENV_VARIABLE` for workflow secrets. A background controller
must have those variables in its own environment. Resume requires credentials
to be available again. Known secrets are redacted in persisted results and
artifacts. Admission rejects credential literals in definitions or nonsecret
inputs instead of modifying their replay semantics. State files and sockets use
0600; newly created state directories use 0700.

## Retention and recovery backups

`cleanup RUN_ID` records removal intent/progress before removing owned Git
workspaces, clones, provider state, invocation files and database records.
Interrupted cleanup is resumable by repeating the command. `cleanup-status RUN_ID`
inspects its journal, including the completion tombstone after deletion.
Retiring runs cannot resume. Cleanup requires stopped execution and resolved
invocation cleanup, holds exclusive ownership, and leaves caller files, other
origins and provider-owned native authentication untouched. Ownership-path
symlinks are rejected; native authentication links inside removed provider state
are removed without following them.

Stop the controller before database maintenance:

```sh
agent-workflow stop
agent-workflow backup --workspace . /private/backups/state.db
agent-workflow bundle --workspace . /private/backups/recovery.tar.gz
agent-workflow restore --workspace . /private/backups/state.db
```

Backup captures committed WAL records consistently. Restore validates integrity
and supported schema, preserves a pre-restore database, and installs atomically.
Runnable/failed/canceled restored records are quarantined as `needs_attention`;
effects may have occurred after the snapshot. Inspect the external state and
use an explicit uncertain-retry acknowledgement. Restored mailboxes are
quarantined and pending entries are retained as interrupted, requiring a new
explicit send rather than replay.

Database-only backup preserves records. Session recovery also requires matching
managed workspaces and engine-owned provider state. Recovery bundles contain
those engine-owned paths and exclude native credential files and references.
They are private sensitive archives, not publication artifacts. Restore files at
matching managed paths and restore their database through the command; caller
Git administration and native provider credential stores remain separate.
MCP exposes `controller_stop`, `database_backup`, `database_bundle` and
`database_restore`; maintenance still requires the controller to have stopped.

## Workflow syntax

Existing manual/workflow-call inputs, reusable jobs, `needs`, `if`, sequential
steps, outputs, `continue-on-error`, and structured output contracts continue to
work. YAML parsing rejects duplicate keys, aliases, unknown fields, and cycles.
The small expression grammar supports paths, literals, comparisons, `not`, and
`and`; it does not add general scripting, parentheses, OR, or expression functions.

### Commands and bounded iterations

```yaml
jobs:
  check:
    repeat:
      max-attempts: 3
      until: ${{ steps.check.outputs.exit_code == 0 }}
    steps:
      - id: check
        argv: [go, test, ./...]
        timeout: 5m
        continue-on-error: true
```

`argv` executes directly. Shell execution requires both `run` and an explicit
`shell`, for example `shell: sh`. `working-directory` and positive-duration
`timeout` are supported. Outputs include `exit_code`, `stdout`, and `stderr`;
stdout/stderr are also durable artifacts. Each stream is limited to 16 MiB.
Arguments must render to strings; use interpolation such as
`'attempt=${{ iteration.number }}'` when including a number in an argument.

`iteration.number` starts at one; `iteration.previous` starts as an empty object
and then contains the previous status, outputs, and steps. Repeat conditions can
also inspect `iteration.outputs` and `iteration.status`. Reusable workflow jobs
can repeat. The engine retains all attempt history and fails on exhaustion.
`max-attempts` must be 1–100; the development example uses three fix cycles.

Reusable jobs also support ordered dynamic iteration:

```yaml
jobs:
  reviewers:
    uses: workflows/reviewer.yml
    for-each:
      items: ${{ inputs.reviewers }}
      key: item.id
    with:
      reviewer: ${{ item }}
      ordinal: ${{ iteration.number }}
      identity: ${{ iteration.key }}
```

`items` is a literal array or a whole expression returning an array. `key` is
an expression returning a nonempty unique string for each item. All identities
and call inputs are validated and pinned before starting any child. Literal item
payloads remain data, including strings containing expression delimiters.
`for-each` requires `uses` and cannot combine with `repeat`. YAML and Go are
supported; the browser preserves and validates YAML without guided iteration
controls. Each iteration job runs its children sequentially without occupying
an execution slot while waiting for child jobs.

The job exposes `outputs.items` in configured order and `outputs.complete`.
Each item retains its key, one-based number, physical attempt and child ID,
outputs, child jobs, failure class and reason. Outcomes are `completed`,
`exhausted`, `provider_failure`, `interrupted`, and `pending`. Completion requires
at least one completed item, no unresolved provider failure, and no pending or
interrupted item. Empty and all-exhausted lists have `complete: false`;
exhaustion remains an explicit skip. A classified provider failure fails the job
while permitting later items; a caller can inspect the aggregate with a dependent
job condition. `continue-on-error` never hides integrity, ownership, cleanup,
instruction, checkout or storage failures.

Adapters classify recoverable provider errors with `workflows.ErrProviderFailure`
or `workflows.ErrUsageExhausted` (wrapped with a reason). Arbitrary errors are
blocking failures. Registered actions supply binding and evidence guards; use
those guards before provider work. The engine revalidates retained action
bindings and evidence before each item and provider dispatch, and before reuse.
It never infers evidence validity from provider text.

Explicit resume reuses validated completed items and creates fresh physical
children for exhausted, failed or interrupted items. The normal
`--retry-uncertain` acknowledgement still applies. Successful runs containing
exhausted skips can be reopened. Prior attempts and completed results remain
immutable. Changed ordered lists, payloads, resolved inputs, definitions or action
contracts require a new run; definitions are always taken from the admitted
snapshot. Dependent aggregates are invalidated on retry. Read-only aggregation
can refresh; changed inputs to completed mutations or commands block replay.
Within item retries, registered mutations retain a stable logical operation
identity: validated completed effects are reused, and uncertain effects require
reconciliation proof of completion or no effect before proceeding.

The engine advertises `ordered-dynamic-iteration-v1` through `Capabilities()` and
controller health. Embedders must supply the optional `IterationJournal`
extension (implemented by SQLite schema 9). It pins plans, appends item admission
and terminal observations, binds ordinary step inputs through `BeginInput`, and
fences every write against the originating execution owner. A committed child
completion recovers a lost parent/item checkpoint without repeating its work.
Schema upgrades use the existing exclusive lock and private pre-upgrade backup.
Legacy JSON stores cannot execute iteration workflows.

Structured output uses native schemas on session backends and strict-object
contracts on current CLI backends, followed by engine validation.
`output.repair-attempts: 0` disables repair. The older
`repair_attempts` and `repairAttempts` spellings retain their legacy defaults.
Verified tool-free repair is currently available only through `claude-sdk`;
other backends fail validation without repeating the implementation task.
Completed task output and repair admission/results are checkpointed separately.
Explicit resume can retry a bounded formatting phase using cached task output,
including after a lost terminal checkpoint, without running implementation again.
Attempt records retain `task_attempt_id`, original outputs, and repair history.
Reported `cost_usd` is included when the provider supplies it; native Codex
subscription usage has no dollar figure. Deduplicate cached task usage by
`task_attempt_id` when aggregating resumed attempt records.

### Development and publication

[develop.yml](workflows/develop.yml) runs planning, bounded implementation/test/
review cycles through [develop-iteration.yml](workflows/develop-iteration.yml),
final checks and two final reviews, and GitHub publication. It uses separate `planner`, `builder`, `reviewer_claude`, and `reviewer_codex`
roles; choose their backends explicitly in your configuration. The example
configuration gives the builder write access and the reviewers read-only/plan
permissions. Managed SDK/container policies are available when stronger isolation
is required.

Enable publication only for a deliberate allowlisted repository:

```yaml
publication:
  repository: owner/disposable-repository
  base: main
  remote: origin
  checks: [test]
  reviewers: [review_one, review_two]
```

Controller actions are `workspace/create`, `workspace/snapshot`,
`workspace/integrate`, `git/commit`, and `github/pr`. `workspace/create` defaults
to a detached worktree at the admitted baseline; `independent: true` creates a
separate clone. `workspace/snapshot` creates an independent fixed-commit clone.
Integration cherry-picks completed writer commits serially and retains conflicts.
Commits reconcile a durable action footer after uncertain completion. Caller
worktree changes and branch position are preserved.

Publication requires a clean managed workspace at the supplied commit, the exact
configured GitHub remote, every configured check reporting `checked_commit`, and
at least two distinct review jobs reporting `clean: true` and `reviewed_commit`
for that commit. Workflow definitions are trusted to report those results
honestly. The publisher creates a run-specific branch, pushes without force,
reconciles one PR per run, and never merges. A push-negotiation guard rejects unexpected advertised remote changes and preserves existing pre-push hooks. Unexpected remote changes stop it.

## MCP and worker controls

```sh
# Codex registration:
codex mcp add agent-workflow -- /absolute/path/agent-workflow mcp --workspace /repo
# Claude registration:
claude mcp add --transport stdio agent-workflow -- /absolute/path/agent-workflow mcp --workspace /repo
```

The stdio server uses the official Go MCP SDK and starts the workspace
controller if necessary. Protocol output stays on stdout; diagnostics go to
stderr. Tools expose workflow discovery/start, status/events/cancel/resume,
artifact listing/retrieval, and worker spawn/send/status/wait/interrupt/close.
Starts and sends return durable handles. Waits are bounded to 20 seconds and
can be polled. MCP disconnection does not cancel admitted work.

Workers have stable IDs, parent links, provider session IDs, ordered durable
mailboxes, and deduplicated message request IDs. Default limits are 64 workers,
depth eight, 64 queued messages per worker, 32 turns per root worker, 30 minutes
per turn, and 16 MiB output. Child grants are reserved from the parent's remaining
budget and tools must be a subset of the parent's policy. Interrupted owner-loss
turns are retained without automatic replay; worker recovery requires an explicit
acknowledgement. Run cancellation closes all its workers.

```sh
agent-workflow worker --operation spawn --run RUN_ID --role reviewer --turns 8
agent-workflow worker --operation send --run RUN_ID --worker WORKER_ID \
  --request-id message-1 --message 'Review the fixed snapshot'
agent-workflow worker --operation messages --run RUN_ID --worker WORKER_ID
agent-workflow worker --operation interrupt --run RUN_ID --worker WORKER_ID
agent-workflow worker --operation close --run RUN_ID --worker WORKER_ID
agent-workflow worker --operation resume --retry-uncertain --run RUN_ID --worker WORKER_ID
```

External MCP facade waits do not use execution capacity. Claude SDK roles can
also use controller-brokered worker tools and explicitly configured external MCP
servers. Provider-supplied arguments cannot choose another run or parent. Child
native tools, MCP grants, workspace, execution boundary, and runtime limits
remain within role and parent policy.

Configure servers and exact grants explicitly:

```yaml
mcp-servers:
  lookup:
    transport: streamable-http
    endpoint: https://tools.example.invalid/mcp
    timeout: 30s
    headers-env:
      Authorization: WORKFLOW_LOOKUP_AUTH

# Add these fields to a claude-sdk role:
# mcp-grants:
#   - {server: engine, tool: worker_spawn}
#   - {server: engine, tool: worker_send}
#   - {server: engine, tool: worker_status}
#   - {server: engine, tool: worker_wait}
#   - {server: engine, tool: worker_interrupt}
#   - {server: engine, tool: worker_close}
#   - {server: lookup, tool: search}
```

Stdio servers use `transport: stdio`, `command`, explicit `args`, and optional
`environment-env` mappings from server variable names to controller credential
references. HTTP `headers-env` works the same way. These credentials stay in the
broker/server environment. Omitted grants authorize no tools; wildcards are
rejected. Tool descriptions, object schemas (2020-12 or unspecified dialect),
and catalog fingerprints are pinned at admission and checked on resume.

The broker journals calls before dispatch, caches identical completed calls,
rejects conflicting call identities, and reserves child grants once. Lost
external responses remain uncertain and are never replayed automatically.
Inspect them with `agent-workflow tool-calls RUN_ID` or
`GET /runs/RUN_ID/tool-calls`; `?all=true` includes completed and denied calls.
An uncertain call prevents terminal success. Explicit uncertain retry admits a
new attempt without erasing the earlier call or claiming its effect succeeded.

Capacity handoff is invocation-wide: the SDK must report the model stream stopped
and every active native tool stopped or waiting for controller permission before
its lease can suspend. Results reacquire capacity before provider work resumes.
Native tool hooks also obtain capacity before effects. Suspended invocations are
bounded at 128 per controller. Deterministic tests cover workflow → child →
grandchild execution at capacity one and actual pinned-SDK multi-tool callbacks.
Codex's managed-policy and quiescence capabilities remain an explicit release
gate; another backend is never substituted automatically.

## Usage accounting

`agent-workflow usage RUN_ID`, `GET /runs/RUN_ID/usage`, and MCP `run_usage`
return normalized totals plus provider/model and invocation detail. Status adds
`normalized_usage`; existing raw `usage` and `cost_usd` outputs are unchanged.
Input totals include Claude cache reads/creation. Cached input remains a separate
reported subset. Provider thread counters are recorded as observations/deltas,
not repeatedly added snapshots. Cached task checkpoints retain their originating
identity; formatting repairs and worker turns count separately.

Unknown dollar cost is `null`, with `known_cost_usd` showing only reported
amounts. Lost final observations retain partial tokens and `complete: false`.
Provider-reported usage from failed calls is retained. Schema upgrades backfill
available historical observations without changing snapshots or attempts.
No pricing lookups, billing estimates, or spending-budget enforcement are added.

## Operations and development

`/health` remains liveness/compatibility metadata. `/ready` checks storage writes,
scheduler heartbeat, cleanup prerequisites, private state permissions, and state
and temporary filesystem capacity independently. Unavailable prerequisites return
explicit admission errors; cancellation and inspection remain usable.
`/metrics` serves queue depth/age, occupied/suspended slots, worker/run states,
failures, cleanup backlog, and storage errors over the existing private socket.
Metric labels contain no run IDs or prompts. Controller JSON logs rotate at
10 MiB with three retained files.

```yaml
controller:
  max-queued: 128
  max-active-runs: 4
  run-timeout: 30m
  min-free-bytes: 67108864
  min-free-inodes: 128
  # temporary-directory: /private/workflow-tmp
```

The byte/inode defaults apply independently to both filesystems. An explicit zero
disables that threshold. Doctor reports effective cleanup guarantees, dependency
versions, capacity and state permission checks, and unresolved invocations.
Protocol tests and production certification are reported separately.

`init` creates the initial role configuration and embedded example workflows
without overwriting existing files.
`doctor` checks versions/capabilities, cached native login status, gateway model
endpoint reachability/authentication, container runtime/image availability, the
Git baseline, and state write permission. A cached native login can still be
rejected by the model service; an actual live probe requires a separate budget.
Provider preflight rejects unavailable executable/credential references before
admission. `cleanup` removes terminal originating-run records and their reusable
children, artifacts/mailboxes/intents, registered worktrees, and independent
workspace clones. It preserves caller files and refuses active/uncertain groups.
Cleanup is refused while a controller owns the workspace.

```sh
make test       # deterministic race tests, no model calls or credentials
make check      # vet and workflow validation
make build
agent-workflow bridge setup --directory bridge/claude
node --check bridge/claude/bridge.mjs
# Actual pinned SDK against a local fake API; no upstream model calls:
AGENT_WORKFLOW_SDK_TEST=1 go test ./pkg/clirunner -run TestSDKBridgeAgainstFakeGateway
# Optional Linux container conformance using an already available pinned image:
AGENT_WORKFLOW_CONTAINER_IMAGE=sha256:IMAGE_ID go test ./pkg/clirunner -run TestContainer
```

Pull request CI runs native race tests and Docker/Podman conformance on Linux
AMD64, plus workflow validation, isolated filesystem checks, and the complete UI
browser suite. Merge queue (`merge_group: checks_requested`) CI additionally runs
Linux ARM64 and both macOS architectures. Pushes to `main` and reusable release
checks also run the full platform matrix. The required `Required` aggregate
fails unless every job selected for that event succeeds.

Enable **Require merge queue** for `main` when adopting this workflow, retaining
**Required** as the required status check. The queue must validate the full matrix
before merging; ordinary PR success covers only the Linux AMD64 feedback path.

Live tests and publication pilots are separate from deterministic tests and
require explicit budgets and a disposable repository.
Shared hosting, Windows runtime support, cron/webhook dispatch, native-definition
import, automatic conflict resolution, and automatic merge remain outside scope.

The original schema/expression/output foundations came from PicoClaw; see
[NOTICE](NOTICE) and [LICENSE](LICENSE). Runtime bindings and orchestration are
standalone Go implementations.

Stable release preparation, signed evidence, verification and remaining certification
gates: [release operations](docs/RELEASE_PIPELINE.md).

The independent [workflow viewer and editor](ui/README.md) provides React components,
plain JavaScript mounts, and a standalone browser bundle for authoring workflow YAML.
Run `make ui-install ui-check ui-test ui-browser-test` for deterministic UI validation,
or `npm run dev --prefix ui` to explore the repository examples locally.
