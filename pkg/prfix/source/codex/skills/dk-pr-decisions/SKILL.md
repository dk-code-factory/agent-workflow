---
name: dk-pr-decisions
description: Collect and reconcile settled PR decisions, then publish them when authorized with source references.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-decisions` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Use [shared PR evidence](../../../docs/pr/supplied-evidence.md) for analysis and live verification.

Read the complete PR discussion, user instructions and current Decisions. Distinguish settled choices from suggestions, open questions and generated progress. Save exact sources. Ask the user about important unresolved scope choices. Base proposals on evidence and label them as proposals.

Follow [decision language review](../../../runtime/dk-pr-decisions/references/language.md) for the configured `language_review_passes`, at most three passes. Use the configured `section_heading` when locating and publishing the bounded decision section; consumers read this setting from this skill. When authorized, use `scripts/publish.py` to update only Decisions. Preserve the rest of the title and body. Return structured decisions, open questions, sources and original instruction snapshots.
