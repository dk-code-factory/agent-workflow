# Native token experiments

The [experiment plan](token-usage-experiment-plan.md) defines the ordered
candidates and acceptance gate. These are test-only exercises, not a production
execution engine or model launcher. Native agents choose their actions from
frozen workflow guidance. Offline task/domain results remain stand-ins; full
recipe regressions separately cover recovery and publication behavior.

## Integration with current main

PR #151 already imported the usage reader and its tests from the original
PR #145 head, `c9c2a181e72f7a3673013e9a6c901d5347918240`. Against main
`7240325eed0bc8cdd39ac139743435f1c9aff8bb`, this PR adds the historical
experiment records and independent runtime/guidance fixture coverage. Production
files and release metadata retain main's version 3.7.2. No production token
optimization is adopted.

Original experiment commits and their source pins are preserved on
`archive/pr-145-token-experiments`. The rebase does not rerun the trials or change
their decisions. Validation below describes the original integrated revisions;
validation of the rebased PR is recorded separately in its description and CI.

## Measurement protocol

Use new external arm directories for every trial. The iteration exercise accepts
`init before RUNTIME_SHA GUIDANCE_SHA` to freeze a baseline independently of the
runtime. `init after RUNTIME_SHA GUIDANCE_SHA` freezes checkout guidance. Pin the
interface, runtime dependencies, source, guidance and native assignment before
launch. Preserve actual host model, effort and service tier. The three execution
orders are baseline/candidate, candidate/baseline, baseline/candidate. Compare
each experiment with the latest accepted guidance, including prior accepted
changes only. Keep original assignments, conversations, telemetry and traces
private. Committed reports contain hashes, normalized counts and assessments.

Read per-response `token_usage_record` records with the test-only
`tests/fixtures/native_usage.py`; cumulative usage events are not additional
responses. Account for the measured parent turns and every descendant, including
handoff preparation, waiting and result consumption. Inherited conversation
records do not represent newly billed responses: deduplicate response IDs
globally and select only the explicit measured thread/turn scopes. Never infer
complete telemetry merely from the records that happen to be present. Retain
native terminal outcomes and reconcile expected response/descendant inventories
with execution evidence. Missing telemetry or unresolved native work prevents
acceptance.

Input includes cached reads and cache writes. Report those separately and
subtract both to calculate fresh input. Output includes reasoning; report
reasoning separately without adding it again to total tokens or output cost.
Cost estimates apply to each response before aggregation so long context
multipliers are not applied to an entire run by accident.

The pricing basis, retrieved 2026-11-05, is the official
[GPT-6.1 Sol model page](https://developers.openai.com/api/docs/models/gpt-6.1-sol).
Standard rates per million tokens are input $2, cached input $0.10, cache writes
$2.50 and output $10. Above 272,000 input tokens, apply 2× to input/cache rates
and 1.5× to output for the complete response. Fast rates are 2× standard,
ultrafast 6×, and batch/flex 0.5×. These are API-equivalent estimates, not account
billing; subscription usage has no long context surcharge. Unknown controls or
pricing prevent an acceptance claim.

All correctness checks and a fresh independent review must pass. Require at
least 10% median paired total-token reduction, improvement in two of three pairs,
and no median estimated-cost increase. Preserve findings, required coverage,
preferences, recovery, counters and final CI. Record limitations even when the
gate passes: three pairs establish bounded evidence for the measured scenario.
Reject unsupported controls, missing evidence and inconclusive candidates.

## Experiment 1: coordination

The [three-pair report](evidence/145-experiment-1.json) rejects the additional
single-coordinator/notification guidance. Median total usage increased from
805,422 to 1,641,527 tokens; no pair improved. Median API-equivalent estimated
cost increased from $0.2299976 to $0.3575996. The candidate guidance was removed.

All six runs preserved required findings, selective refresh and retained state.
One candidate retried an unavailable optional provider without resolving its
blocker; strict native adherence therefore failed independently of performance.
Failed exploratory commands and progress-write attempts remain included in the
measurements. Both arms prohibited relay agents, so the sample cannot establish
the benefit of removing actual relay coordinators. The normalized report retains
the rejected patch as nonexecuting evidence; raw traces remain private.

## Experiment 2: review cycles

The [three-pair report](evidence/145-experiment-2.json) rejects the additional
coverage-preflight and stage-completion guidance. Median total usage increased
from 674,573 to 852,207 tokens; one pair improved. Median estimated API cost rose
from $0.2015252 to $0.2329612. The candidate guidance was removed.

Independent review reproduced all raw accounting and verified retained findings,
selective refresh, required coverage and final fixture outcomes. Candidates did
not retry the unavailable optional provider, while one baseline did. A candidate
progress append failed without losing original progress or immutable results.
Strict all-trial adherence therefore failed as well as the numeric gate. Hook
execution remains separately covered by regressions.

## Experiment 3 protocol: existing context and bounded verification

Compare inline continuation with a fresh native verifier, in three alternating
pairs separately for short and long existing contexts. The task assesses current
retained coverage, selective invalidation, original findings and sourced
dispositions, incomplete optional work, counters and missing completion evidence.
It does not execute pending reviews or complete the workflow.

The setup turn reads an immutable orientation corpus before the task assignment.
Short context contains the task-state, native-handoff, preference and reviewer
contracts. Long context adds complete frozen production Python and governing
Markdown sources. Original source chunks preserve exact bytes and full output;
unread or truncated evidence must be retrieved or reported incomplete. Source
bytes and the first parent task-response input establish the observed context
classes; no filler or compaction-setting changes are used.

Both task arms receive identical verification instructions and source access.
Candidate parents prepare one fresh verifier, wait on its native handle, validate
its terminal report and consume the result. The task-specific preference handoff
includes a scoped conditional instruction to preserve original finding URLs;
original reviewer selections remain separately preserved to avoid invalidating
the task whose reuse is being checked. Missing evidence requires retrieval or an
incomplete report. Neither arm may claim pending tasks, allocate cycles, invoke
domain work, publish, or infer final-head CI success.

Report task, setup and combined usage separately. The primary comparison concerns
a task from an existing context; task-phase savings do not imply whole-session
savings. All task-phase parent preparation, waiting, result consumption and
descendant responses count. Native launch and terminal records reconcile the
recursive descendant inventory; inherited history is deduplicated by response ID.
Report native tool calls for parent and descendants separately, alongside their
total. Preserve full original final outputs privately and commit only normalized
metrics, hashes, quality checks and independent decisions.

Independent orientation setup may overlap once its inputs are frozen. Keep the
measured task phases within each context class in their declared alternating
order. In this sample, long B1's task also overlapped short C3's task; later long
orientation overlapped an earlier long task. All usage remains counted. Shared
cache and service effects are limitations; these experiments make no global
serialization, strict cache-isolation or elapsed-time claims.

The [twelve-trial report](evidence/145-experiment-3.json) rejects delegation in both
context classes. Short median task tokens increased from 444,814 to 1,033,698;
long median task tokens increased from 1,230,168 to 2,540,372. No pair improved.
Median task API-equivalent cost increased from $0.1711008 to $0.3711368 for short
context and from $0.2296960 to $0.5722448 for long context. Setup and combined
comparisons also failed the gate. The candidate guidance was removed.

Independent review confirmed all usage, source pins and required assessment
content. Strict short-context correctness failed because one parent's structured
report repeated coverage keys, overwriting Boolean values with hashes when
parsed. Its child report and parent prose retained the correct conclusion; the
original output remains unchanged and the defect is recorded. Long-context
assessment correctness passed. These bounded verification trials do not support
a broader conclusion about investigation or general code review.

## Experiment 4: focused immutable-evidence handoffs

The [six-arm report](evidence/145-experiment-4.json) rejects the fresh-context
handoff proposal for bounded retained-evidence verification. Both parents
launched one native verifier: inherited history versus a focused assignment with
complete immutable evidence references. All three candidate task totals improved,
but median task tokens fell only 9.62%, from 2,355,154 to 2,128,497, below the
required 10%. Median estimated API cost increased from $0.4393272 to $0.6883816.
The candidate guidance was removed; no production optimization was adopted.

Setup, complete parent/child task and combined usage remain separate. P3's
original candidate parent completed orientation before the interruption but was
inaccessible in the resumed session; its task had never started. A distinct
recovery arm repeated orientation from 146 byte-identical copied files and ran
the unstarted task. The original setup's 1,330,137 tokens and $0.3623796 estimated
cost remain additional interruption overhead. Copied specifications retain
references to preserved original immutable evidence; recovery state and trace
are separate. Neither original nor recovery counters or frozen sources changed.

P3 baseline's setup used a semantically equivalent closing sentence with different
bytes. The recovery setup omitted the canonical template's trailing LF byte.
Both variations and hashes are disclosed, with unknown token/cache effects.
Failed exploratory calls, recovered truncation, actual overlaps and original
output hashes remain in evidence. Independent raw-output review also found an
unrecovered 2,370-token truncation inside the recovery child's orientation-corpus
inventory. Mandatory assessment sources were complete, but strict all-trial read
adherence failed; the original child report is preserved without repair or rerun.
A separate 482,985-token missing-evidence probe
returned incomplete when mandatory preference context was unavailable; it is
excluded from the three performance pairs. These bounded task results do not
establish whole-session savings or generalize to other review responsibilities.

## Experiment 5: native reasoning effort

The [twelve-arm report](evidence/145-experiment-5.json) records three alternating
pairs for each scenario, with actual native effort controls and identical
source, guidance, task, preferences, assignment, model and tier within each
scenario. Source `4490536` is the latest retained state after experiment 4.
Every measured task worked inline, with no orientation or downstream verifier;
all task responses count. Experiment-owner orchestration is outside the measured
tasks, so these results do not establish complete root-session savings.

Routine bounded verification with medium effort reduced median tokens from
392,677 to 340,344 (13.33%); all three pairs improved. Median estimated API cost
increased from $0.1716020 to $0.1890292. Reject this proposal under the unchanged
cost gate. Semantic assessments pass, but strict routine adherence also fails:
one baseline ran the unauthorized read-only `resolve --help` operation. Its
successful role lookup, unchanged state and all usage remain preserved; no
completed trial was repaired or rerun.

Brief routing/status with low effort reduced median tokens from 283,630 to
204,023 (28.07%); all three pairs improved. Median estimated cost fell from
$0.1054068 to $0.0864920. These are isolated native task measurements. The current
host exposes effort selection when launching a new native task, but provides no
evidenced control to change an existing inline task's effort. Creating production
agents merely to lower effort is forbidden. A policy for selecting effort on an
already independently required task was not tested end to end. Reject production
routing-policy adoption as unsupported; preserve the passing measured treatment
separately from that decision. Existing inline behavior and explicit model/effort
requirements remain unchanged.

All original findings, sourced dispositions, conditional preference decisions,
required/optional coverage, counters and unfinished-work evidence remain in the
assessments. Original reviewer selections remain separate; the supplemental
condition applies to verification and is false for routing-only work. Candidate
policy text was absent from both frozen guidance arms to isolate native effort;
selection/adoption behavior is outside these samples. The proposed guidance was
removed, with its patch retained as nonexecuting evidence. Raw conversations
remain private. Upstream finalizer/preflight and commit-policy changes are outside these pinned native inputs
and require separate final integration regressions.

## Historical integration validation

The [validation record](evidence/145-final-validation.json) pins integrated
source and preserved commits from before the rebase, using main
`d18c6eb7be2c4f8588fc99dc2060565e4e641429`. Its finalizer/preflight and
commit-policy changes are retained separately from native performance inputs. The release is
synchronized to 3.4.1. Production commands, configuration schemas, private formats
and native guidance match integrated main. Final validation records distinguish
the prior 3.3.1 integration and its passing 792-test/seven-subtest suite from the
new 3.4.1 integration, which passes 842 tests and seven subtests on Python 3.12.
Package, distribution synchronization, whitespace and current-base release checks
also pass. The earlier published head received no CI checks after main
advanced and caused conflicts; that observation is not a passing CI result.

Earlier local attempts remain recorded: missing parallel dependencies and a
process-count assertion on default Python 3.14. The supported Python 3.12 setup
uses isolated CI dependencies, without assertion changes or native-trial reruns.
The whole personal configuration hash differs from the initial private
observation; no experiment action edits personal or compaction settings, and
effective native model/effort/tier controls were independently verified.
Whole-file equality is not claimed. Final exact-head CI is recorded in PR #145
after final publication; local checks do not substitute for that gate.
