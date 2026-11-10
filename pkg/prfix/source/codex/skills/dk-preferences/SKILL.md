---
name: dk-preferences
description: Add, update, list, inspect or remove personal and repository preferences for implementation and review, with concrete scopes and contradiction checks before saving.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PLUGIN_ROOT to this plugin's root directory. Read [the preference contract and commands](../../../docs/preferences.md). Use `scripts/preferences.py` for private catalog access. Existing saved PR rules are separate; do not convert them automatically.

Resolve configuration for `dk-workflows:dk-preferences` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

For mutations, route through the dedicated native configuration agent in `dk-workflows:dk-config` under [its contract](../../../docs/configuration-agent.md). Pass the exact ask, relevant scope and original instructions. If already inside that assigned agent, perform the admission below directly without recursive delegation. Unavailable native delegation blocks mutations. Retain the request ID across clarification and interruptions. Apply through [configuration requests](../../../docs/configuration-requests.md); `commit` and `remove` require the ID bound to the prepared operation.

For listing or inspection, use `list` or `show` and `render` the persisted records. For removal, identify the requested records and use the observed revision. Preserve unrelated records.

For additions and updates:

1. Read the user's request, current repository layout, applicable repository instructions and all current personal and repository preferences through `list`. Personal admission checks the current repository only. Inspect instructions governing the proposed scope, including new-file ancestors. Do not infer upstream preference inheritance.
2. Produce one independently applicable instruction per record. Remove filler and vague language while preserving strength, conditions and exceptions. Prefer exact file, recursive directory or root-anchored glob selectors, including intended new files. Use a condition only for applicability paths cannot express. Prompt-only scope has an empty include list and requires a condition. Use exclusions for path exceptions. Ask when making a request concrete would require inventing intent; do not silently weaken or broaden it. For newly admitted records, default omitted application modes from `default_applies_to`. Preserve existing records' persisted modes when an update omits them; changing defaults never reinterprets saved records.
3. Run `prepare` with the complete proposed change set. Updates supply the existing stable ID and the complete replacement record. Inspect the normalized proposal and returned admission context. Compare potentially overlapping records and applicable repository instructions for contradictions, including personal versus repository records, conditions and exceptions. Retrieval has no priority system. Present each contradiction with its sources and obtain a revision, replacement, narrowing or cancellation before saving; do not pick a winner automatically. Include all agreed replacements and removals in one transaction. Reuse exact duplicates returned by `prepare`.
4. Once the instruction and scope are concrete and contradictions are settled, admit the exact preference proposal through the configuration request helper and apply it without an additional approval step. Bind the request to the current governing instructions, admission context and earlier decisions. A stale revision requires fresh preparation, rereading current instructions and admission against the new context. Changes in repository instructions or request context also require fresh admission even if the catalog revision is unchanged. Do not blindly retry a failed commit: inspect current records after an uncertain write.
5. Read back changed or reused records with `show` and render them as Markdown. Show ID, owner, application modes, includes, exclusions, condition and instruction. Report removals by ID. Never claim that an unsaved proposal was persisted.

Admission examples:

- “Make my tests better” needs clarification about the desired concrete practice; do not invent a testing rule.
- “Use pytest fixtures for shared setup in tests except generated tests” becomes a `tests` directory include and `tests/generated` directory exclusion, with the original instruction strength preserved.
- “When implementing HTTP retries, honor Retry-After” can use the relevant source paths plus that condition; use prompt-only scope when no stable path scope is known.
- An existing “Use unittest setup methods” preference overlapping a new pytest-fixture request requires a user choice to revise, replace, narrow or cancel before saving.
- If repository instructions prohibit shared fixtures in the same scope, surface that conflict with the instruction source even for a personal preference. Do not save a contradictory rule by assuming personal or repository precedence.
