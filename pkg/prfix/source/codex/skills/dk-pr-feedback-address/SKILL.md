---
name: dk-pr-feedback-address
description: Verify and fix actionable PR feedback from inline threads, reviews and general comments.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-feedback-address` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Use [shared PR evidence](../../../docs/pr/supplied-evidence.md) for analysis and live verification.

Read complete, stable feedback from the supplied handoff. Without a handoff, collect it with the feedback read helpers. Read the governing instructions, Decisions and code for each item. Check its validity and scope. Preserve settled decisions and deferred work. Make focused fixes and run relevant checks. Run configured `additional_validation_commands` as argument arrays in the affected checkout in addition to relevant checks. Never use them to launch Codex or Claude Code.

Apply [feedback response obligations](../../../docs/pr/feedback-responses.md) to each request. Questions for other participants and discussion requiring no author response need no author reply or resolution. Preserve separately actionable author requests in mixed threads and explicit user instructions to reply. Carry source references and the reason for any no-reply assessment into the existing result/dispositions so later reply tasks do not invent an outstanding answer.

Before implementation, follow [preference retrieval and handoffs](../../../docs/preferences.md#workflow-handoffs-and-reuse) in `implementation` mode. Use a current complete caller handoff or resolve preferences and evaluate conditions directly. Preserve records and applicable paths. Refresh when paths, relevant records or condition context change, including before edits expand to new paths. Include the selection and condition decisions in managed task inputs.

Record each finding's disposition with evidence and source links. Follow the caller's permissions for pushes, replies and thread resolution. Local fixes alone do not authorize remote changes. Use the publication and reply skills for authorized remote changes. Return tested fixes, dispositions and remaining blockers.

For managed feedback and reviewer-finding fixes, use the prepared checklist to check every finding disposition, assigned deliverable and relevant test. Supply complete initial checklist evidence or finish the caller's recheck with the same agent before reporting accepted completion. Follow [completion checks](../../../docs/task-state.md#completion-checks).
