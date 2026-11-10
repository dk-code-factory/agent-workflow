# Measured workflow token experiments

Run these experiments in dk-workflows only, from remote main at
`0f927a9d01066080665be1ab7e8bd20eb38ae1d5`. The existing branch and PR #142 are outside scope.
This plan is the bootstrap diff; open a draft PR before implementation.

## Sequence

Compare each candidate against the latest accepted state, in this order:

1. Coordination: one coordinator, no relay coordinators or repeated status
   messages; use native completion notifications and the existing CI waiter.
   Preserve completion, interruption and failure handling. No runtime waiting
   features are in scope.
2. Review cycles: check coverage before dispatch, reuse unchanged complete tasks,
   refresh affected tasks on changed inputs, and finish when required coverage
   and finding dispositions are complete. Preserve required reviewers and the
   five-cycle limit.
3. Separate agents: compare continuing work with a fresh native agent for bounded
   investigation, review and verification; keep brief status operations inline.
   Include parent preparation, waiting, consumption and all child usage. Compare
   short and long existing contexts; adopt only task classes that pass. Leave
   compaction settings unchanged.
4. Focused handoffs: compare inherited history with fresh task-specific
   instructions referencing complete immutable evidence. Preserve exact governing
   instructions, preferences, scope, permissions, source access and reports.
   Return findings and evidence references without intermediate log replay.
   Missing evidence requires retrieval or an incomplete result.
5. Reasoning effort: compare high with medium for routine bounded work, and low
   for routing/status, using supported native per-task controls. Preserve explicit
   model/effort requirements. Reject unsupported or inconclusive changes. Do not
   change personal settings or add a model CLI launcher.

For every experiment: implement, run checks and three paired native trials,
obtain a fresh independent correctness review, then accept or reject. Immediately
commit and push accepted changes with tests and normalized evidence. Remove
rejected implementation changes and record reasons in the PR. Preserve individual
commits and do not merge automatically.

## Measurement and acceptance

Extend the native exercises with a test-only usage reader, not an execution
engine. Pin source, guidance, scenario, model, effort and service tier. Alternate
baseline/candidate order across three pairs per scenario. Deduplicate response
IDs, include every participating descendant, and separately report input, cached
input, cache writes, output and reasoning; reasoning is already part of output.
Report raw tokens, fresh input/output changes, coordination calls and estimated
API-equivalent cost using documented model prices and context multipliers:
<https://developers.openai.com/api/docs/models/gpt-6.1-sol>.
Estimates are distinct from actual billing. Raw conversations and traces stay
private; commit normalized measurements, source pins, checks and decisions.

Accept only when all correctness checks pass, median total tokens fall at least
10%, at least two of three pairs improve, estimated median cost does not rise,
telemetry is complete, and no actionable review finding remains. Reject lost
findings, weakened coverage, broken recovery or insufficient evidence. Three
trials provide bounded evidence, not a general performance guarantee.

## Invariants and completion

Preserve reviewers, finding coverage, preferences, retained budgets, recovery,
public commands, configuration schemas and private state formats. Update shared
guidance and synchronize both host distributions. Include the synchronized patch
version bump in the first accepted shipped change.

Cover unchanged reuse, selective invalidation, required findings, incomplete
optional work, interruption/resume, failed children, duplicate-launch prevention,
retained counters and final-head CI. Test usage accounting for duplicates,
inherited history, missing telemetry and parent/child aggregation.

Run focused regressions per candidate, then the full bundled suite, package
checks, distribution synchronization, whitespace checks and base-aware release
validation. Finish with final-head CI and a draft PR description covering accepted
changes, rejected proposals and limitations.
