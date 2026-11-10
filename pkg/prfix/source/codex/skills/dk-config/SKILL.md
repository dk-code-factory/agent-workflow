---
name: dk-config
description: Change, inspect or explain saved dk-workflows settings when configuration is the user's task. Routine PR routing, review, fixing, splitting and finalization read settings directly without invoking this skill.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Use this skill for configuration requests, including questions about available settings and their effective values or sources. During ordinary workflow execution, use the read-only helpers in [workflow configuration](../../../docs/configuration.md) and [PR configuration](../../../docs/pr/configuration.md) directly. Consuming an existing policy, checking selected-skill availability or merging a PR under current permissions does not require invoking this skill or changing saved settings. Source inspection while developing or troubleshooting this package is ordinary repository work.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PLUGIN_ROOT to this plugin's root directory. Resolve this skill's scoped and invocation-local options through [skill configuration](../../../docs/skill-configuration.md), preserving original additional instructions. This skill owns its [defaults](config.json) and [schema](config.schema.json).

For saved changes, follow [the configuration agent contract](../../../docs/configuration-agent.md). Start a request from the exact user ask with `scripts/config_requests.py start`, retain its ID and delegate a configuration-only assignment to a fresh supported native agent. Pass the original ask, relevant user context, request ID and that contract. The native agent owns clarification, earlier-decision comparison, preference admission and application through validated helpers. It returns questions through the main agent; retain necessary answers with `clarify`, then resume the same request. After interruption, reconcile any existing native agent before resuming. Missing native delegation blocks this execution; never substitute a model CLI or another session runner.

Once intent and contradictions are settled, apply without another approval step. All saved-write commands require a request ID bound to the exact prepared operation. Preserve legacy stores, precedence, unrelated settings, domain histories, original instructions, per-PR choices, review caches and retained budgets. See [request commands and recovery](../../../docs/configuration-requests.md).

For inspection or explanation, delegate a fresh native configuration-only agent with the original ask, relevant context and the same contract, using a transient assignment without a request ID. That agent reads the relevant [workflow](../../../docs/configuration.md), [skill](../../../docs/skill-configuration.md), [PR](../../../docs/pr/configuration.md) or [preference](../../../docs/preferences.md) helpers directly. Create no durable request history and block this configuration-agent execution if native delegation is unavailable. Ordinary workflows continue using direct read helpers without invoking this skill.

Preference mutations invoked through `dk-workflows:dk-preferences` use this same configuration agent with that skill's admission rules. Inside the assigned agent, execute preference admission directly; do not recursively delegate. Skill/runtime edits, installation, other plugins' settings and PR execution are outside the configuration assignment.
