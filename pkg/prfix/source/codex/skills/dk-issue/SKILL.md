---
name: dk-issue
description: Draft, create or update GitHub issues using repository templates and conventions.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-issue` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Read issue templates, maintainer conventions and evidence. A configured repository-relative `template` selects the template; null uses repository discovery. Use configured `fallback_sections` where no template governs. Apply configured `labels` and `assignees` only after verifying them in the target repository, with explicit request metadata taking precedence. Describe the trigger, observed and expected behavior, reproduction steps and relevant validation. Preserve the user's exact constraints. Review the draft before authorized creation or editing. Save the intended change and reconcile uncertain writes before retrying. A PR workflow does not authorize creating issues.

Use [the field conventions](../../../runtime/dk-issue/references/field-schema.md) when repository formatting rules leave a choice open.
