# Portable PR maintenance example

The [copyable example](../examples/pr-fix/README.md) implements PR maintenance
using Python helpers and ordinary YAML workflows with the stock `agent-workflow`
binary. PR-specific capture, report validation, retained allowances, Git policy,
CI classification and publication receipts belong to the example. The controller
owns native agent sessions, scheduling, admitted assets and recovery.

The checked-in `examples/pr-fix/workflows/main.yml` owns preparation, bounded
maintenance cycles, completion/readiness outputs and optional authorized merge.
Reusable YAML shows task barriers, independent verification/fixes, same-session
checklist follow-ups, finalizers, CI observation/repair and publication gates.
Preparation snapshots immutable evidence. Execution writes no workflow YAML or
agent configuration files. Python helpers perform one boundary operation per invocation;
they do not launch providers or run an orchestration loop.

## Setup and preparation

Use an independent Git clone with a real `.git` directory, checked out at the PR
head with the target commit available locally. Container workspace mounts reject
linked worktrees whose `.git` is a file. Commit the copied example definitions
before preparation, and start with a clean worktree. Preserve the exact original
request in private JSON. Put orchestration settings in YAML inputs, deployment
settings in `agents.yml`, and native agent definitions in TOML.

Use a self-contained clone: Git object alternates outside the workspace are not
mounted into helper containers. If cloning with `--reference`, add `--dissociate`.

Writing plans need a repository-local commit identity. Set the intended author
and committer in this clone before preparation:

```sh
git config --local user.name 'Your Name'
git config --local user.email 'your-address@example.com'
```

Helper containers have a private home and do not inherit the host's global Git
configuration or author/committer environment. Preparation rejects writing plans
without valid local values; read-only plans do not need a commit identity.

Install the stock binary, Docker and the configured Codex app-server runtime.
The helper image must already be available locally under a digest-pinned
reference or immutable local content ID and contain Python 3.11 or newer, Git,
`gh` and `sleep`. Python helpers
use only the standard library. Any compatible image can be used; no PR-specific
Dockerfile or custom Go binary is required. Set the image under `execution.image` in `examples/pr-fix/agents.yml`.
Helper jobs omit `image` and inherit the configured image before admission.

Allow enough controller time for the complete selected plan, including review
after source changes. The default `controller.run-timeout` is 30 minutes; set a
larger value such as `2h` in the deployment configuration for long live runs.
The CLI `--timeout` controls client waiting or foreground execution and does not
change the detached controller's run deadline. Restart an existing controller
after changing its configuration, then resume the original run with its retained
claims and budgets.

For recovery, preserve the original deployment file so its hash and retained task
coverage remain unchanged. After stopping its old controller, start the controller
in a separate terminal with an operational deadline override:

```sh
agent-workflow controller --workspace /path/to/checkout \
  --config examples/pr-fix/agents.yml --controller-run-timeout 2h
```

Then resume or submit a replacement execution against the original logical-run
ledger. The override is valid only for `controller`; it does not alter the pinned
agent configuration or replenish review, retry or CI allowances. An unresolved
effect still requires its original reconciliation evidence.

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

Set `agents.yml` `execution.image` to that exact `sha256:...` value. The base tag chooses build
inputs; the resulting content ID pins the actual admitted image. A compatible
registry `IMAGE@sha256:...` reference is also supported. The zero digest in the
sample configuration is a template placeholder, not an available tool image.

Run preparation and the controller as the same non-root workspace owner. The
preparation container's UID/GID must match the container execution identity so
private files remain readable by the admitted helpers. Provider authentication
belongs to the configured host runtime. GitHub credentials enter helper processes
through the declared `github_token` secret, never through plan JSON. The transport
selects the canonical PR host and maps `GH_TOKEN` to `GH_ENTERPRISE_TOKEN` for
enterprise requests when necessary. Preparation never refreshes login.

Copy [request.example.json](../examples/pr-fix/request.example.json) to a private
file. Set the PR URL and exact original instruction. Optional `logical_run`
defaults to a stable PR identity; authorizations retain their request ID, exact
instruction and original source.

```sh
printf '\n/.pr-fix-state/\n/examples/pr-fix/request.json\n' >> .git/info/exclude
cp examples/pr-fix/request.example.json examples/pr-fix/request.json
```

## Stock CLI execution and recovery

Commit authored definitions and configuration first. Supply the existing
credential through `GH_TOKEN`; flags precede the workflow reference:

```sh
agent-workflow validate --config examples/pr-fix/agents.yml examples/pr-fix/workflows/main.yml
agent-workflow run \
  --config examples/pr-fix/agents.yml \
  --input request=examples/pr-fix/request.json \
  --secret-env github_token=GH_TOKEN \
  examples/pr-fix/workflows/main.yml
agent-workflow status RUN_ID
agent-workflow attempts RUN_ID
agent-workflow logs --follow RUN_ID
```

There is no separate preparation command. `snapshot_request` retains the original
request as immutable JSON. Structured `prepare` validates YAML selections, reads
GitHub and retains its plan and result through interruption or response loss.
Preparation cannot dispatch agents or publish. Bindings, checkpoint validation
and reconciliation phases are read-only.

Evidence binds the authored workflow closure, retained helpers, five TOML
roles and the opaque admitted execution/configuration fingerprint exposed by the
engine. Subsequent helpers reject plans from another admitted configuration.
Recovery uses engine snapshots and retained bundles without reloading changed
workspace copies.

`main.yml` declares defaults for `tasks`, `finalizers`, `reviewers`, `adapters`,
`limits`, `permissions`, `ci`, `merge_method`, `accounts`, `settings`, `resources`
and `turns`. Override them with ordinary CLI inputs or a reusable wrapper:

```sh
agent-workflow run --config examples/pr-fix/agents.yml \
  --input request=examples/pr-fix/request.json \
  --input 'tasks=["read-feedback","describe-pr"]' \
  --input 'finalizers=[]' --secret-env github_token=GH_TOKEN \
  examples/pr-fix/workflows/main.yml
```

Selections retain order and repeated occurrences with distinct stable IDs.
Nested arrays form parallel read barriers; writers run sequentially. Managed
review stages run reviewer groups, then independent verification and fixes.
Empty selections still reach coverage and completion. `batch.yml` runs ordered
groups through `group.yml`, then optionally performs independent verification
and fixes. Ordinary task groups and explicit review stages share this sequence
while retaining their original order, parallel read barriers and occurrences.
`native.yml` owns one report schema and report-acceptance boundary for initial,
retry and checklist turns. Initial turns select from five literal TOML roles
admitted together in a fixed catalog; retry/checklist expressions
carry original session references. Eligible retries retain their original failure
classification, while failed checklist turns stop without entering another retry.

Task names select assignments from `adapters`; they do not load installed skills.
The defaults use `read-feedback`, `reconcile-decisions`, `address-feedback`,
`assess-scope`, `describe-pr`, `inspect-commits`, `sync-guidance` and `review`.
Use an object to supply task-specific controls, for example
`{"task":"read-feedback","instructions":"Include deferred requests."}`.
Orchestration stages are explicit objects: `{"stage":"publication"}`,
`{"stage":"reviews"}` and `{"stage":"ci"}`. A reviews stage can override
the top-level reviewers with `{"stage":"reviews","reviewers":[["review","review"]]}`.
Stages cannot run inside parallel task groups. Reviewers must select read-only
review adapters.

For the same Toolkit, Standards and Spec reviewers used by `code-review-ad`,
prepare the [native pass profile](../examples/pr-fix/profiles/README.md) before
admission. It snapshots the installed package briefs, criteria, checks and drivers,
then selects three fresh parallel engine sessions and independent verification.
The profile records its execution differences from the standalone skill and pins
supplemental frozen specifications as resources.

The earlier `skill` objects and fully qualified selector names remain supported. An exact adapter key takes precedence over its alias. Alias
lookup accepts either spelling, including legacy adapter maps used with the new
default selections.

Shallower helper jobs share `helper.yml`, which returns one `result` object.
It calls a static command with the operation and payload as structured input;
each call keeps its own checkpoint, identity and reconciliation boundary.
Native report acceptance selects exactly one initial, retry or checklist result
inside the same helper operation that validates and retains the report. It
preserves engine failure metadata and rechecks continuation and audience evidence
when reusing a checkpoint, without a separate result-selection container.
The deepest native-turn workflow keeps inline helper calls to preserve the
engine's eight-level reusable-workflow limit, including a caller
wrapper. An original attempt and its optional same-session retry feed one
checklist continuation loop. CI polling keeps two bounded loops and the retained
absolute deadline, with each observation and delay inside `ci-poll.yml`.

An existing controller retains its admitted runtime configuration. After its
owned work stops, restart it when changing `agents.yml`; passing another config
does not replace the running controller's roles. The sample uses local
`codex-app-server` agents with `workspace-write` and helper containers with bridge
networking. Native waits release capacity; containers finish before agent dispatch.

A client disconnect or wait timeout leaves admitted controller work retained.
Inspect the run before requesting a resume. Resume the original run after its
owned work has stopped; keep its configuration and credentials available:

```sh
agent-workflow resume --workspace . --config CONFIG \
  --secret-env github_token=GH_TOKEN --request-id RESUME_REQUEST_ID RUN_ID
```

Normal engine recovery reuses the original native session, admitted definition,
turn allowance and helper bundle. Same-session checklist continuation is explicit
in the checklist mode of `native.yml`, with at most three follow-ups. An unfinished
application claim cannot be dispatched by a replacement execution merely because its process has
changed. Resume its originating run or inspect and reconcile the retained effect;
unknown publication effects never grant automatic replay. `--retry-uncertain`
does not override structured mutation reconciliation.

For changed instructions, settings or role definitions, start a replacement only after the original execution has stopped and uncertain
effects have been settled. Keep the same `logical_run` for the PR. Its
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
Checklist evidence, finding sources and disposition sources name these evidence IDs.
Keep references from earlier reports in `deliverables`; their evidence IDs do not
identify evidence in the current report. Each assigned
checklist item must appear exactly once, with `completed` or `incomplete` status;
completed items require evidence. A true `complete` flag alone grants no coverage.

Report contract errors, including extra or missing checklist IDs and invalid
disposition references, retain the rejected
report separately and continue the original native session within the existing
three-round checklist allowance. They grant no coverage and cannot commit or
publish. Source/governing-input failures remain blocking. Corrections preserve
every rejected finding identity and any fenced source edits; only a valid final
report can create the controlled commit. Put supplemental checks and actual test
results in `deliverables` instead of inventing checklist IDs.

The native wire schema closes every object and requires all of its properties,
as Codex structured output requires. Optional values are nullable. Publication
operations are JSON-encoded object strings, and task-specific `deliverables` is
a JSON-encoded object string or null. The helper decodes these open-ended fields
during report acceptance and retains ordinary object reports. Malformed encoded
values enter same-session contract correction after source evidence validation.
Initial turns,
same-session retries and checklist continuations use the same wire schema.

Report `evidence` paths name current repository source or guidance files relative
to the workspace. Captured GitHub discussions and retained report artifacts are
assignment context: reference them in `deliverables`, and attach the relevant
source files separately. Private state and generated workflow files cannot serve
as source evidence, even when their content hashes are correct.

Review fingerprints include the current tree and diff, target/base trees,
discussion, instructions, settings, relevant resources, task controls and native
definition/helper identities. Content reviews can survive history-only changes;
history-sensitive and head-sensitive tasks refresh their respective evidence.
Changed code, incoming discussion or governing inputs invalidates affected reuse.
An independent native verifier accounts for every original finding before fixes.
Finding IDs remain claim-scoped, and checklist continuation preserves original
findings. Continuations use the original native session and immutable workspace
references to its full assignment and prior report, keeping large documents out
of the combined helper/native input envelope. Accepted/deferred findings need
the exact retained user authorization.
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
original head, target and method-specific commit parents. The `merge_method` YAML input
defaults to `merge`; preparation rejects values other than `merge` or `squash`
after retaining the request snapshot, before capturing the PR or dispatching
agents or publication.

## State, backup and compatibility

Application state is `.pr-fix-state`, with an ownership marker, per-logical-run
SQLite ledgers and immutable artifacts. Back up `.pr-fix-state` alongside the
engine recovery bundle and retained helper assets. Preparation records include
the original request, plan and response receipts. Neither cleanup nor backup may
discard another run's or unrelated files.

Migration from generated workflows:

- Move JSON `options` fields to the corresponding declared `main.yml` inputs.
  Move `settings`, `resources` and `adapters` there too. Use `--input NAME=JSON`
  or a checked-in reusable wrapper to retain custom policy.
- Move `image` and `runtime` settings to `agents.yml`; edit the five TOML files
  instead of selecting role paths in the request. `turns` is a workflow input;
  the admitted runtime's worker/turn ceilings still apply.
- Keep only PR identity, exact instructions, logical-run identity and explicit
  authorizations in the request JSON. Remove the separate `action.py prepare`
  command and generated `--input plan=...` execution path.
- Existing admitted runs retain their original engine snapshots, helper bundles
  and generated closures. Do not rewrite them, remove their historical state or
  treat a new execution as recovered coverage. Resume the originating run.

- Supply explicit YAML selections, settings, resources and adapters.
  Implicit host XDG/plugin settings, arbitrary installed skills, automatic
  upstream discovery and legacy process hooks are not imported. Stage reviewed
  effective values explicitly. Unsupported selections fail preparation.
- Custom adapters describe one native task using the declared role names,
  assignment, checklist, resource paths and sensitivity flags. Parallel groups
  may contain independent readers; writers sharing the Git index run in order.
  Arbitrary provider/session controls and shell orchestration are not adapters.
- Managed publication and CI stages accept only `stage`; a reviews stage also
  accepts `reviewers`. Legacy managed selectors accept empty default controls,
  with `options.reviewers` additionally supported for the reviewer selector.
  Put task instructions and optionality on task selections; unsupported managed
  stage controls fail preparation.
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

The `turns` input must permit at least five turns for an original attempt, one
eligible retry and three checklist continuations. It defaults to eight; a lower
runtime ceiling requires a matching input. Retained workers, retries and repairs
remain bounded by the engine and application ledger. Closing sessions does not
restore consumed allowances, and publication audiences consume retained workers.
Preparation bounds the selected plan size; it does not reserve workers for every
conditional repair or publication audience. The separate engine worker ceiling
can stop execution before all review or repair allowances are used.
CI observation uses bounded nested YAML repeats with a retained absolute deadline
of up to 86,400 seconds. Every repair is followed by fresh exact-head observation;
completion and merge recheck the final published head.
