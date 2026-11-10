---
name: dk-commit-message
description: Draft or revise Git commit messages using the changes and repository conventions.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-commit-message` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Read the diff, repository guidance, commit template and recent human commits. When supplied by cleanup, apply the verified [commit policy](../../../docs/commit-policy.md) message instructions. Explicit policy instructions replace built-in stylistic defaults and historical style preferences; required repository conventions and current user constraints still apply. Preserve authorship and source records. Return the reviewed message, policy and sources. Commit only when requested.

Without explicit message instructions, match repository style and apply the configured `subject_target_length`, `body_wrap_length` and `max_line_length` fallbacks and use [the field conventions](../../../runtime/dk-commit-message/references/field-schema.md) when repository formatting rules leave a choice open.
