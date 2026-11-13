# Portable PR maintenance example

The [copyable example](../examples/pr-fix/README.md) implements PR maintenance
using Python helpers and ordinary YAML workflows with the stock `agent-workflow`
binary. PR-specific capture, report validation, retained allowances, Git policy,
CI classification and publication receipts belong to the example. The controller
owns native agent sessions, scheduling, admitted assets and recovery.

Preparation compiles a complete, inspectable workflow closure before admission.
The generated YAML shows task barriers, review/fix repetition, same-session
checklist follow-ups, finalizers, publication audience checks and the final
completion gate. Python helpers perform one boundary operation per invocation;
they do not launch providers or run an orchestration loop.

## Setup and preparation

Use an independent Git clone with a real `.git` directory, checked out at the PR
head with the target commit available locally. Container workspace mounts reject
linked worktrees whose `.git` is a file. Commit the copied example definitions
before preparation, and start with a clean worktree. Preserve the exact original
request and explicitly stage effective settings in the request JSON.

Install the stock binary, Docker and the configured Codex app-server runtime.
The helper image must already be available locally under a digest-pinned
reference or immutable local content ID and contain Python 3.11 or newer, Git,
`gh` and `sleep`. Python helpers
use only the standard library. Any compatible image can be used; no PR-specific
Dockerfile or custom Go binary is required. Use the same image digest for
preparation and `request.image`.

For a generic local tool image, the operator can build these standard tools
without adding a PR-specific Dockerfile to the example:

```sh
docker build -t workflow-tools - <<'DOCKERFILE'
FROM python:3.13-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends git gh coreutils \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE
TOOL_IMAGE=$(docker image inspect --format '{{.Id}}' workflow-tools)
docker run --rm --pull=never "$TOOL_IMAGE" python3 --version
docker run --rm --pull=never "$TOOL_IMAGE" git --version
docker run --rm --pull=never "$TOOL_IMAGE" gh --version
```

Set `request.image` to that exact `sha256:...` value. The base tag chooses build
inputs; the resulting content ID pins the actual admitted image. A compatible
registry `IMAGE@sha256:...` reference is also supported. The zero digest in the
sample request is a template placeholder, not an available tool image.

Run preparation and the controller as the same non-root workspace owner. The
preparation container's UID/GID must match the container execution identity so
private files remain readable by the admitted helpers. Provider authentication
belongs to the configured host runtime. GitHub credentials enter helper processes
through the declared `github_token` secret, never through plan JSON. The transport
selects the canonical PR host and maps `GH_TOKEN` to `GH_ENTERPRISE_TOKEN` for
enterprise requests when necessary. Preparation never refreshes login.

Copy [request.example.json](../examples/pr-fix/request.example.json) to a private
request file, replacing the sample PR, exact instruction and image digest. The
sample reviewer adapter and all five native definitions are explicit. Remote
permissions start disabled and `authorizations` is empty. Keep this private file
out of version control; for the documented path, add a local Git exclusion:

```sh
printf '\n/.pr-fix-state/\n/workflows/.pr-fix-portable/\n/examples/pr-fix/request.json\n' >> .git/info/exclude
cp examples/pr-fix/request.example.json examples/pr-fix/request.json
```

After editing it, prepare with the selected image. `TOOL_IMAGE` and the request's
`image` field must name the same real digest. `GH_TOKEN` references the credential
already provided by the operator.

```sh
docker run --rm --pull=never --network bridge \
  --user "$(id -u):$(id -g)" -e GH_TOKEN \
  -v "$PWD:/workspace" \
  -v "$PWD/examples/pr-fix/workflows/helpers:/actions/helpers:ro" \
  -w /workspace "$TOOL_IMAGE" \
  python3 -B /actions/helpers/action.py prepare examples/pr-fix/request.json
```

Preparation makes GitHub reads and writes application-owned local artifacts and
definitions. It does not start an agent or publish. It returns `logical_run`,
`plan`, `workflow`, `config`, `request_id` and `inputs`. Review the returned plan,
manifest, native definitions and YAML before running it. Definitions, helper
bytes, image digest, original instructions, runtime settings and task selections
participate in the prepared identities. A changed source creates a new closure;
preparation rejects changed bytes in an existing closure. Generated files contain
private assignments and belong outside version control.

## Stock CLI execution and recovery

Use the exact returned values in place of `CONFIG`, `PLAN`, `REQUEST_ID` and
`WORKFLOW`. Flags precede the workflow reference. The credential flag is
`--secret-env`; its value names an environment variable rather than a token.

```sh
agent-workflow validate --workspace . --config CONFIG WORKFLOW
agent-workflow run --workspace . --config CONFIG \
  --secret-env github_token=GH_TOKEN --input plan=PLAN \
  --request-id REQUEST_ID --detach WORKFLOW
agent-workflow status --workspace . RUN_ID
agent-workflow attempts --workspace . RUN_ID
agent-workflow logs --workspace . --follow RUN_ID
```

An existing workspace controller retains its admitted runtime configuration.
After its owned work has stopped, restart it when switching to a different
generated configuration; passing a new `--config` to a running controller does
not replace its role settings. Resume unfinished original work before replacing
its runtime.

The generated runtime uses local `codex-app-server` agent execution and
container-backed Python helpers. The sample sets `runtime.sandbox` to
`workspace-write` so native writing tasks can edit their assigned source files.
An omitted sandbox defaults to read-only and cannot run the writing tasks;
helper-container write access does not change native agent permissions.
Helper containers use explicit bridge networking;
the engine still pins their immutable image and retained helper bundle. Native
agent waits use the controller's durable mailbox mechanism and release execution
capacity. Containers finish their operation before YAML dispatches an agent.

A client disconnect or wait timeout leaves admitted controller work retained.
Inspect the run before requesting a resume. Resume the original run after its
owned work has stopped; keep its configuration and credentials available:

```sh
agent-workflow resume --workspace . --config CONFIG \
  --secret-env github_token=GH_TOKEN --request-id RESUME_REQUEST_ID RUN_ID
```

Normal engine recovery reuses the original native session, admitted definition,
turn allowance and helper bundle. Same-session checklist continuation is explicit
in `continue.yml`, with at most three follow-ups. An unfinished application claim
cannot be dispatched by a replacement execution merely because its process has
changed. Resume its originating run or inspect and reconcile the retained effect;
unknown publication effects never grant automatic replay. `--retry-uncertain`
does not override structured mutation reconciliation.

For changed instructions, settings or role definitions, prepare a new closure and
start a replacement only after the original execution has stopped and uncertain
effects have been settled. Keep the returned `logical_run` for the same PR. Its
application ledger preserves consumed allowances and absolute deadlines across
replacement engine runs. Changing configuration does not replenish them. Explicit
user-authorized continuation records a stable request, exact instruction and
source without erasing earlier consumption.

To record an explicitly authorized allowance extension, place this object in
`.pr-fix-state/continue.json`, replacing `PLAN` and the authorization with the
actual retained user instruction:

```json
{
  "plan": "PLAN",
  "name": "review_cycles",
  "amount": 2,
  "authorization": {
    "request_id": "stable-user-continuation-id",
    "instruction": "Exact user instruction granting two additional review cycles",
    "source": "chat:original-continuation-message"
  }
}
```

```sh
docker run --rm --pull=never --network none \
  --user "$(id -u):$(id -g)" \
  -v "$PWD:/workspace" \
  -v "$PWD/examples/pr-fix/workflows/helpers:/actions/helpers:ro" \
  -w /workspace "$TOOL_IMAGE" \
  python3 -B /actions/helpers/action.py continue .pr-fix-state/continue.json
```

This command records only the deduplicated grant. It starts no workflow or
provider and does not rewrite an admitted graph or renew its structural bounds.
Ordinary resume/replacement still requires settled ownership and effects.

## Reports, review reuse and controlled commits

A leaf receives one typed assignment, its original instructions, relevant settings,
source snapshot, exact checklist and original findings. Its structured JSON report
contains `complete`, `checklist`, `findings`, `evidence` and `blockers`. Evidence is
an array of `{id,path,sha256}` entries naming current regular workspace files.
Checklist evidence and finding sources name these evidence IDs. Each assigned
checklist item must appear exactly once, with `completed` or `incomplete` status;
completed items require evidence. A true `complete` flag alone grants no coverage.

Review fingerprints include the current tree and diff, target/base trees,
discussion, instructions, settings, relevant resources, task controls and native
definition/helper identities. Content reviews can survive history-only changes;
history-sensitive and head-sensitive tasks refresh their respective evidence.
Changed code, incoming discussion or governing inputs invalidates affected reuse.
An independent native verifier accounts for every original finding before fixes.
Finding IDs remain claim-scoped, and checklist continuation preserves original
findings. Accepted/deferred findings need the exact retained user authorization.
Completion uses the latest verification and fix disposition for each finding.
Retries and checklist dispatches refresh governing evidence, including when their
preparation checkpoints are reused. The original fenced writer can keep its
unfinished source edits while instructions, remote evidence and ownership remain
unchanged.

Writing leaves report every changed file as `{path,before,after}` with SHA-256
byte hashes and `null` for absence, plus a repository-native `commit_message`.
The helper checks the manifest, original parent, clean initial index, staged paths
and resulting commit before retaining a controlled commit receipt. Native leaves
do not commit or publish themselves. Unrelated files are never staged implicitly.
The resulting unpublished head still requires explicit push authority and an
independent audience assessment.

The configured limits bound review cycles, eligible task retries, CI repairs and
per-head reruns. Reservations are transactional and idempotent; response loss
cannot consume the same operation twice. CI observation returns one exact-head
inventory at a time, while YAML owns waiting and repair repetition. Absolute CI
deadlines survive process and head changes. Pending checks and empty inventories
cannot establish success. Flake candidates require an open repository issue that
affirms the exact workflow/job/test/signature; saved exceptions require the
original scoped rule. Both verify the exact run, job, attempt and complete failed
step evidence before exemption. Unknown failures remain blocking.

## Publication and final readiness

Selections and permissions never grant user authority. A remote operation needs
both its configured permission and a retained authorization object:

```json
{
  "request_id": "stable-user-request-id",
  "instruction": "Exact user instruction authorizing this operation",
  "source": "chat:original-message-id"
}
```

Put it under its exact operation kind in `authorizations`; `iterate` can cover
ordinary authorized maintenance operations. Merge always needs its own `merge`
entry. Maintaining another author's PR requires `maintain_other` authority.
Never create authorization text on the user's behalf.

Publication retains the exact PR, numeric actor, head/target/branch leases,
payload, sources and authorization before dispatch. A separate native audience
assessment must accept that exact fingerprint. The publisher checks current
source coverage and permissions, then records the intent before the external
write. Reconciliation is read-only; a separate mutation stage records an observed
receipt. Lost creation responses remain unknown when absence is not authoritative.
Missing replies or PRs alone do not prove that another creation is safe. Inline
resolution requires the retained actual answer in the exact original thread.
CI reruns bind the failed run/head/attempt and retained per-head allowance.

`complete` means selected application work has current accepted coverage and
original findings are settled. `ready` additionally requires a clean published
head, current independent review/verification, fresh exact-head successful CI and
live repository merge requirements. Empty task/finalizer selections still run
this live completion gate and cannot establish merge readiness without the
required review evidence. A separately authorized merge is proposed only after
readiness, receives its own audience assessment and repeats the live gates just
before dispatch. The supported merge receipts bind merge/squash to the exact
original head, target and method-specific commit parents.

## State, backup and compatibility

Application state is `.pr-fix-state`, with an ownership marker, per-logical-run
SQLite ledgers and immutable hash-addressed artifacts. Prepared closures live
under `workflows/.pr-fix-portable/LOGICAL_RUN/FINGERPRINT`, also under an owned
namespace. They contain static YAML, copied native definitions, read-only helper
sources, runtime configuration and a byte manifest. Application state is outside
`/actions` and separate from the engine's hidden `.agent-workflow` directory.
Never put unrelated files in these application namespaces. Preparation rejects
unmarked namespaces and refuses to hide Git-tracked source there.

Back up application state and prepared closures together with the engine's owned
backup/recovery bundle and referenced helper assets. A database-only copy omits
executable helper bytes. Stop active owners before copying writable state. Keep
uncertain intents, original reports, receipts and sessions until reconciliation
and retention requirements are satisfied. Cleanup must be explicitly scoped to
these verified application-owned paths; it must not remove unrelated workspace
files or another run's assets.

This portable contract deliberately changes the former Go application's setup:

- Use stock `run`/`resume`; the PR-specific CLI commands, Go registrations and
  runner wrapper are removed from production. Historical Go code and fixtures
  remain deterministic comparison evidence, not the portable entrypoint.
- Supply explicit JSON `options`, `settings`, native role paths and adapters.
  Implicit host XDG/plugin settings, arbitrary installed skills, automatic
  upstream discovery and legacy process hooks are not imported. Stage reviewed
  effective values explicitly. Unsupported selections fail preparation.
- Custom adapters describe one native task using the declared role names,
  assignment, checklist, resource paths and sensitivity flags. Parallel groups
  may contain independent readers; writers sharing the Git index run in order.
  Arbitrary provider/session controls and shell orchestration are not adapters.
- Managed publication and CI selections accept only the skill name and empty
  default controls. A managed reviewer selection additionally accepts
  `options.reviewers`. Put task instructions and optionality on explicit adapters;
  unsupported managed stage controls fail preparation.
- Existing Go/plugin run ledgers and provider sessions are not migrated into a
  new portable logical run. Old evidence remains inspectable through its original
  implementation. Start portable work explicitly rather than claiming recovered
  coverage from those histories.

These changes make the example self-contained; they are not a claim of complete
compatibility with every legacy customization. The [historical parity matrix](PR_FIX_PARITY.md)
describes the earlier implementation. Deterministic fixtures for the portable
contract live under `examples/pr-fix/tests` and in the stock-engine example test.
Live providers, Docker conformance and GitHub publication conformance remain
separate opt-ins.

A configured native `runtime.limits.max-turns` ceiling must allow at least five
turns for the original attempt, one eligible retry and three checklist
continuations. Generated native tasks use up to eight turns within that ceiling.
Preparation counts native task, verification/fix and possible CI-repair workers
across every configured cycle, including repeated stages. Plans exceeding the
stock retained-worker limit of 64 or a lower `runtime.limits.max-workers` ceiling
fail preparation; closing a session does not restore that allowance. Publication
audience sessions also consume retained workers when their intents are dispatched.
CI observations remain in visible nested YAML repeats; the retained absolute
deadline governs the supported wait of up to 86,400 seconds.
