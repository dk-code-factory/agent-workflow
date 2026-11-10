# Workflow improvements measured by dollar cost

Start from remote main `88f81954c1415b118d95ef16c24196bd4bc49201` in an isolated worktree. Leave the original checkout and PR #145 unchanged. Open a new draft PR with this bootstrap plan before implementation; preserve individual commits and leave it unmerged.

Import only the test usage reader and tests from PR #145, with provenance. Keep its historical token gate unchanged. Add a separate API-equivalent dollar evaluator using a frozen, dated official rate card. Never describe estimates as account billing.

Evaluate these proposals in order, each against the latest accepted implementation:

1. **Bookkeeping interfaces:** validated JSON requests delegating to existing task operations, consistent response envelopes, supported native-handle and progress storage. Exercise definition, claim, handle retention, recording, reuse and invalid requests against existing CLI operations and agent-written wrappers.
2. **Structured resume:** read-only packet with retained runs, specs, claims, handles, blockers and evidence references. Missing governing references stay explicit. Exercise unchanged reuse, selective invalidation and unfinished-claim recovery while retaining counters and permissions.
3. **Selective progress retrieval:** cursors and task-specific evidence, preserving compact/full views and complete original artifacts. Exercise large history, exact acceptances, appended progress and accurate coverage.
4. **Durable CI receipt:** private observer identity, retained deadline/state and final evidence through existing ownership and locking. Exercise pending checks, interruption/resume and terminal consumption; head changes, denied access, timeout, no checks and failed checks never count as success.
5. **Command discovery:** concise versioned command/schema reference generated from parser definitions with valid ordinary, hook and finalizer examples. Exercise discovery and representative operations against existing help/source inspection.

Keep commands compatible. Helpers do bookkeeping, never execute workflows or make review judgments. Each workload uses real production helpers, isolated Git repositories and controlled local GitHub transports. Native coordinators choose operations; fixtures provide inputs and verify outcomes without choosing the next action.

For each proposal: implement, run focused correctness checks, freeze both arms, measure three pairs in baseline/candidate, candidate/baseline, baseline/candidate order, then accept or decline. Freeze runtime, guidance, resources, assignment and scenario independently for each arm; intended candidate runtime changes are permitted. Keep model, effort, service tier and execution conditions equal. Observe cache usage without changing personal settings.

Measure initial assignment through terminal assessment, including attributable preparation, waiting, result consumption and all descendants. Globally deduplicate response IDs and verify independent participant, turn and terminal inventories. Reasoning is part of output, never an extra charge. Preserve unfavorable completed trials; missing evidence is inconclusive and forbids adoption.

For three baseline costs B and candidate costs C, accept only with complete telemetry and passing correctness, median(C) <= 0.95 * median(B), sum(C) < sum(B), and at least two C[i] < B[i]. Token reductions are diagnostic only. Cover exact 5%, smaller savings, equality, total regression despite improved median, insufficient improving pairs, duplicate history, missing descendants and incomplete telemetry.

Immediately commit and push every accepted improvement with tests, synchronized guidance and normalized evidence. Include a synchronized patch release in the first accepted production change. Remove declined production changes before the next experiment and retain measurements/reasons. A proposal must work without previously declined proposals.

Track implementation, review and measurement spending separately from recurring workflow cost, report cumulative spending, and impose no fixed dollar ceiling. Preserve original findings/acceptances, scoped preferences, claim ownership, recovery, budgets and duplicate-publication safeguards.

After accepted changes synchronize both hosts and run package, full offline, whitespace and base-aware release checks. If any proposal passes, run three more pairs comparing the accepted bundle with the original baseline using the same gate. A failed combined gate preserves accepted commits and leaves the draft explicitly unready. Expected scope: 30 individual native arms, plus six combined arms if anything is accepted.

Finish with exact-head CI and rewrite the PR using Issue, Fix, Validation and Decisions, listing accepted, declined and inconclusive proposals, individual and combined costs, cumulative spending and limitations. No merge or squash is authorized.
