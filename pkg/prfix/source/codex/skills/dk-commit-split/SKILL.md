---
name: dk-commit-split
description: Review and organize all PR commits while preserving the reviewed final tree, authorship and logical commit boundaries.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-commit-split` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Resolve [the commit policy](../../../docs/commit-policy.md) before cleanup. The policy helper combines existing saved policies with this skill's `policy` option; null selects built-in behavior. Pass supplied invocation-local cleanup options through the policy helper's `--options FILE` on spec, handoff and verify; this preserves local prompt/provider/null precedence. Honor `max_passes` without exceeding the existing five-pass ceiling. Reuse a verified current supplied handoff; direct calls resolve their own effective policy. A saved prompt supplies grouping and message instructions directly. Resolve a selected policy skill natively with pinned PR context, history, repository requirements and current user constraints; require successful execution and a complete report with nonempty `policy_prompt`. Policy providers only return instructions. Missing or incomplete providers block cleanup.

Follow [commit cleanup](../../../docs/pr/commit-cleanup.md). Inspect every commit and apply the effective policy to boundaries and messages. Explicit policies replace built-in stylistic defaults; required repository conventions, exact user constraints and rewrite safeguards still apply. Explain why no change is needed, or reorganize commits when authorized and safe. Save policy sources, ownership evidence, recovery refs, exact remote leases, test results, boundary reasons, and the reviewed and final heads and trees.

Pass the policy's message instructions and original constraints to `dk-workflows:dk-commit-message`. Use `dk-workflows:dk-pr-publish` for authorized pushes with exact leases. A policy grants no rewrite or publication permission. An unchanged head does not prove that every commit was assessed. Use `dk-workflows:dk-pr-split` to move work into separate PRs.
