# Skill-owned configuration

Saved mutations use the [dedicated configuration agent](configuration-agent.md) and [request history](configuration-requests.md). Every saved-write command requires `--request-id ID` bound to its exact prepared operation; read commands are unchanged.

Every bundled skill owns `config.json` (explicit bundled defaults) and `config.schema.json` (its accepted options) beside `SKILL.md`. Codex is the maintained source; `scripts/sync_skills.py` copies all three files into Claude's distribution. Empty configuration objects are intentional for skills without domain options. Shared invocation fields stay in [the invocation schema](../schemas/invocation.schema.json).

Skill identities must always be fully qualified as `plugin:skill`, including bundled identities such as `dk-workflows:dk-ci`. Short names and paths are rejected. Configuration never launches Codex or Claude Code; the existing session owns execution and supported native delegation.

## Read and override

Read effective options without invoking the configuration skill:

```sh
python3 PLUGIN_ROOT/scripts/config.py --repository HOST/OWNER/REPO \
  skill-show --skill dk-workflows:dk-ci
```

The result includes complete `options`, actual `sources`, top-level field `provenance` and all `checked_paths`, including absent scopes whose later creation can change the effective configuration. Precedence is bundled defaults, user, optional upstream, repository, then invocation-local options. Pass `--upstream HOST/OWNER/REPO` before the subcommand. Within a scope, existing domain records and legacy process fields precede `skill_defaults["plugin:skill"].options`. Objects merge, lists replace, and commit policies replace as a whole. Schema defaults document behavior; readers insert the explicit values from the owning `config.json`.

Existing private workflow overrides, commit policies, size settings, completion prompts, preferences, exceptions, per-PR choices and receipts retain their original paths. New option overrides live under `skill_defaults["plugin:skill"].options` in the existing user or repository workflow file. No private data is moved into the installed skill directory.

For a selected invocation, preserve its original `instructions` verbatim as an additional prompt alongside the skill and caller's governing assignment. Its `options` are invocation-local overrides; save them as a private JSON object and pass `--options FILE` to readers and helpers that support it. Agent controls are separate from skill options. Unsupported native model/delegation requests block execution.

For an authorized settings change:

```sh
python3 PLUGIN_ROOT/scripts/config.py --repository HOST/OWNER/REPO \
  skill-set --skill dk-workflows:dk-ci --scope repo --file OPTIONS.json --request-id REQUEST_ID
python3 PLUGIN_ROOT/scripts/config.py --repository HOST/OWNER/REPO \
  skill-reset --skill dk-workflows:dk-ci --scope repo --request-id REQUEST_ID
```

`skill-set` replaces only that skill's options in the selected scope. `skill-reset` removes only those options, restoring inheritance. Both preserve other skills, instructions, agent controls and unrelated settings. Reads never save or normalize private records in place. Invalid saved values block resolution rather than being reset.

## Invocation lists

`tasks`, `finalizers` and `reviewers` contain shared invocations. Workflow lists are sequential; an explicit `{"parallel": [INVOCATION, INVOCATION]}` group launches independent invocations concurrently and waits for every sibling's terminal result. A failed required sibling blocks later work; incomplete optional work is reported. Repeated skills and instruction-only invocations are supported. Legacy nested parallel arrays remain readable. There is no responsibility registry, condition language or generic process executor. The owning skill controls iteration, applicability, freshness and completion within retained budgets. A configured task does not grant permission.

Read a selected list with `skill-plan --skill PLUGIN:SKILL --kind tasks|finalizers|reviewers`, using the same repository, upstream and local options. `groups` preserves ordering and parallel boundaries. Flatten groups only to address a zero-based occurrence, never to discard execution ordering. Reviewer scheduling additionally honors its owning `execution` (`parallel` or `sequential`) and `max_parallel` (null means no extra configured cap), within actual host capacity. There is no default reviewer; an empty reviewer selection cannot establish review coverage.

Add `--resolved` when constructing handoffs. The additional `resolved_groups` preserves those same boundaries; each entry has a flattened `index`, the inherited `invocation` (including exact instructions and native agent controls), and its leaf `configuration` with effective options, sources, checked paths and provenance. Instruction-only entries have null `configuration`; their options remain in `invocation`. External skills retain their own options contract. This read saves nothing and does not establish availability, execution or freshness at a later task boundary.

Preflight selected skills and native controls with `skill-preflight --skill PLUGIN:SKILL --available SKILLS.json [--capabilities HOST.json]`. It checks nested plans, routes and policy providers, including optional selections, and rejects recursive selections. Require `ready:true`. For selected reviewers, also pass `--native-reviews REVIEWS.json` and verified host evidence capabilities under [native evidence preflight](native-review.md#evidence-capability-preflight), before claims or cycle consumption. Use the same `--options` and upstream at preflight and execution.

For managed work, use `scripts/dk.py prepare --file REQUEST.json` with the canonical repository and PR. Each descriptor selects its owning skill, list kind and flattened index, while its ordinary definition supplies resolved resources, original assignment, consumed context and permissions:

```json
{
  "run": "RETAINED_RUN",
  "tasks": [{
    "kind": "skill", "skill": "dk-workflows:dk-pr-fix", "selection": "tasks",
    "index": 0, "options": {}, "definition": {"skill_identity": "dk-workflows:dk-pr-feedback-read", "resources": {"skill": "/installed/feedback/SKILL.md"}, "assigned_instructions": "Read complete current PR feedback.", "instructions": {}, "inputs": {}, "options": {}, "governing_inputs_complete": true}
  }]
}
```

Use `selection:"finalizers"` for finalizers; the retained run is required. Use `selection:"reviewers"` for reviewer leaves, passing the selected reviewer-run local options. Preserve the same local options and upstream on resume. Finding verification definitions assign `finding-verification` and an independent native evidence contract; instruction-only work uses its owning skill identity. Reviewer definitions use `content-review`. The helper derives distinct occurrence identities while retaining those evidence requirements. Definitions for these independent tasks include their predeclared `native_review` contract.

`prepare` retains the compact selector with the existing replay definition. Fresh sessions use `resume`, then `prepare` with saved keys; `finish` regenerates the current selection before recording results. The low-level `skill-spec --skill PLUGIN:SKILL --kind tasks|finalizers|reviewers --index N --definition FILE` remains available, with `--run RUN_ID` for finalizers and `--options FILE` for local owning options.

Claim, execute, record and recover under [task state](task-state.md). Specs capture skill-owned defaults/schemas and resolved options, preserve additional prompts and check live selection and policy before claims and recording. Each occurrence has its own identity; finalizers additionally bind the retained run. List edits can change occurrence selection: reconcile unfinished work before adopting a changed plan. Cached results never replace live CI or publication checks. The skill must collect actual host observations; configuration and self-reported JSON do not prove execution or semantic correctness.

Legacy process commands remain compatibility readers for existing saved settings. New skill prompts use skill-owned plans and managed `prepare`/`finish`; they do not introduce coordinators, named task maps, cycle nodes or a second agent runner. Low-level spec/start/record commands do not attach managed checklists on their own and are reserved for integrations.

## Domain settings

CI and flake helpers read their owning options automatically and accept `--options FILE`. Explicit CLI timing/discovery flags override the selected options. CI deadline windows survive interruption; changing configuration does not restart an existing window. There is at most one pending-only extension.

Automatic discovery during CI classification also reads `dk-workflows:dk-ci-flakes` options, using the CI caller's upstream. `ci_wait.py --flake-options FILE` supplies overrides for that skill separately from CI timing options. Unchanged scans reuse their retained identity, including older default-limit scans; changed discovery limits permit a distinct scan without deleting prior evidence. An interrupted unchanged scan is not silently retried.

`limits.retries_per_task`, `limits.ci_repairs_per_cycle` and `limits.ci_reruns_per_head` accept zero to disable those attempts. The initial task and review remain eligible; `review_cycles` stays between one and five. Retained counters and tighter saved limits survive resume and cannot be replenished by changing configuration.

Commit grouping policies and PR size settings also consume their skill-owned options while retaining older scoped records. Repository requirements and exact user instructions govern drafting; configured fallback sections and message lengths apply where those leave a choice. Metadata such as labels, assignees and templates must exist in the target repository before use. A null template selects repository discovery.

Additional validation commands are argument arrays, supplement relevant checks, and must never launch Codex or Claude Code. Feedback replies honor `resolve_answered_threads` only within caller authorization. Partial review publication always uses `COMMENT`; configurable review events never authorize approval. Publisher merge defaults only fill an omitted method in an already authorized concrete request; all live merge guards still apply.

Pass `--upstream HOST/OWNER/REPO` and `--options FILE` to `publish.py prepare` when supplied by the publisher invocation. An explicit request method takes precedence; the existing journal retains only the resolved concrete method. Publish and reconcile use that retained intent without rereading default methods.

Preference default modes apply when admitting new records. Changing the default does not reinterpret persisted records or silently change an existing record's modes. Completion prompts, instructions and options cannot weaken evidence requirements, expand permission or restore omitted finalizers.
