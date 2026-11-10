# PR fix finalizers

Keep `finalizers` in the owning skill's options and use [skill-owned configuration](skill-configuration.md), `skill-plan` and managed `prepare`/`finish` under the retained run. Skills can repeat and each occurrence has its own claim. Legacy process APIs remain available for existing saved plans.

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

`dk-workflows:dk-pr-fix` runs `finalizers` after review/fix tasks finish. Save the list through `dk-workflows:dk-config` with the other workflow settings:

```json
{
  "skill_defaults": {
    "dk-workflows:dk-pr-fix": {
      "options": {
      "finalizers": [
        "dk-workflows:dk-commit-split",
        "dk-workflows:dk-pr-decisions",
        "dk-workflows:dk-pr-description",
        "dk-workflows:dk-pr-feedback-reply",
        {"parallel": [
          {
            "skill": "team-tools:check-links",
            "instructions": "Check links in changed documentation.",
            "agent": {
              "model": "chosen",
              "reasoning_effort": "high"
            }
          },
          "my-plugin:check-licenses"
        ]},
        "dk-workflows:dk-ci",
        "dk-workflows:dk-pr-publish"
      ]
      }
    }
  }
}
```

Every entry uses the shared [invocation contract](invocations.md): a fully qualified skill identity, or an object with `skill`, `instructions`, `options`, `agent` and `required`. Instruction-only calls use the owning skill identity. Resolve selected skills and requested agent controls through native tools before work; unavailable or ambiguous selections block execution. A skill may appear in distinct occurrences; use an index to select repeated skills. There are no user-supplied IDs or deeper nested lists.

The outer list is sequential. An explicit `{"parallel":[...]}` object is one parallel group; legacy nested nonempty arrays remain readable. Claim the group's tasks before launch, invoke its independent skills concurrently through native tools, and wait for every launched task to settle before advancing. Record successful siblings immediately even when another fails. Failed, incomplete, cancelled, uncertain or interrupted work blocks later groups. If native parallel execution is unavailable, report the group blocked rather than silently running it serially. Do not put skills that write the same PR body, thread or branch in the same group. Resolve dependencies before launch; an unsafe group is a configuration blocker, not permission to reorder it.

## Defaults and scope

Omitting the owning skill's `finalizers` option inherits the next lower scope. Existing `processes.pr-fix.finalizers` records remain compatibility inputs. A supplied list replaces the entire inherited list; `[]` disables finalizers. Scope order is repository, explicitly supplied upstream, user, then defaults. The default sequential plan selects `dk-workflows:dk-commit-split`, `dk-workflows:dk-pr-decisions`, `dk-workflows:dk-pr-description`, `dk-workflows:dk-pr-feedback-reply`, `dk-workflows:dk-ci` and `dk-workflows:dk-pr-publish`. Shared `skill_defaults` apply to these calls; local invocation objects override them.

The default cleanup assesses the full series and publishes a tree-preserving rewrite only when needed. Decisions, description and reply finalizers finish outstanding work. CI observes the final published head. The default publisher reconciles outstanding operations and requests re-review from appropriate prior reviewers when authorized; invoke it with concrete prepared requests. Finalizers perform no-op assessments when their work is already satisfied.

Reply finalizers apply [feedback response obligations](pr/feedback-responses.md): unanswered discussion or questions for another participant do not create an author reply requirement. A sourced no-reply assessment can complete without publication or user approval to omit an optional answer. Leave those threads unresolved, including the other participant's open question in a mixed thread. Concrete author requests, explicit user reply instructions and retained publication reconciliation still require their usual handling; repository review and merge requirements remain in force.

Before selected commit cleanup, resolve [the effective commit policy](commit-policy.md). Supply its verified handoff to cleanup and include configuration, provider resources, consumed context and returned instructions in managed task inputs. Native policy providers only return grouping and message instructions; failure blocks cleanup and later finalizers. Run this before final CI. A custom list without cleanup does not run the provider or add cleanup. Policies do not change permissions or squash intermediate pushes.

A custom list determines finalization actions. Do not run removed defaults through a hidden fallback or the completion prompt. This setting does not disable feedback fixes, scope assessment, description/Decisions synchronization or replies required earlier in review/fix work. Incoming review and standalone skills do not execute this list.

## Execution and freshness

The agent reads the plan and invokes skills. Python handles the shared [managed task lifecycle](task-state.md); hooks and finalizers use the same bookkeeping and preserve their existing occurrence identities. Read normalized groups with:

```sh
python3 PLUGIN_ROOT/scripts/config.py --repository HOST/OWNER/REPO \
  skill-plan --skill dk-workflows:dk-pr-fix --kind finalizers
```

The result contains `groups`, each an array of references, and configuration `sources`. A single string becomes a group of one. Pass `--upstream HOST/OWNER/REPO` consistently when inheriting that scope.

For the current group, call `prepare --file REQUEST.json` with the retained run and one task per skill. Each task uses `kind:"skill"`, `skill:"dk-workflows:dk-pr-fix"`, `selection:"finalizers"`, a zero-based flattened `index` and an ordinary `definition` with resolved resources, exact assignment, original permissions, immutable starting context and consumed inputs. Omit `responsibility`; the helper derives it and captures live plan/configuration. For example:

```json
{
  "run": "RUN_ID",
  "tasks": [{"kind": "skill", "skill": "dk-workflows:dk-pr-fix", "selection": "finalizers", "index": 4, "definition": {
    "skill_identity": "team-tools:check-links",
    "resources": {"skill": "/installed/team-tools/check-links/SKILL.md"},
    "assigned_instructions": "Check links in the pinned PR documentation. Return evidence and explicit completion.",
    "instructions": {"user": "/private/original-instructions.txt"},
    "inputs": {"context": "/private/finalizer-start-context.json"},
    "options": {}, "governing_inputs_complete": true
  }}]
}
```

Save the request privately and prepare the current group:

```sh
python3 PLUGIN_ROOT/scripts/dk.py --repository HOST/OWNER/REPO --pr PR_URL \
  prepare --file /private/finalizer-request.json
```

The helper verifies membership and the selected skill identity, captures the selected invocation and resolved options, checks live selection drift and derives task identity from the retained run and list occurrence. It returns each assignment, claim and checklist without requiring separate spec files or claims. Supply exact assignments from the original request, applicable completion instructions and the selected skill; the helper does not infer a task from its name. Invocation options, prompts and model/effort requests follow the common contract. A definition's `options` supply additional execution context.

Prepare only the current group; do not claim later groups before it completes. Omit reviewer cycle, retry and CI budget flags from finalizer requests. Follow the returned lifecycle actions and submit each original output and explicit completion with `finish`, then satisfy its `recheck` gate before accepting completion. Verify successful terminal execution and the skill's nonempty completion report. Mere launch, progress, or an empty findings list is insufficient. Save native handles separately from governing inputs when delegated. Finalizer claims consume no review-cycle/retry budget; reserve the existing CI repair and rerun budgets before those actions.

The low-level `skill-spec`, legacy `finalizer-spec` and `start`/`record` commands remain [compatibility APIs](task-state-low-level.md) for integrations. They do not attach a managed checklist on their own. Workflow agents use `prepare`/`finish`, including for legacy saved plans through `kind:"finalizer"` descriptors.

Keep the same run on resume. Reconcile unfinished claims and uncertain side effects before recovery. Reordering can change occurrence identity; reconcile unfinished work before adopting a changed plan. Changes to the plan, assignment, resource or consumed context invalidate affected results. Removing a skill does not erase its unfinished claim; reconcile it explicitly. Preserve completed unchanged siblings. New runs get separate finalizer tasks, while publication journals still prevent duplicate writes across runs.

Use immutable snapshots for starting inputs a skill intentionally changes, then verify live resulting state before accepting its output. CI and publication tasks normally use `cacheable:false` and always perform current checks. Cached completion is never proof of current CI or a remote write. Reconcile saved receipts before repeating an action. When a task is noncacheable, inspect its saved completion and effects before a new invocation.

Run commit changes and governing-content synchronization before CI covering the resulting head. A parallel group cannot contain CI and a skill that may change that head. If finalizer effects invalidate scope or review coverage, settle the current group and return to the affected fix/review stages before continuing the plan. Retain the same run, review budgets and CI deadlines. After CI repairs, refresh scope, affected reviews and any selected cleanup assessment, then obtain CI for the resulting final head. Later changes stale prior CI and require renewed evidence; they cannot be hidden by list ordering.

## Completion

After the selected list finishes, prepare the coordinator’s managed `kind:"completion"` occurrence under the same run and satisfy [completion checks](task-state.md#completion-checks) before declaring success. Its helper captures the live selected plan, including an empty list. `dk-workflows:dk-pr-fix` performs a final live check of scope, instructions, discussion, Decisions, head, target, review coverage, finalizer results and publication receipts. Follow effective completion instructions within the selected plan and permissions. These checks and the report are coordinator duties, including for `[]`.

Report selected-plan completion separately from PR readiness. Identify skipped default duties and missing evidence. A plan without CI may finish its selected actions, but cannot claim CI was verified; omitted cleanup cannot be reported as assessed. External finalizers may supply equivalent current evidence. Failed selected work remains blocked. Report optional incomplete reviews, accepted findings and CI exceptions. A completion prompt cannot silently restore disabled finalizers or expand permission.

All finalizers inherit the exact caller permissions. `dk-workflows:dk-pr-fix` alone does not authorize merging, creating issues or unrelated writes. Preserve a separate explicit merge request and fulfill it through `dk-workflows:dk-pr-publish` after current review, verification, CI and repository requirements pass, including when a custom finalizer list omits CI. Reordering or adding a skill grants no additional permission.

Give every finalizer its prepared checklist and follow the shared `recheck` gate with the same agent before accepting completion. Mandatory exhaustion blocks later groups; report every best-effort omission.
