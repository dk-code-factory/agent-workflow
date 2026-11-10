---
name: dk-pr-fix
description: Review and fix your PR, publish changes and run configured finalizer skills while preserving progress and budgets.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PLUGIN_ROOT to this plugin's root directory. Use the current agent and native skill tools. Check ownership with `scripts/pr_route.py --pr PR_URL` under [PR routing](../../../runtime/pr.md). Proceed for your PR or when explicitly asked to maintain another author's PR. Otherwise invoke `dk-workflows:dk-pr-review` before editing code or publishing. PR ownership alone does not grant permission to write or rewrite the branch.

Resolve configuration for `dk-workflows:dk-pr-fix` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Read [task state and handoffs](../../../docs/task-state.md) before managing a task. Follow [the fix recipe](../../../runtime/pr-iter.md) for completion and freshness rules. Execute this skill's resolved `tasks` in list order, honoring explicit parallel groups; the recipe owns repetition and applicability. Read selections with `skill-plan` and use the managed skill-occurrence lifecycle in the configuration guide. Configured tasks grant no permission. Keep the run id and counters on resume. Cached results may contain findings; they never authorize publication.

After review/fix work, follow [finalizers](../../../docs/finalizers.md). Run this skill's resolved `finalizers` invocation list in order, honoring explicit parallel groups. These selections replace default finalization actions. Use `completion_instructions` when configured; otherwise preserve the existing completion behavior. The default ending includes commit cleanup and final CI. Custom lists replace it; report omitted checks rather than silently running them. Final live checks and reporting remain coordinator duties.

Retain a separate explicit merge request supplied directly or through `dk-workflows:dk-pr-iter`, including "check out, iterate, then merge". After the fix workflow and finalization complete, fulfill it through `dk-workflows:dk-pr-publish` under [merge publication](../../../docs/pr/publication.md). Require current clean review, verification, successful CI and satisfied repository requirements even when custom finalizers omit those checks. Default no-merge scope limits permission inferred from iteration alone; it does not revoke that explicit request or require another confirmation. Preserve the exact PR and instruction on resume, subject to later narrower instructions.

Give every managed stage its prepared checklist, including feedback/reviewer fixes, CI repairs, hooks and finalizers. Require shared checklist acceptance before advancing. Before declaring the selected workflow complete, use the managed completion occurrence built from the live selected plan, including an empty plan; disabled finalizers remain disabled. Follow [completion checks](../../../docs/task-state.md#completion-checks).
