---
name: report-bug
description: Investigate suspected Agent Workflow engine defects, search for duplicates, and prepare sanitized bug reports for review or explicitly authorized posting.
---

Resolve the plugin root two directories above this skill. Use its absolute
`scripts/agent-workflow` launcher (`check`, then `exec --`) for verified engine
inspection. For a Claude host, pass `--host claude` on every launcher invocation
before `--`; the workspace selects its own execution runtime. Read [bug reporting](../../references/bug-reporting.md).

Inspect the definition, validation errors, environment and durable run evidence.
Distinguish invalid YAML/TOML, unsupported configuration, missing dependencies,
expired credentials and application failures from an engine defect. Do not
claim an engine bug solely because a workflow failed. Reconcile uncertain
effects before any attempted reproduction; use disposable local fixtures and
deterministic tools unless the user authorized live execution.

Collect a minimal reproduction, expected/actual behavior, engine/plugin/Codex
versions, platform and relevant sanitized evidence. Avoid secrets, auth files,
private transcripts, user paths and unrelated source. The bundled
`scripts/bug-report.py draft` creates a local draft from selected fields; inspect
it yourself for sensitive content because automatic redaction is best-effort.

Search open and closed issues in `dk-code-factory/agent-workflow` for duplicates
with `scripts/bug-report.py search --query <terms>`. Read plausible matches and
include links. If GitHub cannot be searched, report duplicate checking as
incomplete. A failed search does not prove absence of duplicates.

Present the exact title/body and duplicate findings for review. Post only after
explicit approval or an already explicit instruction to post that report.
Use `scripts/bug-report.py post` with the SHA-256 of the reviewed body. A digest
binds reviewed bytes; it is not itself user approval. Declined posting stops
publication. Do not create test issues during acceptance.

If creation has an uncertain result, inspect the durable posting journal and
search by its marker before retrying. The helper refuses automatic repeat
creation after an uncertain attempt. Never treat a network failure as proof
that no issue was created. Return a draft or verified issue URL, and explain
what remains unverified without inventing evidence.
