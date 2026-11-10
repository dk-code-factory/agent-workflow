---
name: dk-ci-approve
description: Approve fork workflow runs after complete incoming review, with current evidence and journaled recovery.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md) and [CI approval evidence](../../../docs/pr/ci-approval.md).

Resolve configuration for `dk-workflows:dk-ci-approve` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

This skill owns only the fork workflow approval gate. It permits CI execution; CI success, PR review approval and merging remain separate actions. Invoke during `dk-workflows:dk-pr-review` finalization after discovery, necessary independent verification, finding dispositions, configured tasks and publication reconciliation, before waiting for CI. The default `dk-workflows:dk-pr-review` option `permissions.approve_fork_ci:true` is independent of `approve_pr` and `merge_pr`. Analysis-only instructions disable this mutation. Never invoke automatically from `dk-workflows:dk-pr-fix`.

Read original review outputs and current task coverage. Require another author's open non-draft PR at the unchanged reviewed head and target, complete current selected review and verification, completed configured tasks, no unresolved actionable or uncertain findings, unchanged governing context and reconciled publications. Use the skill-owned evidence mode with the exact incoming-review and reviewer-run local options. Bind this finalizer's active spec and claim in `approval_task`; other unfinished claims block approval. A supplied `ready:true`, successful CI or human approvals cannot replace review evidence.

Run `python3 PACKAGE_ROOT/scripts/ci_approve.py approve --evidence FILE`. The helper discovers all pages and publishes one `ci-approve` journal operation per eligible run and attempt. It rechecks readiness before every dispatch. Return all retained operation paths and receipts, or the verified no-op when no eligible runs exist. Permission failures, ambiguous associations and changed inputs are blockers. After interruption or uncertain dispatch, reconcile retained operations read-only before further approvals. Never resend an uncertain approval or approve deployment gates.
