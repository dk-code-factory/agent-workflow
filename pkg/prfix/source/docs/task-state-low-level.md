# Low-level task-state commands

Use this reference for existing integrations. Routine coordination uses [the managed interface](task-state.md). Both share contract v2 fingerprints, claims, results and budgets. Generate specs with `spec --definition FILE` using the ordinary definition fields in that contract.

## Inspect, claim and invoke

| Command | Use |
| --- | --- |
| `status --compact --run RUN_ID --spec A.json --spec B.json` | Current coverage, validated result paths, budgets and all unfinished PR claims. |
| `status --run RUN_ID` | Full progress. Read it and original results before assessing dispositions, exact acceptances or recovery. Omit `--run` when evidence spans runs. |
| `status --explain --spec A.json` | Changed inputs and other reuse blockers. Can combine with compact and run options. |
| `start-batch --spec A.json --spec B.json` | Claim tasks before invoking them or reading cached results, even when status reports reuse. |
| `start --spec A.json` | Claim one task, including tasks needing different retry or budget options. |

Without specs, status inspects runs and unfinished work. Run selection filters runs only; coverage and unfinished claims remain PR-wide. An unknown run is an error. Compact output omits progress bodies. Follow its references and validated original result paths. See [status details](status-diagnostics.md) for decoding.

Task status reports `coverage_complete` only when a retained complete result matches current inputs, has no unfinished claim or invalidation, and satisfies required native execution validation. `reusable` additionally requires task and result cacheability. CI approval may accept a noncacheable result from the current retained run/cycle, but it still requires `coverage_complete`.

For a new run, omit `--run` from status until a start actually invokes a task with that run id. Reuse alone does not create a run. Continue passing the chosen run id to starts so later invocations retain its budgets.

Both start commands return an `action` for each task:

- `reuse`: read the validated original result, including findings.
- `invoke`: save the new claim and invoke the native skill with pinned inputs.
- `recover_required`: resolve the unfinished claim before invoking this task. Other eligible tasks may proceed.

For single-task `start`, `invoke` returns `claim`, `manifest` and `changed`; use `claim.key` for the task key and `claim.id` for recording or recovery. `reuse` and `recover_required` instead include top-level `key` and `task`, plus `result` or `unfinished` respectively. `start-batch` wraps results in `tasks` and includes top-level `key` and `task` in every item, regardless of action.

Batch results preserve order and include the manifest and all unfinished PR claims. Capture, validation or budget errors save no new claims or counters. The batch captures every spec before mutation, checks claims and budgets under one PR lock, then replaces the manifest once. A lost response or storage failure leaves the outcome uncertain. Inspect status and reconcile before retrying.

## Record and recover

Run `record --spec A.json --claim CLAIM_ID --original OUTPUT --normalized RESULT.json --status complete|failed|incomplete`. For native reviewers also supply `--native-evidence OBSERVATIONS.json`, following [native reviewer handoffs](native-review.md). Preserve original bytes. Normalized results require `complete:true|false` and a `findings` list. Keep skill-owned fields. Failed and incomplete results use false. Record successful tasks immediately, even if others fail. Save stale, failed and incomplete attempts accurately. Recover explicitly when required.

Use `record --progress PROGRESS.json` for append-only outcomes, dispositions and exact user acceptances with sources. Keep progress outside governing instruction and input files. Completed analysis may contain findings; assess readiness separately. Failed, incomplete, damaged, invalidated or unfinished results never satisfy coverage.

Claims never expire. Confirm the prior agent stopped and reconcile its writes before recovery. Use `start --recover CLAIM_ID` or repeated `start-batch --recover TASK_KEY=CLAIM_ID`. Keep the original run and, for unchanged inputs, cycle. Changed review inputs require a new cycle.

To close a stopped claim without replacement work, use `invalidate --key TASK_KEY --reason TEXT --recover CLAIM_ID`. This also works when missing resources prevent recapture. Repeat `--key` to invalidate several tasks.

Follow [native reviewer handoffs](native-review.md). Save the host handle and terminal outcome in operational progress. Before recording success, the coordinator collects host execution evidence and checks skill completion. The store rejects empty or whitespace-only original output for complete tasks, including reuse of older empty results. Native review requirements are fingerprinted with resources and instructions. Reserved review responsibilities (`review` as a hyphen, underscore or colon delimited word, and `finding-verification`), explicit `native_review` tasks, and `code-review*` skill names require this contract even when omitted by the caller. Use `content-review` for configured reviewers; arbitrary generic responsibilities are not automatically classified from prose.

`native-binding --spec A.json --claim CLAIM_ID` exposes expected claim, fingerprint, skill/version/resources and original pinned Git identities for a host collector. It creates no observed events. `record` validates the supplied observations, distinct child handles, terminal outputs and required driver tool outcomes. Missing contracts or unavailable, malformed or incomplete evidence downgrade requested completion to incomplete with an execution diagnostic. Original output, evidence bytes, requested normalized fields and findings remain retained; run, retry and cycle history are unchanged. Check the returned status. Reuse applies the same gate to historical reviewer artifacts without rewriting them. The trusted coordinator must obtain actual host observations; the store does not authenticate self-authored JSON or infer semantic adherence from report text. Failed or incomplete attempts may keep empty output.
