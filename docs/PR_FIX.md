# PR-fix YAML application

`agent-workflow pr-fix` materializes the PR maintenance contract from
**dk-workflows 5.2.0** into ordinary, versioned YAML workflows. The application
registers individual operations with the existing typed action API. Provider turns use native `agent/run` tasks and their ordered event hooks; PR
claims, evidence validation, and publication remain application actions.

The authorized core extension, `WorkerManager.AcknowledgeFailure`, lets explicit
YAML stages acknowledge settled optional failures and eligible retries in the
same execution. It preserves original failure status, classification, and
outputs, closes the worker, and records a deduplicated receipt. It grants no
successful task coverage. Active turns, uncertain effects, and unsafe failure
classes retain the cleanup barrier. See [the parity matrix](PR_FIX_PARITY.md).

## Preparation and execution

Requirements: Git, Python 3, `gh` authenticated for the selected host, and a
`codex` runtime configured with provider `codex-app-server`, explicit native or
API-key authentication, and its existing execution policy. Native tasks support
Codex's default tool policy; explicit managed tool subsets and MCP grants remain
unsupported. The bundled source readers use Python's standard library; an
installed dk-workflows package is not required. Preparation never refreshes
provider login.

Run from a checkout containing the PR head and target objects. Initial runs
require a clean worktree. Preparation performs GitHub reads, resolves settings,
validates every selected adapter and role, and writes only local application
state and generated definitions. It never starts an agent or publishes anything.

```sh
agent-workflow pr-fix prepare --workspace . --role codex \
  --instruction-file /path/to/original-request.txt \
  --adapters examples/pr-fix/adapters.yml \
  --options examples/pr-fix/options.json \
  https://github.com/OWNER/REPO/pull/123
```

The `codex` role must exist in `agents.yml`; adapter pass roles use that runtime.
An existing controller retains its admitted role configuration. Restart it after
changing `agents.yml`; role drift blocks claims before native dispatch.
The example options preserve default tasks and finalizers while supplying a
reviewer. **The source default reviewer list is empty.** Selecting the reviewer
stage without configuring reviewers blocks preparation.

Preparation returns the logical run ID, immutable plan path, generated workflow
reference, resolved roles, input arguments, stable admission request ID, omitted
finalizers, engine capacity accounting, and potential capacity warnings. Generated
files live under `workflows/.pr-fix/RUN/FINGERPRINT/`. They can be inspected using
normal workflow validation, status, attempts, logs, and the workflow UI. They
contain private assignments and must stay out of version control.

```sh
agent-workflow pr-fix run --workspace . --prepared /absolute/plan/path.json --detach
agent-workflow status --workspace . ENGINE_RUN_ID
agent-workflow pr-fix resume --workspace . --run LOGICAL_RUN_ID
```

Flags precede positional arguments. `run` also accepts the preparation flags and
a PR URL. `--prepared` inherits the saved configuration path unless `--config`
explicitly overrides it. It uses the normal workspace controller. Client disconnection or a
client timeout leaves admitted work retained. `resume` reloads the saved inputs,
reads current settings and role configuration, and resumes unchanged unfinished
work. Changed pins require a replacement execution under the same logical run.
The application first checks that the previous execution has stopped, reconciles
uncertain writes read-only, and retains original native outputs and completed
siblings. Unknown effects or unsettled invocations block replacement.

The private ledger is `.agent-workflow/pr-fix/ledger.sqlite`; immutable evidence,
plans, operation results, and source contracts are alongside it. Engine attempts
and native messages remain in the engine store and are referenced by identity.
Existing plugin run histories are never imported. A workspace's retained logical
run for a PR preserves budgets even when tasks or finalizers are changed.

## Settings, adapters, and limits

The pinned readers preserve defaults → user → optional upstream → repository →
invocation precedence, replacement of lists, exact additional instructions,
preferences, whole commit policies, legacy settings, and per-PR partition policy.
Use `--upstream HOST/OWNER/REPO` for an explicit upstream scope. Original stores
are read in place and are never rewritten. Invocation options use `--options`;
`{"tasks": [], "finalizers": []}` still runs the managed live completion gate.

Every selected custom skill requires a versioned adapter. The example adapter
shows sequential procedure layers and parallel passes. Each pass declares its
role, bounded assignment, and evidence requirements. Repeated invocations receive
distinct occurrence identities. Nested reviewer lists expand before dispatch.
Unavailable, recursive, unsupported, and conflicting selections block preflight,
including optional selections. Supported custom controls are declared explicitly;
currently only an exact role-matched model control is available. Reasoning effort
and other undeclared controls block preflight.

Read-only parallel passes must declare any access that conflicts with a writer.
Parallel mutations are rejected because leaves share a Git index. Select
sequential procedures for edits. Resource directories, contents, modes, and
symlink targets are pinned; cycles and oversized resources block preparation.

Default budgets are five review cycles, one eligible usage/session retry per
occurrence and cycle, five CI repairs per cycle (including cycle zero), and three
reruns per head. Reservations are transactional and idempotent. Changing a plan
does not reset them or retained CI deadlines. Three same-session checklist rounds
are allowed; an unfinished checklist does not renew analysis. Existing engine
worker, role, closure, and iteration limits still apply. Preparation rejects a
first pass that exceeds a static worker limit and reports potential repeated-pass
capacity overruns; conditional publication and recovery workers also consume the
engine's fixed per-execution worker allowance.

An exhausted review allowance can be renewed with an explicit, deduplicated
authorization. This records a grant and does not start work:

```sh
agent-workflow pr-fix continue-run --workspace . --run LOGICAL_RUN_ID \
  --request-id USER_REQUEST_ID --authorization 'Exact renewed user instruction' \
  --source 'chat:message-id' --cycles 2
```

Replaying the same request does not grant more cycles. Changed text, source, or
cycle count under that request ID is rejected.

## Evidence and publication

Leaves receive a bounded assignment, exact instructions, pinned source contracts,
current preferences, report schema, and occurrence-specific checklist. They may
not select stages, coordinate agents, commit fixes, or publish. Original outputs
and engine-observed native passes establish coverage; valid JSON and empty
findings alone do not. Checklist follow-ups use the same native session. After
controller shutdown, explicit YAML lifecycle stages recover the session binding
without replacing the original analysis with a fresh session.
Runner setup failure before message dequeue stops the application wait with the
original worker diagnostic; it does not invent a native completion or acknowledge
the unresolved message.

Admission reserves space for the largest checklist envelope within the worker's
1 MiB prompt limit; exact instructions are never truncated. Parallel publication
or CI procedures that share managed singleton occurrences are rejected before
claims. Inactive cycle hooks do not consume first-pass worker admission.

Each original finding receives a claim-scoped identity, so independent reviewer
local IDs cannot collide and checklist continuations preserve the same identity.
Finding verification preserves original sources. Fixes need sourced dispositions;
accepted or deferred findings require exact retained user authority. Content
reviews can survive tree-preserving cleanup; history, publication, and CI evidence
refresh separately. Scope reports cover final-diff accounting, both partition
modes, per-PR opt-outs, ownership boundaries, and recovery refs. Cleanup reports
cover the entire series and distinguish prepared work from published work.
Failed or incomplete optional reports retain their original findings and omissions
but cannot establish accepted dispositions. Conditional preference decisions must
remain within that preference's matched paths.

Cycle admission reserves the retained review allowance before any before-cycle
hook. Reusing accepted reviewer results skips cycle hooks; after-cycle hooks run
after all configured cycle tasks, including fixes and publication.
Final coverage requires both the retained cycle identity and current hook
fingerprints. A changed hook-only resource blocks that hook's coverage without
invalidating reviewers that did not consume it.

Commit-policy providers return a nonempty `policy_prompt` in their original
accepted report. Cleanup receives those exact instructions, context and result
references; an empty or stale provider result blocks cleanup. Literal policies
preserve their exact text, and null selects built-in behavior. A verified
history-only cleanup retains its starting policy handoff for the resulting head.

Configuration never grants publication or merge permission. Supply an explicit
`--authorizations` JSON file when writes are authorized. Merge reconciliation checks the numeric actor and exact target/head parents for
the selected merge method. Each operation kind, or
`iterate` for ordinary maintenance operations, maps to:

```json
{"request_id":"user-message-id","instruction":"Exact user instruction","source":"chat:message-id"}
```

Maintaining another author's PR also needs `--maintain-other` with that object.
Otherwise preparation reports the incoming-review route and does not edit the PR.
Merge requires its own `merge` authorization; `iterate` never authorizes it.

Supported intents are guarded pushes/rewrites, PR edits, general/inline replies,
thread resolution, new split-branch pushes and draft PRs, CI reruns, review
requests, and separately authorized merges. Split branches require an accepted
scope boundary with current governing inputs, even when a content change keeps
the added-line count unchanged, and an absence lease; the draft PR requires the reconciled
branch receipt. Manual partitions need concrete separate authority. Resolution
requires the actual inline reply in the exact thread. CI reruns validate the
run/head/attempt before dispatch and retain their per-head budget.

Every actual payload gets a separate native audience review naming its exact
fingerprint. Writes retain exact intents and receipts. Lost responses are probed
read-only before another dispatch; absence of a comment or draft PR does not
prove that an uncertain creation never happened. Such uncertainty blocks replay.
Reply identities bind actor, PR, inline destination and exact body independently
of caller IDs. Alternate IDs reference the original effect and receipt; they
cannot create another answer or replace an uncertain original.
Previously verified replies have a separate recovery path. Run the generated
`reply-recovery.yml` with the retained plan and original intent ID. Its
`prfix_reply_inspect` action captures the current PR/discussion, numeric actor,
original receipt and individual reply. A 404 is `unavailable` only when the
complete reply list also lacks that reply; access errors and inconsistent
observations block recovery. Inline inspection also requires its routing source.
The bounded `reply_recovery` leaf reassesses whether an answer remains necessary.
It may propose a new reply with `replaces` containing the original `run`/`id`,
inspection path/fingerprint, `decision:"replacement_required"`, concrete reason,
original sources and exact authorization. Supply that authorization under
`reply-replacement:ORIGINAL_RUN:ORIGINAL_ID`; existing user instructions may
support it, but neither configuration nor the missing comment grants permission.
One replacement is linked transactionally to each inspected disposition. Its
publication runs separately through the ordinary audience and publication YAML.
Replay retains that link and receipt; changed evidence requires fresh inspection
and reassessment, and uncertain linked writes must be reconciled first. Stale
undispatched replacements can be superseded while retaining their history. The
original intent and historical receipt are never rewritten. A later missing
linked answer is recovered through that answer's own operation. An obsolete
answer instead uses the existing sourced no-reply assessment and authority rules.
Only exact reconciled application writes can explain discussion changes between
preparation and dispatch; incoming discussion invalidates a prepared operation.

Focused commits use only the accepted changed-file manifest and native commit
message, verify staged blobs and resulting tree/parent, and preserve unrelated
files, including filenames containing Git pathspec metacharacters. Untracked
snapshot hashing streams regular files within a 64 MiB aggregate limit and
10,000 entries; it rejects oversized files before allocating their contents.
Public text is separate from private workflow reports. Merge runs after
selected completion and requires current review/verification, exact-head CI, and
live repository merge requirements, including REST `mergeable_state:clean` and
GraphQL `mergeStateStatus:CLEAN`. No cached result grants merge authority.

An uncertain review request can be settled without inventing delivery evidence.
The generated `review-request-recovery.yml` performs read-only inspection of the
original intent, current requested reviewers, complete reviews and timeline.
A bounded audience assignment reassesses current code and discussion. It may
return a sourced `review_request_disposition` with `decision:"no_resend"`, the
inspection fingerprint and a concrete reason that no further request is needed.
YAML then calls `prfix_review_request_dispose`; it retains the original intent,
historical state and `delivery:"unknown"`, without a publication receipt or POST.
Changed PR, actor, head, requests, timeline, reviews or discussion requires fresh
inspection and reassessment. Missing history or backend support blocks recovery.

For a stopped execution, run that generated recovery workflow through the normal
controller with the same role configuration, retained `plan` path and exact
intent ID; then resume the original PR-fix execution. For example:

```sh
agent-workflow run --config agents.yml \
  --input plan=/private/retained-plan.json \
  --input 'intent={"id":"original-request-id"}' \
  workflows/.pr-fix/RUN/FINGERPRINT/review-request-recovery.yml
```

Use the actual generated reference and saved configuration path. This recovery
does not reset the logical run, claims, budgets, deadlines or publication history.
If another review request is still required and historical delivery remains
unknown, the obligation stays blocked; changing IDs cannot authorize resending.

The final private report distinguishes `selected_workflow_complete` from
`pr_ready` and lists omitted default finalizers, optional failures, readiness
issues, and blockers. Disabling work does not silently restore it or imply PR
readiness. Admitted custom finalizers can cover the corresponding default duty
through their declared adapter kind and accepted current evidence. A custom CI
finalizer still needs controller-observed successful exact-head CI; an identity
change alone neither omits the duty nor supplies its completion.
Deterministic validation uses fake providers, fake GitHub commands,
and disposable local repositories. No live-provider or publication conformance
scenario is part of the application test suite.
`make test` runs the full suite with the race detector and a sixty-minute
per-package deadline for the workflow recovery integration tests.

## Source and license

[pkg/prfix/source/PROVENANCE.json](../pkg/prfix/source/PROVENANCE.json) pins the
copied 5.2.0 contracts, schemas, defaults, and compatibility readers by SHA-256.
The GitHub adapter follows the official [review-comment reply API](https://docs.github.com/en/rest/pulls/comments#create-a-reply-for-a-review-comment)
and [pull request creation API](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request), including explicit same-owner fork identities.
Their [MIT license](../pkg/prfix/source/LICENSE) is retained. The application only
executes its read-only settings bridge and pinned readers; the bundled runtime subset is
the transitive configuration-reader import closure. Publication and provider
orchestration helpers outside that closure are omitted.

CI test exemptions require complete exact-attempt metadata and logs for every
failed job step. Each failed-step log must fit supported test-runner envelopes
and exact single-line test signatures. Successful setup-step output is excluded
by the failed-step reader; missing or ambiguous step attribution blocks proof. Additional or unsupported output
blocks a test exemption, including otherwise benign output that this verifier
cannot account for. Explicit whole-pipeline exceptions are checked separately. Rerun proposals require an integral exact
attempt before dispatch. A verified rerun receipt atomically opens one retained
observation window for that operation; replay preserves the window and every
older deadline. CI duration controls exceeding Go's representable whole-second
range are rejected during preparation before claims or provider work.

An issue-backed flake exemption also requires a live open issue in the exact PR
repository, matching its canonical URL rather than a pull request. Its declaration
must affirmatively match the workflow, job, test and failure signature: either a
matching `dk-flaky` JSON block with `flaky:true`, or affirmative prose with all four
values quoted exactly. Negative, uncertain, partial and unscoped prose cannot
exempt a failed check.

Live governing-policy changes, including a PR partition opt-out, require a
replacement preparation under the same logical run. A separately authorized
merge also requires the clean reviewed local head to equal the published head;
selected-workflow completion cannot merge an older head while leaving a local
fix unpublished.

Publication preserves the exact reviewed public body. Creation reconciliation
uses private pre-dispatch object inventories, numeric actor identity and unique
post-baseline objects with the full expected payload. Missing or ambiguous writes
remain uncertain; no private operation ID is appended to GitHub text.

Native reports are limited to 16 MiB, 4,096 entries per collection, 16,384 aggregate
entries and 32 levels of JSON nesting before typed decoding. These checks bound
collection expansion as well as serialized size. Inherited reviewer-group
instructions and governing resources reach each child; optional groups retain
child failures as omissions. Group-level native coordinator controls are rejected
because the group is deterministic; configure supported controls on its children.
Each bounded leaf contract still controls the permitted work. Mutating claims must retain their own resources and
applicable implementation preferences through report acceptance.

Optional reviewer failures remain explicit omissions and may finish selected work,
but they supply no review coverage. Readiness and merge require at least one
accepted current completed native review in addition to every required review.


Top-level PR-fix invocation instructions are retained verbatim alongside the
original user instruction and inherited group/leaf prompts. Native model/effort
controls on that deterministic top-level process are rejected before claims;
configure supported controls on bounded leaves instead. Review and verification
occurrences remain read-only; writable hooks and custom mutators receive the
implementation preference handoff, conditional-decision gate and drift checks.

Preparation and recapture pin workspace `AGENTS.md` and scoped `AGENTS.md` files
in changed-path ancestors, including ignored files. Their paths and digests reach
every leaf, and instruction-only changes invalidate affected execution even
without a content diff. Adapters must declare other governing external/backend
resources explicitly.

A custom adapter is a versioned replacement procedure. Its declaration owns the
required passes, deeper-look work and evidence; engine-observed coverage applies
to that declared procedure. It does not claim execution of an installed plugin's
coordinator or infer an undeclared third-party driver contract.

GitHub integer identity tokens remain exact in provider responses, saved evidence
and receipts. Resource byte budgets apply to bytes actually read, with a bounded
reader protecting the stat/read interval.
