---
name: dk-pr-iter
description: Identify the PR author and your accounts, then fix your PR or review another person's PR under saved publication policies.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PLUGIN_ROOT to this plugin's root directory. Follow [PR routing](../../../runtime/pr.md). Follow an explicit request to fix or review. Otherwise run `scripts/pr_route.py --pr PR_URL` with invocation-local `--options FILE` when supplied, using resolved `personal_accounts` and fully qualified route invocations, report its selected process and invoke the returned `invocation` under the shared [invocation contract](../../../docs/invocations.md). Default routes select `dk-workflows:dk-pr-fix` or `dk-workflows:dk-pr-review`. Preserve custom prompts and native agent model/effort settings; preflight their support. Pass original instructions, identity evidence and saved run state. Unknown identity blocks automatic routing. Routing grants no extra permission to publish, approve or merge.

Resolve configuration for `dk-workflows:dk-pr-iter` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

For a combined request such as "check out this PR, run dk-pr-iter, then merge it", carry out the requested checkout safely and retain the explicit merge instruction and exact canonical PR through routing, downstream handoffs, finalization and resume. Pass that permission to either selected workflow. After it completes, fulfill the merge request through `dk-workflows:dk-pr-publish` with current clean review, verification, successful CI and satisfied repository requirements. Default `dk-workflows:dk-pr-review` option `permissions.merge_pr.enabled:false` and the fix workflow's default no-merge scope do not revoke the user's separate instruction. Do not ask again merely because the workflow was routed or resumed. A later narrower instruction overrides the affected permission; report a concrete blocker if readiness fails. Checking out or iterating alone still grants no merge permission, and merging grants no permission to post `APPROVE`.

Preserve the retained run across sessions. Distinguish interruption resume from a renewed explicit user request for review; pass its exact instruction, source and stable request id downstream. An exhausted run can receive a bounded continuation only under [retained budgets](../../../docs/task-state.md#retained-budgets), without resetting history or treating a fresh session alone as authorization.
