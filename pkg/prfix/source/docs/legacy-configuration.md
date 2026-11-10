# Legacy process compatibility

Saved mutations use the [dedicated configuration agent](configuration-agent.md) and [request history](configuration-requests.md). Every saved-write command requires `--request-id ID` bound to its exact prepared operation; read commands are unchanged.

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

Configure a process coordinator, its named tasks and its permissions. Every call uses the same [invocation contract](invocations.md) and [JSON Schema](../schemas/invocation.schema.json): `coordinator`, values in `tasks`, reviewer lists, cycle hooks, finalizers, commit policy providers and saved drafting selections. Every selected skill must use its exact `plugin:skill` identity; short names and paths are rejected.

```json
{
  "$schema": "../schemas/config.schema.json",
  "schema_version": 1,
  "skill_defaults": {
    "dk-workflows:dk-pr-reviewers-run": {
      "options": {
        "reviewers": ["code-review-ad:code-review-ad"]
      }
    }
  },
  "processes": {
    "pr-fix": {
      "tasks": {
        "review_changes": {
          "skill": "dk-workflows:dk-pr-reviewers-run",
          "instructions": "Run the configured reviewers on the pinned change."
        }
      },
      "limits": {"review_cycles": 3},
      "hooks": {
        "after_cycle": [
          {"instructions": "Check invariants affected by this cycle."}
        ]
      }
    },
    "pr-review": {
      "permissions": {"post_review": false, "approve_fork_ci": false}
    }
  }
}
```

The schema reference above is relative to the bundled [example](../examples/config.json). When keeping a configuration elsewhere, point `$schema` at the installed schema or associate it in the editor. Helpers treat `$schema` as metadata and never fetch it. See the [schema catalog](../schemas/README.md) for all file formats and validation boundaries.

## Read and save

Run `python3 PLUGIN_ROOT/scripts/config.py --repository HOST/OWNER/REPO COMMAND`:

| Command | Result |
| --- | --- |
| `show` | Effective explicit overrides and their source files. |
| `show --expanded` | Overrides merged with every bundled process default. |
| `set --scope repo\|user --file CONFIG.json --request-id ID` | Validate and replace the selected scope. |
| `reset --scope repo\|user --request-id ID` | Remove that scope's workflow settings. |
| `preflight --available SKILLS.json [--capabilities HOST.json] [--process NAME]` | Verify selected skills and native agent controls. |

Private files remain under `${XDG_CONFIG_HOME:-~/.config}/dk-workflows/v2/`: `user.json` and repository digest filenames. Objects merge by key across defaults, user, optional upstream and repository scopes. Lists replace inherited lists; `[]` clears one. Shared `skill_defaults` apply to every use of the selected skill; invocation-local settings override them. Custom instructions are preserved verbatim. An invocation object must select a `skill` or supply `instructions`, including when overriding an inherited string.

`set` replaces the whole selected scope. Read its existing source file and preserve unrelated settings before changing it. Place `--upstream HOST/OWNER/REPO` before `show` or `preflight`. Pass the same upstream to `scripts/pr_route.py` and managed invocation definitions. Existing private rules, scoped preferences, PR policies, completion prompts and runtime state retain their own stores and source contracts; see [PR configuration](pr/configuration.md) and [preferences](preferences.md).

## Processes

The bundled defaults provide `pr-iter`, `pr-fix`, `pr-review`, `pr-reviewers-run`, `commit-message` and `issue`. A custom process needs a `coordinator` invocation. The coordinator's recipe owns ordering, loops, verification and recovery. It selects tasks by purpose name, rather than by skill identity or list position. Missing capabilities block the affected responsibility.

| Field | Meaning |
| --- | --- |
| `coordinator` | Invocation managing the process. |
| `tasks` | Purpose names mapped to invocations used during the process. |
| `permissions` | Allowed remote and local mutations. Selecting a task grants no permission. |
| `limits` | Retained review, retry and CI budgets. |
| `hooks` | Ordered invocations before and after review cycles. |
| `finalizers` | Ordered completion invocations, with nested parallel groups. |
| `routes` | Process names selected for your PR or another author's PR. |

Task names use `snake_case`, such as `read_feedback`, `review_changes`, `verify_findings`, `post_review`, `approve_fork_ci` and `check_ci`. Fix tasks also include `collect_decisions`, `address_feedback`, `split_scope`, `organize_commits`, `draft_commit_message`, `update_description` and `reply_to_feedback`. `classify_ci_failures` and `publish_pr_actions` select the CI classification and publication helpers. See the [complete defaults](../examples/config.expanded.json) for every task.

`coordinator` and each task accept a skill name or the full invocation object with custom instructions, options and native agent controls. For example, `"coordinator":{"skill":"dk-workflows:dk-pr-fix","instructions":"Preserve the published API.","agent":{"model":"gpt-6.1-sol","reasoning_effort":"high"}}` uses exactly the same format as a reviewer or hook. `pr-review.tasks.verify_findings` selects the independent finding verifier and assigns verification only; never recursively invoke the full review workflow for that task.

Task maps merge by name across scopes. Overriding `tasks.review_changes` preserves `read_feedback`, `check_ci` and the other inherited tasks. An empty task object makes no overrides. Adding a task does not schedule execution: the owning coordinator's recipe must call it. Hook, reviewer and finalizer lists still replace inherited lists, and an empty list disables that inherited selection. Replacing an implementation does not remove obligations in the governing recipe.

`pr-iter.routes` maps `own_pr` and `other_pr` to configured process names. Routing compares immutable account IDs on their own host. Additional personal accounts live in `accounts.personal` as `{"host":"github.com","id":123}`. Verify the account before saving its numeric ID. Host names normalize to lowercase; each scope replaces the entire account list. The authenticated account needs no entry. Unknown identity blocks automatic routing.

PR process limits default to five review cycles, one same-input retry per task, five CI repairs per cycle, and three CI reruns per head. They may be lowered with `review_cycles`, `retries_per_task`, `ci_repairs_per_cycle` and `ci_reruns_per_head`. The runtime retains the lowest limits observed for a run and never resets consumed counters. Configuration cannot raise these ceilings. A renewed explicit user review request can receive one bounded `continue-run` grant under [retained budgets](task-state.md#retained-budgets); preserve the initial configured limit and all consumed counters after a grant.

`pr-fix.permissions` enables the actions covered by ordinary maintenance: exact-branch pushes and guarded commit rewrites, PR edits, feedback replies and fully answered thread resolution, draft split PRs and appropriate review requests. It does not default to approval or merging. `pr-review.permissions` defaults to `post_review:true` and `approve_fork_ci:true`; its other permissions are false and its recipe remains read-only apart from authorized review publication, fork workflow approval, PR approval and merging. Its narrower process contract still applies even if an unrelated write flag is set. `approve_pr` is separate from `approve_fork_ci`. `merge_pr` is `{"enabled":false,"method":"squash"}` by default; supported methods are `squash` and `merge`. PR approval and merging need their own enabled setting or exact user instruction. Narrower instructions, including analysis-only review, override saved policy. Live CI, review, ownership and repository requirements still apply.

The ordered `pr-fix.finalizers` list uses common invocations and explicit parallel objects; legacy nested parallel lists remain readable. See [finalizer execution](finalizers.md). Fork workflow approval is separate from PR approval and CI success; [CI approval](pr/ci-approval.md) retains its current-review requirements.

## Availability and models

There is no default reviewer. Select reviewers through the `options.reviewers` of the `review_changes` task, either in shared `skill_defaults` for its selected skill or in its process-local invocation. Standalone `pr-reviewers-run` uses its coordinator's reviewer options. Selection follows the purpose name even when the selected skill is replaced. Reviewers use ordinary invocation strings or objects; `required` defaults to true. Optional selected skills must also resolve before work. Review completion, coverage and finding disposition remain separate.

Resolve installations with native tools and pass their exact identity strings as a JSON array to `preflight --available`. Every configured identity must match its installed fully qualified identity exactly. Require `ready:true`; exit 0 alone is insufficient. The result reports `missing_selected_skills`, `ambiguous_selected_skills`, `unsupported_agents` and `resolved_skills`. With `--process`, check the selected process and its nested calls; route first when using `pr-iter`. Without it, check all configured processes.

An `agent` object requests a separate native agent. Model and reasoning controls need verified host capabilities, supplied as:

```json
{
  "delegation": true,
  "models": {"gpt-6.1-sol": ["low", "medium", "high"]},
  "reasoning_efforts": ["low", "medium", "high"]
}
```

Use actual host observations, not this illustrative list. `models` lists available models and their supported efforts; `reasoning_efforts` describes the inherited model. Unsupported requests block work instead of being ignored or replaced. Omitting `agent` preserves normal process execution, including independent verification where the recipe requires it. An inline skill call does not switch the current agent's model. Preflight proves availability only; each skill owns its instructions, options, execution and completion report.

## Updating older settings

This format replaces the former flat configuration. Unsupported older flat records and short skill identities are rejected with an explicit error and are never reset or silently converted. Update the existing private file deliberately: move identities to `accounts.personal`, shared invocation settings to `skill_defaults`, workflow entrypoints to `coordinator`, and role or operation selections to purpose-based `tasks`. Move incoming review policy to `processes.pr-review.permissions`: `post_findings` becomes `post_review`, `approve_ci` becomes `approve_fork_ci`, `approve` becomes `approve_pr`, and `merge` becomes `merge_pr`. The verifier lives at `tasks.verify_findings`. Reviewer options belong to `tasks.review_changes`, or shared defaults for its selected skill. Cycle hooks move under each owning process; remove user-supplied hook IDs. Move the finalizer list to `processes.pr-fix.finalizers` and replace `/skill/PLUGIN:SKILL` entries with plain `PLUGIN:SKILL` names. Preserve original prompts, options, permissions, opt-outs and source records. Regenerate task specs from the new selectors without resetting retained history or budgets; reconcile pending effects before adopting changed lists.

External packages own their domain settings, which may be referenced through `extensions`. The separate `dk-architecture` package is not invoked automatically. Renaming or reselecting a skill does not move state or complete unfinished work.
