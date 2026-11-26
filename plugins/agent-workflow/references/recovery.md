# Durable run operations

All commands below go through the installed launcher: `exec -- <command>`.
On a Claude host, use `exec --host claude -- <command>` and forward the same
`--host claude` profile to setup/check; workspace runtime selection stays separate.
Place flags before positional arguments and supply the same workspace each time.

```sh
agent-workflow validate --workspace /repo workflows/audit.yml
agent-workflow run --detach --workspace /repo --parallel 2 --input scope=src workflows/audit.yml
agent-workflow list --workspace /repo
agent-workflow status --workspace /repo RUN_ID
agent-workflow logs --workspace /repo RUN_ID
agent-workflow attempts --workspace /repo RUN_ID
agent-workflow usage --workspace /repo RUN_ID
agent-workflow inputs --workspace /repo RUN_ID
agent-workflow answer --workspace /repo --gate GATE_ID --evidence-id EVIDENCE_ID \
  --answer-id UNIQUE_DELIVERY_ID --answer-file /private/answer.json RUN_ID
agent-workflow interrupt --workspace /repo RUN_ID
agent-workflow resume --workspace /repo --parallel 2 --detach RUN_ID
agent-workflow cancel --workspace /repo RUN_ID
```

An answer file contains exactly one non-null JSON value. Inspect the current
request and evidence first. Stale evidence, duplicate deliveries, reused answer
IDs and invalidated gates are conflicts. Inspect the gate lifecycle and accepted
value after a lost response. Never blindly resubmit with a fresh delivery ID.

Waiting is durable and nonterminal. Foreground/synchronous run or resume returns
2 at a gate; a detached call returns a handle immediately. Accepted answers
queue continuation once all questions are answered and cleanup has finished.
Interrupt pauses automatic continuation while retaining gates and answers;
resume clears that pause. Cancel invalidates gates.

If the task owns an isolated controller, it may stop/restart that controller
for an approved recovery test. Inspect the same SQLite workspace afterward,
verify unchanged run/gate/evidence IDs, reject a stale evidence answer, then
answer the current gate. Verify completed preparation through receipts or a
counted fixture effect; merely seeing the final report does not prove no replay.
Do not kill a shared controller or start a competing owner.

Run admission pins definitions/configuration, agent definitions, budgets and
completed effects. A changed file is not a new admitted definition on resume.
A timed-out client does not imply a failed run. Recover the durable ID by
inspection before any retry. `needs_attention` requires examining the recorded
cause. Uncertain commands/provider turns/external effects require reconciliation
and explicit permission before `resume --retry-uncertain`. Never delete state,
reset limits, or create a replacement run to evade that safeguard.

Retain the original controller parallelism along with the run ID. A resumed
client must select the same `--parallel` value; the CLI default is four, so a
run started with `--parallel 2` also needs that value on resume. A configuration
mismatch is a diagnostic, not permission to restart the controller with new
limits. Client `--timeout` controls waiting; it does not extend an admitted run.
