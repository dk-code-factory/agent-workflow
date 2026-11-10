---
name: dk-pr-split
description: Move unrelated work or split large changes into separate PRs under saved size, scope and publication policies.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-split` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Follow [partition guidance](../../../docs/pr/partition-cleanup.md). Read current size and per-PR partition policies directly with `scripts/pr_config.py --cwd REPO --pr PR_URL pr-size list` and `pr-partition show` under [PR configuration](../../../docs/pr/configuration.md). Consuming those policies does not invoke `dk-workflows:dk-config`. Save ownership evidence, recovery refs, exact remote leases, test results and reasons for each split boundary. Explain why work should stay in this PR, or return verified publication receipts for the split. Use `dk-workflows:dk-commit-split` to assess or reorganize commits while preserving the final tree.
