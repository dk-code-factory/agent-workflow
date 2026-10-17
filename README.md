# Agent Workflow

A Go controller for single-user Claude/Codex workflows on Linux and macOS. It
supports durable workflow admission, guarded recovery, command steps, bounded
iterations, worker mailboxes, isolated Git workspaces, and GitHub publication.

**Release status: implementation candidate, not yet production certified.** See
[release evidence and outstanding gates](docs/RELEASE.md). The deterministic CLI
and MCP publication pilots pass. Codex app-server has passed a native-auth live
probe; live Claude authentication and the gateway gate remain unverified.

## Build and run

Requires Go 1.25+. Install only the providers selected by your roles. The SDK
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
native skills, plugins, agent discovery, and MCP servers; provider
managed restrictions remain applicable. Native delegation and discovery tools
are rejected. SDK repair calls receive no tools and no original task prompt.
The bridge assigns its own session title and disables auto-memory/consolidation;
provider-native structured-output attempts are limited to one, leaving repair
under the engine's durable tool-free phase.

For Codex direct credentials, select `authentication.mode: api-key` and provide
`credential-env`. The app-server authenticates through its native login API and
uses engine-owned provider state. Custom Codex endpoints are not implemented.
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

Images must already exist locally and be pinned by digest. No image is pulled
implicitly. Containers receive a read-only root filesystem, bounded resources,
a private temporary filesystem, the selected independent repository, private
provider session state, the SDK bridge if needed, and explicit provider auth.
Host homes, SSH agents, GitHub environment credentials, and Docker sockets are
not mounted. A separate lifetime guardian removes the container if its owner
exits. Native Codex container authentication requires an explicit
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
agent-workflow export RUN_ID
agent-workflow export --artifact ARTIFACT_ID RUN_ID > stdout.txt
agent-workflow stop
agent-workflow cleanup RUN_ID
```

`status`, `list`, `logs`, and artifact inspection work without the controller.
Resume uses the saved definitions rather than current YAML. Completed steps and
successful jobs are retained. A stopped owner leaves active runs
`needs_attention`; no effect is replayed automatically. Inspect the workspace and
attempts before using `--retry-uncertain`, which explicitly permits uncertain failed or
interrupted effects to run again. A checkpointed completed task awaiting schema
repair is resumed as formatting only and does not require replaying its actions.
Control reusable workflows through their
originating run. Their execution capacity and workspace ownership are shared.

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

External MCP waits do not use execution capacity. Provider-internal nested
worker tools and capacity release while a provider turn waits for its own child
are not yet integrated; see the outstanding release gates.

## Operations and development

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
npm ci --prefix bridge/claude
node --check bridge/claude/bridge.mjs
# Actual pinned SDK against a local fake API; no upstream model calls:
AGENT_WORKFLOW_SDK_TEST=1 go test ./pkg/clirunner -run TestSDKBridgeAgainstFakeGateway
# Optional Linux container conformance using an already available pinned image:
AGENT_WORKFLOW_CONTAINER_IMAGE=sha256:IMAGE_ID go test ./pkg/clirunner -run TestContainer
```

CI runs race tests on Linux/macOS. Live tests and publication pilots are separate
from deterministic tests and require explicit budgets and a disposable repository.
Shared hosting, Windows runtime support, cron/webhook dispatch, native-definition
import, automatic conflict resolution, and automatic merge remain outside scope.

The original schema/expression/output foundations came from PicoClaw; see
[NOTICE](NOTICE) and [LICENSE](LICENSE). Runtime bindings and orchestration are
standalone Go implementations.
