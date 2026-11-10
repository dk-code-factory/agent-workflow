---
name: dk-pr-publish
description: Publish prepared PR changes, replies, reviews and authorized merges with current checks and recovery records.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-publish` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Follow [publication and recovery](../../../docs/pr/publication.md). Use `scripts/publish.py` for description edits, replies and pushes with exact leases. Use its `resolve-thread`, `request-review`, `create-draft-pr`, `ci-rerun`, `ci-approve`, `post-review` and `merge-pr` handlers for other actions. This skill checks GitHub state and handles recovery independently of the workflow cache.

For a previously verified reply that is now unavailable or edited, use `inspect-reply` and an explicitly reassessed `dispose-reply` decision from the publication contract. Preserve historical evidence separately from the current answer obligation. A replacement requires a fresh separately authorized request linked by `replaces`; uncertain initial writes and unavailable or ambiguous read access remain protected. A settled reply disposition does not authorize thread resolution.

For an uncertain review request, use `inspect-review-request` and, when reassessment supports it, `dispose-review-request` with `no_resend`. Keep historical failure/provenance separate from current observations; absent reviewers do not prove non-delivery. Independent authorized replies may proceed under their own live checks while the original review intent remains pending. Never replay it or change action IDs to bypass uncertainty; follow the publication contract's freshness and duplicate guards.

For a concrete merge request without a method, use `default_merge_method`; an explicit authorized method takes precedence. Merging requires separate explicit or saved policy authorization, completed clean review and verification, and current CI. Iteration alone does not authorize merging. Save the exact target, payload and returned operation path. After an uncertain outcome, use that operation for read-only reconciliation. Cached analysis never authorizes an action.

Pass supplied upstream scope and invocation-local publisher options to `scripts/publish.py prepare` with `--upstream` and `--options FILE`. Recovery uses the retained concrete intent, even after default settings change.

An explicit request to merge the exact PR supplies authorization with the default configuration, including for your own PR. `dk-workflows:dk-pr-review` option `permissions.merge_pr.enabled:false` disables automatic incoming-review merges; it does not veto the request. Require satisfied repository review requirements; do not invent an approval requirement where GitHub has none. When blocked, report the exact guard diagnostic and required next step privately, distinguishing rejection before dispatch from an uncertain write.

Fork CI approval uses [current incoming-review evidence](../../../docs/pr/ci-approval.md), independently of PR approval or merging. Use `dk-workflows:dk-ci-approve` to discover eligible runs and retain one operation per run and attempt.
