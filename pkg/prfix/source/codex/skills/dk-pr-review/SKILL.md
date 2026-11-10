---
name: dk-pr-review
description: Review another person's PR, verify findings independently and post confirmed issues. Approve or merge only when separately authorized.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PLUGIN_ROOT to this plugin's root directory. Follow [the incoming review recipe](../../../runtime/pr-review.md) and [task state and handoffs](../../../docs/task-state.md). Read this skill's resolved `tasks`, `finalizers`, `limits` and `permissions` through [skill-owned configuration](../../../docs/skill-configuration.md). Execute each list in order, honoring parallel groups, while preserving the recipe's applicability, independent verification, freshness and completion requirements. Use managed skill-occurrence descriptors for list occurrences; the recipe owns repetition. Use `dk-workflows:dk-pr-reviewers-run` to find issues, then a separate native verification pass before publication.

Resolve configuration for `dk-workflows:dk-pr-review` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

For an explicit verification-only assignment, perform only the independent finding-verification procedure on the supplied candidates and pinned context, then return dispositions and evidence. Do not execute workflow tasks/finalizers, edit, publish or recursively coordinate another review.

Keep original reviewer outputs, dispositions and sources, pinned head and target, native agent handles and publication receipts across interruptions. This workflow does not authorize changes to the author's code, description or commits. It normally posts verified findings; follow analysis-only instructions and saved policy. Fork workflow approval runs automatically when ready under `dk-workflows:dk-pr-review` option `permissions.approve_fork_ci` (default true), before waiting for CI; when selected in the finalizer list, use `dk-workflows:dk-ci-approve` with original review evidence and current coverage. PR approval and merging each require separate explicit or saved policy authorization and current completion checks.

Give discovery and independent verification agents their prepared checklists. Accept reports only after the shared completion checks; retain original evidence and report optional omissions. Follow [completion checks](../../../docs/task-state.md#completion-checks).
