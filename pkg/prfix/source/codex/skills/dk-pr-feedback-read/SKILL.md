---
name: dk-pr-feedback-read
description: Read complete inline PR feedback, review bodies and general comments without changing GitHub.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-feedback-read` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Use [shared PR evidence](../../../docs/pr/supplied-evidence.md) for analysis and live verification.

Use the supplied complete evidence handoff. Without one, use `fetch_unresolved_review_threads.py` and `fetch_pr_conversation.py` under `runtime/dk-pr-feedback-address/scripts/`. Capture complete, stable snapshots across all pages, with source references.

Include settled decisions and deferred work. Distinguish resolvable inline threads from top-level comments and review bodies. Recheck the head and snapshot stability. A truncated page is incomplete.

Preserve intended participants and separately actionable requests in complete thread context for [feedback response obligations](../../../docs/pr/feedback-responses.md). Do not label feedback as requiring an author reply solely because it is unanswered or unresolved.
