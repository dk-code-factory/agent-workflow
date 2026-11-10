# Native dollar experiment record

The [plan](dollar-experiment-plan.md) defines five independent candidates and the dollar acceptance gate. Experiments use test-only fixtures, frozen production helpers and fresh native coordinators. Production adoption requires complete telemetry and correctness plus the dollar gate; incomplete telemetry is inconclusive.

## Provenance and accounting

`tests/fixtures/native_usage.py` and `tests/test_native_usage.py` are imported byte-for-byte from PR #145, commit `c9c2a181e72f7a3673013e9a6c901d5347918240`. No rejected production guidance, runtime changes or historical decisions are imported. The historical token evaluator is unchanged. `tests/fixtures/dollar_usage.py` supplies the independent dollar evaluator.

The rate card was verified on 2026-11-05 against the [official model page](https://developers.openai.com/api/docs/models/gpt-6.1-sol) and [official pricing](https://developers.openai.com/api/docs/pricing). Standard USD per million: input 2, cache reads .10, 30-minute cache writes 2.50, 12-hour cache writes 5, output 10. Above 272,000 input tokens, input/cache rates double and output is multiplied by 1.5 for the complete response. Fast/priority is 2x, ultrafast 6x, batch/flex .5x; regional processing adds 10%. Reasoning is already part of output. Estimates are API-equivalent costs, separate from account billing.

Freeze runtime, guidance, resources, scenario, assignment and execution conditions before the first trial. Keep all completed arms. Private artifacts retain original assignments, terminal messages, event streams, rollouts and source references. Public reports normalize paths and retain hashes, correctness outcomes, cost evidence and limitations. Observed cache usage is never manufactured.

## Verification

Accounting tests cover duplicate parent history and complete descendants, missing responses/turns/terminals, cache/context/tier charges, exact 5% savings, smaller savings, equal costs, total regressions and insufficient improving pairs. Focused candidate checks precede native measurement. The initial focused run passed 237 tests before two test setup errors from disk quota; the same complete focused selection then passed all 239 tests using disposable shared-memory test output. Neither attempt was a performance trial.

## Decisions after host-context audit

No proposal currently has valid adoption evidence. Proposals 1–4 are inconclusive after the full starting-context audit. Proposal 1 lacks complete response telemetry. Proposals 2–4 have varying host base-instruction metadata; proposals 3–4 also have varying effective initial messages, including skill catalogs. Proposal 2 has matching effective messages but its different base metadata prevents proving a pinned starting context. Do not dismiss that uncertainty as unrelated to the workload. The initial resource freeze covered production files, inputs and personal configuration, but omitted these native host inputs.

The initial acceptances of proposals 2 and 4 are withdrawn. Their original commits and numerical gate results remain in history and normalized evidence, and their source/tests are archived as patches. Active production changes and the provisional patch release are removed. No completed trial is rerun. The separate dollar evaluator now requires the host-context audit to pass.

### Initial numerical decisions, retained for provenance

Proposal 1 is inconclusive: all six native correctness checks pass, but the original WebSocket transport lacks independent response inventories. Preserve all arms; no production change is adopted. Observed medians are $0.1619104 baseline and $0.1539644 candidate (4.91% savings, below the dollar threshold independently of incomplete telemetry). See [proposal 1 evidence](evidence/151-experiment-1.json) and the archived candidate patch under tests/fixtures/dollar-candidates/.

Proposal 2 initially passed the numerical/correctness gate (acceptance now withdrawn): all six native arms pass correctness and complete independent SSE/host/rollout inventories. Baseline costs are $0.2143820, $0.2183528 and $0.2303840; candidate costs are $0.1901388, $0.2310276 and $0.1873048. Median savings are 12.92%, total cost falls from $0.6631188 to $0.6084712 and two pairs improve. See [proposal 2 evidence](evidence/151-experiment-2.json). The second pair's regression is retained. First accepted production change synchronizes the patch release to 3.5.1.

The resume packet was exercised against the original production baseline, without proposal 1. Its focused Python 3.12 suite passes 225 tests. Accounting, collection and frozen fixture construction pass 84 checks, including missing creation/completion inventories, unexpected descendants and unknown cache-write TTL. This initial validation did not establish matching native host metadata and therefore cannot establish adoption or bundle readiness.

For proposal 2 and later, an invocation-only provider with the same OpenAI endpoint/authentication and model disables WebSocket capability so raw SSE creation/completion inventories can be independently retained. Personal settings are unchanged. Both arms use the same gpt-6.1-sol/high/default controls and transport. Proposal 1 remains under its original frozen transport and is never rerun.


Proposal 3 is declined. Baseline costs are $0.1271160, $0.1662800 and $0.1637432; candidate costs are $0.1624512, $0.1570100 and $0.2054872. Median cost changes from $0.1637432 to $0.1624512 (less than 1% savings), total cost rises from $0.4571392 to $0.5249484 and only one pair improves. All telemetry is complete. Three reports declare complete:false while accurately identifying the expected partial coverage; the strict completion check also does not pass. Preserve those declarations, all original reports and [proposal 3 evidence](evidence/151-experiment-3.json). The source and seven focused candidate tests are archived as a patch; active production changes are removed. Focused runs passed 167 and 83 tests, with overlap.

Subsequent assignments explicitly distinguish completion of the bounded exercise from overall workflow coverage. This clarification applies equally to both arms and does not rerun or alter earlier trials. Combined validation is prepared to exercise exactly the accepted proposal set, with the original baseline as its comparison. No completed measurement is retried.


Proposal 4 initially passed the numerical/correctness gate: baseline $0.2566360, $0.1955628 and $0.2124984; candidate $0.1490952, $0.1933976 and $0.2095708. The median fell from $0.2124984 to $0.1933976 and all three pairs improved. Its 103 focused checks and subsequent complete offline run (919 tests plus seven subtests) passed. The later host-context audit invalidates adoption despite those favorable numbers; see [proposal 4 evidence](evidence/151-experiment-4.json).

Proposal 5 is declined with complete telemetry, matching observed starting contexts and passing correctness in all six arms. Baseline costs are $0.2367000, $0.1798040 and $0.2604020; candidate costs are $0.2843668, $0.2376092 and $0.2439592. Median cost rises from $0.2367000 to $0.2439592; total cost rises from $0.6769060 to $0.7659352 and only one pair improves. Its 180 focused tests passed. See [proposal 5 evidence](evidence/151-experiment-5.json). Its source and tests remain archived; no production reference interface is adopted.

## Final scope and readiness

All 30 individual arms completed in their original alternating order. No completed arm was rerun or replaced. Final decisions: proposals 1–4 inconclusive; proposal 5 declined; none accepted. The six conditional combined-validation arms are not triggered because there is no valid accepted bundle. This is not a successful combined validation or a readiness claim.

The initial favorable decisions and their commits remain intact, followed by an explicit withdrawal commit. All five candidate patches are retained under tests/fixtures/dollar-candidates/. The provisional production helpers and patch version were removed. Upstream main advanced to `af9123e21de114799ab5673f20ae9464edeb059c` (3.6.0) and was merged without rewriting experimental history. Final production files match that upstream state; original checkout and PR #145 were not modified.

The [spending ledger](evidence/151-spending.json) separates native recurring trials from coordinator implementation, review/validation, measurement and mixed/unallocated overhead. It globally deduplicates response IDs and preserves an explicit active-coordinator cutoff. It is observed API-equivalent spending, not a complete account bill. Experiment 1 and coordinator overhead retain their stated telemetry limitations. There is no fixed dollar ceiling.

Final offline validation passes 969 tests plus seven subtests on Python 3.12, with synchronized hosts, package, whitespace and base-aware release checks. All five archived patches apply to their recorded baselines. See [validation evidence](evidence/151-validation.json). Exact-head CI is recorded in the PR description; CI is checked after the last evidence commit so its head remains exact. Keep the PR draft and unmerged.
