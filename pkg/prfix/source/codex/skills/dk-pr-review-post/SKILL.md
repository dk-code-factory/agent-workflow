---
name: dk-pr-review-post
description: Publish verified inline PR findings when authorized; report workflow blockers privately.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-review-post` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Read original review results, verification dispositions, and the current head and diff. Publish only completed, verified findings. Each comment must map to a changed line and preserve its evidence. An incomplete review may post independently confirmed findings as `COMMENT`; report remaining coverage and verification blockers privately to the invoking user. Withhold findings whose evidence is insufficient.

Post only when explicitly requested or authorized by the invoked incoming review workflow. Local analysis alone does not authorize publication. Use `scripts/publish.py` with kind `post-review` and the exact reviewed payload. Approval requires separate authorization and completed clean coverage. Never approve the authenticated account's own PR.

Save the operation path. After uncertainty, reconcile actor, head, body and comment identities. Use `blocking_event` and `nonblocking_event` for completed verified findings; partial reviews always use `COMMENT`. Add a substantive repository summary only when explicitly requested or `include_summary` is enabled. These options never authorize `APPROVE`. Keep the general body empty otherwise; never publish operational scope, coverage blockers or execution limitations. Do not claim a partial review is clean.
