# Bounded native iteration exercise

Issue #117 changes coordinator guidance. A scripted integration test cannot establish that a native agent follows it. This opt-in exercise compares two fresh native coordinators on one equivalent, offline resumed task. Native agents choose their reads, claims, task invocations and recordings; the fixture does not choose the next operation.

The runtime and original guidance are pinned at `620f8df51b1066e44faeb69878235adcd3c49317`. The after arm substitutes the changed guidance; both arms use the same runtime, Git commits, source text, selected fixture skills and starting task state. The frozen runtime predates the Git-capture optimization in #115; these observations do not measure or verify that optimization. Repeat the exercise against the merged prerequisite runtime before attributing a result to combined helper changes.

## Scenario and boundaries

The retained run has one completed review cycle, a completed content reviewer with a source-backed accepted finding, another completed content reviewer, a completed history reviewer and an incomplete optional reviewer. The checkout has an identical tree after a commit-message rewrite, and only the second content reviewer's installed resource has changed. The coordinator assesses selective reuse, refreshes affected reviews, retains the stable run and counters, reads original finding evidence, and reports optional incompleteness separately. Partition and cleanup use the same installed fixture skill with distinct task identities. CI and publication are noncacheable fixture tasks.

This is a bounded resumption exercise, not a full native end-to-end PR iteration or a trial of external reviewer implementations. Domain invocations are offline stand-ins. Fixture collection and verification compare complete local source bytes; they are not GitHub pagination or production supplied-evidence compatibility trials. Publication returns a reconciled no-op and performs no remote writes. The existing complete-recipe integration test separately covers domain transports, CI retries, fixes, interruption and duplicate-publication controls.

## Reproduce

From this checkout, initialize two new directories outside repositories:

```sh
python3 tests/fixtures/native_iteration_exercise.py --root "$EXERCISE_BEFORE" init before
python3 tests/fixtures/native_iteration_exercise.py --root "$EXERCISE_AFTER" init after
```

An optional revision after `before` or `after` selects the same frozen runtime for both arms, for example `init before RUNTIME_SHA`. Guidance before remains pinned at the recorded original revision.

Use separate fresh native agents, without inherited conversation or findings. Give both agents the same assignment below, substituting only their arm directory. Do not run this through another model CLI or a scripted coordinator. Resolve model and effort through the host; retain the actual host settings if observable, otherwise report them as unknown. Keep the initialized directories and original agent assignments/results privately.

> Use only `python3 ARM/frozen/interface.py --root ARM COMMAND ARGS` to read or act on this offline fixture. Read the fix skill: `guidance/codex/skills/dk-pr-fix/SKILL.md` in the current arm, or `guidance/codex/skills/dk-pr-iter/SKILL.md` in the historical arm. Read its references, `exercise.json`, `pins.json` and original governing evidence as needed.
>
> Resume run `exercise` after the tree-preserving rewrite and reviewer resource change. Assess required coverage, finding dispositions with sources, separate partition and cleanup roles, CI for the exact head, and publication freshness. Refresh required tasks. Report incomplete optional tasks. Keep run, cycle and retry counters.
>
> Do not modify the checkout, resources or specs, reset state, use a network or launch agents. Choose the operations yourself. Finish with an accurate structured assessment.

The interface has these operations:

| Operation | Result |
| --- | --- |
| `read PATH` | Full text inside this arm, including frozen guidance, specs and retained original results. |
| `resolve` | Selected fixture skill identities and installation availability. |
| `collect` | Complete local source artifact and receipt with hash, size and head. |
| `verify` | Compare current fixture source bytes with that receipt. |
| `state ARGS` | Invoke the frozen production state CLI; `--spec` takes a task name from `exercise.json`. |
| `invoke TASK CLAIM` | Produce that task's offline domain output, requiring its live claim. |
| `finish JSON` | Save the native coordinator's assessment. |
| `summarize` | Print observed operation counts, byte sizes, retained counters and saved assessment. |

For `record`, use the invoked fixture output path as both `--original` and `--normalized`. A false `complete` must be recorded as incomplete. Non-review tasks use the retained run without allocating a review cycle. An incomplete optional result can remain incomplete; it cannot be relabeled complete. Before assessing a reused finding or recovery, read full retained progress and referenced original results. Preserve collection receipts separately from source artifacts.

After initialization, run the copied `ARM/frozen/interface.py` for every operation; do not edit it or the frozen guidance while either arm runs. Both `exercise.json` files retain its SHA-256.

Operations dispatched within an initialized arm append their arguments, success flag and returned byte count to private `trace.jsonl`, including expected argument, data, filesystem and helper failures. Command-line parsing and initialization failures are outside this trace; initialization is fixture setup, not a native invocation. Compare trace summaries and original assessments, then inspect source hashes, counters, normalized task outputs and blockers before stating outcome equivalence. Account for additional manual or host operations separately. Returned byte counts exclude the printed newline. Summaries do not prove that a read was understood or that all unobserved tools were unused.

## Observed sample

The committed [sample report](evidence/117-native-exercise.json) records one controlled pair of fresh Codex native agents with identical frozen interface, runtime, Git and source pins. Its historical interface SHA-256 is `1a39c8ceec3576c977338ddf8902fe67b0791b869ffc2f7a66e6bb757db62026`, predating the missing-operand tracing correction. An earlier exploratory pilot is excluded. It separates observed coordinator reads, collections, state calls, task invocations and output bytes from unavailable token counts, internal context reads, wall-clock attribution and model settings. Reported bytes describe this fixture's tool returns; they do not measure model context size or production performance. One pair does not establish general efficiency or reviewer recall.

Both controlled arms produced equivalent normalized task results, source-backed F1 acceptance, receipt contents and counters: two cycles, no retries/repairs/reruns, six domain invocations, and the same incomplete optional blocker.

| Observed interface metric | Before | After |
| --- | ---: | ---: |
| Successful instruction reads | 10 | 12 |
| Successful other reads | 12 | 14 |
| Collection calls | 2 | 2 |
| Freshness verifications | 1 | 7 |
| State-helper calls, including errors | 14 | 16 |
| Domain invocations | 6 | 6 |
| Failed operations | 3 | 3 |
| Returned bytes, including errors | 48,280 | 69,397 |

The after arm issued the cached-result start guard for reviewer A; the before arm relied on status alone. Both had already read retained evidence during inspection, so the sample does not prove the complete ordering of result consumption. Both retained stage outcomes in task results and their assessments without appending new progress entries. These observations remain adherence limitations. The after arm returned more bytes and performed more freshness checks; no efficiency, token or wall-clock improvement is claimed. The report includes normalized operation lists so the counts can be recomputed.
