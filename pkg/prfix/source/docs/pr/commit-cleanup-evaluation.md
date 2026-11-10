# Commit cleanup evaluation

Baseline: `0f927a9d01066080665be1ab7e8bd20eb38ae1d5` (refreshed main).

Evaluate each candidate against the last accepted version. Retain a candidate
only when applicable correctness checks pass and its bounded native exercise
demonstrates the stated benefit. Equal results are inconclusive and are dropped.
Dropped candidates cannot become prerequisites for subsequent experiments.

## Hypotheses and gates

| Candidate | Hypothesis | Required evidence |
| --- | --- | --- |
| 1. Separate cleanup guidance | Moving cleanup to its own document reduces instruction reads. | Fewer instruction bytes, full per-commit assessment, unchanged ownership, recovery, tree and publication safeguards; both host distributions and callers updated. |
| 2. Plan before rewriting | Explicit boundaries, source mapping and messages reduce repeated rewriting. | Valid mixed/fixup boundaries and reviewed final tree with fewer applications or a corrected boundary error; coherent series unchanged. One application and at most one concrete correction. |
| 3. Deterministic inspection helper | Read-only JSON inspection reduces agent inspection operations. | Independently checked pins, unique merge base, tree, ordered metadata, parents, messages and changed paths; no repository mutation; fewer native inspection operations; patches still read. |
| 4. Reuse unchanged assessments | History-sensitive assessment specs avoid duplicate semantic work. | Zero new assessments on unchanged rerun; message, history, target and governing-context invalidation; current lease/publication checks; resulting-series coverage after rewrite. |
| 5. Validate according to inputs | Exact executable-input evidence avoids redundant local tests. | Content-only test reused; history-dependent, changed and incomplete evidence rerun; final tree and affected boundaries checked; fresh CI for rewritten published heads. |
| 6. Prepared and blocked outcomes | Explicit outcomes prevent false publication completion. | Accurate preparation-only completion, blocked always incomplete, publication-required preparation incomplete, interrupted publication reconciled without duplicate push; no-op and published cases preserved. |

The helper must not fetch, create recovery refs or rewrite history. Ambiguous
history or ownership blocks rewriting. Outcome changes are local to cleanup and
do not change the global task-state schema.

## Protocol

Use real temporary repositories, isolated worktrees and local remotes. Run one
fresh before/after native-agent pair per candidate and one final combined pair,
at most 14 fresh runs. Keep observable models/settings equal. Agents receive
actual guidance and raw task artifacts without expected answers or prior findings.
Retain guidance hashes, assignments, original outputs, complete success/failure
traces, repository snapshots and receipts privately outside the repository.

Measure instruction bytes read, inspection/Git calls, rewrite applications, test
executions and assessed outcomes. These are bounded observations, not token or
general speed measurements. No open-ended tuning: drop failed or inconclusive
candidates. Commit a sanitized, reproducible results summary.

Cover coherent no-ops, mixed commits, fixups, unchanged reruns, history-only
changes, dirty worktrees, ambiguous ownership, changed target/lease, tree mismatch,
incomplete test evidence and interrupted publication. Check interactions in the
final pair; remove the responsible candidate on failure and rerun affected
deterministic checks.

## Decision ledger

All six candidates and the final combined pair are complete. Retain 1, 3 and 6;
drop 2, 4 and 5. No final interaction failure required removing a retained change.

| Candidate | Decision | Evidence and limitations |
| --- | --- | --- |
| 1 | Keep | Native instruction reads: 15,161 → 13,750 bytes. Both arms assess all nine source commits, preserve the reviewed trees, keep the coherent series, and block dirty/mixed-author rewrites. Recovery refs and unchanged remote leases independently verified. One pair; application and test-count differences are not attributed to document separation. |
| 2 | Drop | Both fresh arms use one application, produce the same valid three-commit result, and retain the coherent series unchanged. No demonstrated application reduction or boundary error corrected; equal outcomes are inconclusive. Proposal removed before candidate 3. |
| 3 | Keep | Native inspection operations: 48 → 32; recorded Git calls including helper internals: 69 → 67. Agents read source and resulting patches and preserve trees and coherent history. Twelve deterministic checks include a later completeness refinement rejecting shallow history and disabling implicit promisor fetches; the combined pair uses that final helper. These are operation counts, not speed or token claims. |
| 4 | Drop | Both arms perform zero new semantic assessments on the unchanged event and invalidate after message, history, target and context changes. Both inspect the changed remote lease and block future publication. Existing task-state guidance already supplies the behavior; one fewer overall assessment in the after arm reflects an extra initial content/history probe in before, not an unchanged-rerun benefit. Proposal removed. |
| 5 | Drop | Each arm executes one content test and one history test. After rejects the invalid receipts and reuses fresh content evidence across matching boundaries, but the controlled exercise demonstrates no avoided execution versus before. No test savings claimed. A malformed application in after is traced and recovered. Proposal removed. |
| 6 | Keep | Before cannot represent completed preparation (beta: null outcome, incomplete); after returns prepared/complete. Publication-required theta remains prepared/incomplete; dirty gamma becomes blocked/incomplete. Both reconcile the original interrupted push with one original send. After retains its verified receipt but blocks completion without fresh CI evidence. Successful published paths remain covered by the production publication and full-recipe suites; native reconciliation alone does not establish CI readiness. |

## Completion gates

Commit surviving candidates separately with tests and decision evidence. Retain
shared evaluation support separately only when surviving experiments need it.
Run focused helper/state/publication tests and the complete offline suite under
Python 3.12, package/link/whitespace/message/changed-skill checks and host parity.
Use version 3.3.0 if the CLI or expanded outcomes survive, otherwise 3.2.2 for
guidance changes; do not bump if every candidate is dropped. Update the PR to the
actual scope, push, and verify final-head CI. Leave it open and draft if all
candidates are dropped or required validation remains incomplete. Never merge.

## Measured sample and limits

Exactly 14 fresh native Codex agents ran without inherited conversation or
findings. Each pair received the same assignment except its arm path. Model and
effort were inherited without overrides; resolved settings were not independently
observable. Source, interface and guidance hashes and original assessment hashes
are in the [sanitized report](../evidence/146-commit-cleanup-evaluation.json).
Full semantic outputs, traces, Git repositories, receipts and recovery evidence
remain private outside the repository. No agent read this ledger or prior
findings. Transport capture of source hashes does not expose their contents.

| Native metric | C1 before/after | C2 | C3 | C4 | C5 | C6 | Combined |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Instruction bytes read | 15,161 / 13,750 | 12,754 / 7,114 | 12,754 / 7,253 | 14,995 / 23,414 | 7,253 / 21,500 | 23,462 / 28,115 | 28,230 / 37,174 |
| Inspection operations | 75 / 69 | 35 / 25 | 48 / 32 | 18 / 32 | 18 / 28 | 63 / 79 | 109 / 97 |
| Recorded Git calls | 95 / 93 | 55 / 46 | 69 / 67 | 74 / 93 | 41 / 62 | 147 / 182 | 178 / 230 |
| Rewrite applications | 2 / 1 | 1 / 1 | 1 / 1 | 0 / 0 | 1 / 2 | 2 / 2 | 2 / 2 |
| Local test executions | 2 / 8 | 4 / 4 | 4 / 4 | 3 / 2 | 2 / 2 | 8 / 8 | 6 / 14 |
| Recorded assessments | 9 / 5 | 4 / 4 | 3 / 4 | 7 / 6 | 2 / 3 | 7 / 8 | 16 / 15 |
| Failed operations | 5 / 4 | 0 / 1 | 3 / 2 | 0 / 1 | 1 / 2 | 0 / 0 | 8 / 0 |

These are single observations. The combined after arm consumes more instruction
bytes, recorded Git calls and local tests, while using fewer inspection
operations. There is no demonstrated overall instruction, Git, token, latency or
production speed improvement. The retained benefits are the isolated C1/C3 gates
and C6's accurate completion semantics. Unrelated metric differences do not
rescue dropped candidates.

Instruction bytes count successful guidance reads, including repetitions.
Historical inspection counts use the fixed mutation-exclusion list recorded in
the sanitized report. Review found that other mutation spellings could be counted;
there is no evidence they occurred in the original runs. The retained transport
now counts recognized read-only commands and explicit configuration queries;
unknown or ambiguous forms are excluded. Historical interface restoration retains
the original counter for reproducing the reported experiment. Recorded Git calls include helper
internals but exclude Git inside test bodies and environment-version probes.
Applications include failed dispatches. Assessments count explicit saved semantic
records, including original/resulting-series and safety work; they cannot measure
unobserved reasoning. Setup tests and the original interrupted send are excluded.

C1-before and C5-after recovered malformed application requests. Expected missing
Git settings and other failures are retained in traces. Combined-before recovered
an omitted-mode content claim and then used explicit history capture. The retained
transport now rejects missing modes before claiming, with a regression check.
The final pair additionally archives every returned body and immutable test
receipts; earlier pairs retain original semantic outputs and operation metadata.

Every arm's final committed tree equals its reviewed tree. Coherent alpha remains
unchanged; dirty gamma and mixed-author delta retain their original heads and
edits. Beta/theta rewrites retain recovery refs and leave their remote heads
unchanged. Epsilon's lease movement is an intentional fixture event. Both final
arms reconcile zeta's original send with no duplicate. After retains its verified
receipt but blocks completion without fresh CI. The offline publication and full
recipe tests separately cover successful published completion and exact-head CI.
The native exercise does not simulate authenticated CI.

## Replay

Create source directories from the baseline, outside the checkout. The
[replay bundle](../evidence/146-commit-cleanup-replay.json) contains
`accepted_source_patches` for the original C1, C3 and C6 code and guidance,
independent of later rebases and review fixes. Apply them in that order as needed,
starting from the baseline; the evaluation ledger is intentionally excluded.
The bundle also contains dropped proposals and historical interface/helper
restoration patches. Apply each candidate only to its last accepted source:
C1 uses baseline; C2/C3 use C1; C4/C5/C6 use C1+C3. The combined pair compares
baseline with C1+C3+C6. Dropped guidance is never active in later arms.

```sh
python3.12 tests/fixtures/commit_cleanup_exercise.py --root "$BEFORE" init "$BEFORE_SOURCE" 1
python3.12 tests/fixtures/commit_cleanup_exercise.py --root "$AFTER" init "$AFTER_SOURCE" 1
```

Use candidate IDs 1–6 or 0 for the combined scenario. For C3’s isolated after
arm, copy the current retained `scripts/commit_series.py` into the reconstructed
C1+C3 source, then apply `helper_restore_patch_for_isolated_pair`. Its resulting
bytes must match `historical_helper_sha256`. The combined after arm uses the
helper from the original accepted C3 patch, without that isolated restoration.
To reconstruct historical transport behavior, copy the retained exercise fixture
into each source, then apply its selected `interface_restore_patches_from_retained`
patch before initialization. Keep the copied interface and guidance frozen
throughout each pair. Use the same model/settings and fresh native agents without
previous outputs. Give each agent only its arm and this assignment:

> Use only `python3.12 ARM/frozen/interface.py --root ARM COMMAND ARGS` for every
> read or action. Read exercise.json, the actual installed cleanup skill and
> required references, and every tasks/CASE.json. Perform the assignments using
> real Git through the interface. Choose guards, patches, boundaries, mapping,
> messages, applications, tests, assessments and event handling yourself. Preserve
> recovery evidence before mutation. Do not publish preparation-only assignments,
> modify frozen resources, use a network or launch agents. Record each semantic
> assessment through assess or managed state; finish with full structured case
> results and summarize. Report actual coverage, safety, publication and blockers
> candidly, without inventing unsupported outcomes.

Compare the saved original results and complete source/resulting patches before
interpreting counters. `summarize` recomputes counts from traces; independent Git
reads verify trees, refs, ownership and unchanged remotes. Hashes detect artifact
changes. Native operation choices and therefore counts can vary on replay.

## Final validation

Python 3.12.13: **722 tests and seven subtests pass** through the complete offline
suite, including 12 helper and six exercise-transport checks. Focused state and
publication suites also pass (171 tests). Package, base-aware release, links,
host parity, changed-skill, whitespace and message-length checks pass. Release
metadata is synchronized to **3.3.0**.

One full-suite repeat hit the existing linked-worktree process-count assertion
(35 calls instead of 28). Its log is retained. An unchanged complete rerun passes;
no state implementation or assertion was changed to obtain that result.

After the evaluation, main advanced with the 3.3.0 finalizer release. The PR
was rebased onto that release; its synchronized package version is now 3.4.0.
The measurements and validation counts above describe the original experiment.

Review follow-up also isolates repository-local Git environment variables and
ignores legacy graft ancestry. Four regression cases fail on the pre-fix helper
and pass with the correction; the helper suite now has 16 checks. This adds one
`rev-parse --local-env-vars` query per inspection after the measured experiment.
The original operation counts above are historical, not a new helper benchmark.

A second review follow-up forces UTF-8 Git log output so repository encoding
settings cannot break metadata framing. The helper suite now has 18 checks.
Seven replay checks rebuild source guidance from the fixed baseline and apply
every historical interface patch, the three dropped proposals, and the isolated
helper restoration with its recorded resulting hash. The experiment was not rerun.

A third review follow-up uses Git’s explicit `--no-lazy-fetch` option, requiring
Git 2.45 or newer and making older Git fail before inspection. The helper suite
now has 19 checks; the retained transport has 14 exercise/replay checks. Mutation
counting is verified with real branch/configuration writes and queries. All seven
restored historical interfaces match their original recorded SHA-256 values.

Main subsequently added the 3.4.0 commit-policy release. The PR preserves that
policy resolution while moving cleanup guidance and enforcing outcome semantics;
its current synchronized release version is 3.5.0. Historical source patches
continue to reconstruct the original experiment independently of this rebase.
