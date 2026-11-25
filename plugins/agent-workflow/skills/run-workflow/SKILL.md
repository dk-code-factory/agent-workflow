---
name: run-workflow
description: Start or manage requested durable Agent Workflow runs, inspect progress, answer approval gates, interrupt, cancel and resume safely.
---

Use the plugin's absolute `scripts/agent-workflow` launcher with `exec --` before
engine arguments. Resolve the plugin root two directories above this skill.
Use `check` and, if needed for the requested run, `ensure`. For a Claude host,
pass `--host claude` before the launcher separator; workspace `agent-runtime`
chooses the execution provider independently. Read the packaged
[run and recovery reference](../../references/recovery.md) before mutations.

Identify the workspace and requested definition or existing durable run ID.
Inspect `list`, `status`, `logs`, `attempts`, `usage` and `inputs` as appropriate.
Validate a new definition before starting it. Confirm the requested execution
scope, external effects, credentials and limits from the user's authorization;
ask only about missing material decisions. Do not author a new workflow as a
side effect of a request to execute one.

For a new authorized run, use `run --detach --workspace <repo> <definition>` and
retain its durable ID, original workspace and controller parallelism immediately.
Use that same
`--parallel` value on resume instead of relying on the CLI default of four.
After a disconnected or timed-out admission, inspect retained runs before
retrying. A disconnected client never justifies a
replacement run. Inspect the admitted definition and completed work; edits on
disk do not replace the admitted snapshot on resume.

At an input gate, show the actual prompt and pinned evidence to the user. Obtain
the requested decision, inspect the current gate again, and send one non-null
JSON answer with exact gate ID, evidence ID and unique answer ID. Do not infer
approval from elapsed time. After an uncertain response, inspect accepted
answers before delivering again. Reject stale evidence rather than rebinding it.

Use `interrupt` for a recoverable pause and `cancel` only for requested
cancellation. Resume the same run ID with its retained definitions, completed
results, deadlines, turn/iteration allowances and limits. Reconcile uncertain
effects first; `--retry-uncertain` requires explicit authorization to repeat
those effects. Never replenish allowances or force replay to obtain success.

Report run ID, state, remaining input/uncertainty and retained outputs. Waiting
(synchronous exit 2) is not success or failure. Manage only the controller and
processes owned by this task; do not restart unrelated services.
