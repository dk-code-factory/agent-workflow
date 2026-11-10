---
name: dk-pr-description
description: Draft or update PR titles and descriptions using the diff, templates and agreed scope.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-description` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Read the complete diff, PR template, linked issue, maintainer conventions and current Decisions. Use the configured repository-relative `template` when present; null uses repository discovery. Apply `fallback_sections` only when repository formatting leaves a choice. Honor `update_title` and `update_body`, preserving each disabled field. Describe the resulting behavior and validation. Preserve Decisions and unrelated body sections. Make the description match the agreed scope before review.

Draft locally and review the text. Publish authorized edits with `scripts/publish.py` after checking the current head, title and body. Follow [the decision record](../../../runtime/dk-pr-description/references/decisions.md) when handling Decisions.

Use [the field conventions](../../../runtime/dk-pr-description/references/field-schema.md) when repository formatting rules leave a choice open.
