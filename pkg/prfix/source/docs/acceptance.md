# Behavior contract

Required behavior: unchanged reruns invoke zero reviewers; failed or interrupted tasks retain completed siblings; five initial review/fix cycles with retained same-cycle retries and explicit bounded continuation; tree-preserving commit cleanup; fresh publication reconciliation after interruption; exact-head CI; existing partition settings and explicit human acceptances.

[Managed bookkeeping](task-state.md) combines spec construction, capture and claims in prepare, retains minimal replay definitions in existing entries, and finishes by task key. Fresh-process resume recaptures current sources without model-built specs and exposes compact counts and evidence references. Tests cover legacy adoption without cache/budget resets, concurrent/lost-response claims, successful siblings, source drift, removed occurrences, history-only cleanup and read-only targeted result/progress reads. Preference handoffs reuse unchanged condition decisions; the settings facade reads selected sections once without storing effective snapshots. These checks are behavior and output-size proxies, not native token measurements. Follow [state admission](../DESIGN.md#objective-and-state-admission) for every additional persisted field.

Routine PR execution reads current settings directly without invoking `dk-workflows:dk-config`.
Requests to change, inspect or explain configuration still use that skill; scoped
preferences use `dk-workflows:dk-preferences`. The [native decision exercise](native-config-invocation-exercise.md)
covers collected request shapes and boundary cases. Offline helper tests verify
unchanged stores, scope precedence, settings refresh and missing-skill blockers.

[Review continuation](task-state.md#retained-budgets) distinguishes ordinary interruption resume from renewed explicit user review authorization. The helper appends a bounded allowance to an exhausted run under a stable request identity without resetting history, counters or claims. Tests cover legacy cross-session exhaustion, target advancement after a clean pass, idempotent authorization/replay, preserved evidence and concurrent requests. The separate [bounded native exercise](native-review-continuation-exercise.md) checks coordinator interpretation with isolated synthetic state.

[Scoped preferences](preferences.md) have deterministic file, directory, glob, exclusion and rename matching, atomic personal/repository admission with stale-revision rejection, and separate implementation/review modes. Native admission settles contradictions with the user before saving. Coordinators evaluate conditions and preserve complete records and applicable paths downstream. Task-specific selections and decisions are fingerprinted; relevant changes refresh affected work and unchanged selections preserve review reuse. Preference unit, handoff and full-recipe fixtures cover the storage and consumption boundaries; they do not establish independent native semantic-admission adherence.

Agents resolve and invoke native skills; selected skills own internal passes and completion reports. Domain skills own GitHub and publication. Architecture work is independently selected. The cache applies only to invocations explicitly managed by workflows.

[Commit policies](commit-policy.md) accept exactly one saved prompt or native skill, with whole-policy repository/upstream/global inheritance and reset. Policy providers return complete instruction reports without side effects; missing providers and incomplete reports block cleanup. Captured configuration, resources, history, context and returned instructions govern reuse. Tests cover private round trips, unrelated-setting preservation, prompt/default direct calls, provider failure, relevant changes and unchanged reuse. A real Git fixture verifies one subject-only commit, identical final tree, preserved content review and exact resulting-head CI. The [bounded native sample](native-commit-policy-exercise.md) additionally exercises in-session policy interpretation and a lease-protected push to a local bare remote; it does not establish independent host lifecycle adherence.

[Native reviewer handoffs](native-review.md) retain host handles and terminal
outcomes separately from governing inputs. Successful host execution and a
nonempty complete skill report are both required; missing, malformed, failed,
cancelled or unknown outcomes cannot establish coverage. Empty completed results
are rejected on recording and reuse. The coordinator trusts native execution and
the selected skill instead of reconstructing nested execution from wire events.

With the default finalizers, iteration waits for CI at a final completion gate after review/fix cycles, commit cleanup and governing-content synchronization. Pending or unsuccessful intermediate checks do not block independent review or cleanup. Selected CI failures remain blockers; code repairs refresh scope, affected review coverage and selected cleanup before observation of the new final head. Retain observation deadlines, five CI repairs per cycle, three transient reruns per head and the current authorized review allowance. Standalone CI observation is unchanged.

[PR fix finalizers](finalizers.md) replace the ending with qualified skill strings and nonempty parallel groups. Scoped lists replace rather than merge, an empty list disables finalizers, and omitted configuration preserves effective default role selections. External skills resolve through native tools. Groups retain individual claims and original results across interruption and failure; later groups wait for all siblings. Plan/resource/context changes stale evidence, while unfinished task identity survives reordering. Final live checks distinguish selected-plan completion from skipped or missing readiness evidence. Offline tests cover configuration, public helper commands, concurrent fixture skill processes, failure barriers and recovery; these fixtures do not establish native-agent adherence.

Coordinated Feedback, Decisions and read-comments consumers at a shared analysis freshness boundary use one complete initial PR evidence handoff by default. Each consumer still verifies live sources immediately before acceptance or recording. Invalid artifacts and changes to feedback, resolved discussion, Decisions or direct/file instructions require recollection and reassessment even at an unchanged head. Standalone collection, full original schemas, publication validation and final live completion checks remain available. The [bounded native evidence exercise](native-evidence-exercise.md) records equivalent coordinator tasks with initial collections, per-consumer acceptance verifications, mocked transport reads, returned bytes and original outcomes.

Regression scenarios in tests exercise actual Git trees across squash, independent reviewer resource edits, scope/instruction changes, damaged and failed outputs, concurrent claims, interruption, explicit invalidation, retained cycle/retry limits and readiness separate from completed findings. Package tests exercise publication interruption and standalone domain helpers. No authenticated model or GitHub calls are required.

[Feedback reply scenarios](native-feedback-reply-exercise.md) cover an inline follow-up, a review-body finding with an embedded file/line reference and a top-level comment. Explicit offline requests verify their publication endpoints, attribution bodies, source identity and single-dispatch recovery. The separate bounded native exercise checks agent-selected requests; fixture transport results alone do not establish native source selection.

PR skills, helpers, tests and fixtures are bundled in dk-workflows. Its workflow, PR and integration suites run without another repository or external test checkout. Architecture remains in its independent repository. Production coordination invokes selected native skills and keeps domain behavior inside its owning skills/modules.

The full recipe test in tests/test_recipe_integration.py drives separate helper/skill processes and a real local Git remote. It covers feedback, Decisions/description synchronization, partition assessment, interrupted and partial reviews, retained retry/cycle budgets, findings fixes, tree-preserving squash cleanup, final coverage, replies/resolution, final CI/rerun receipts, re-review and completion. It verifies that CI is not observed before reviews and cleanup, that the normal path observes only the final head, and that a final CI repair refreshes review and cleanup before the new head is observed. It then repeats standalone review and iteration, asserting zero reviewer launches and no duplicate publications. This is deterministic integration coverage; native-agent adherence also needs a controlled skill exercise when recipes change materially.

An explicit checkout, `dk-workflows:dk-pr-iter`, then merge request survives routing to either workflow, finalization and ordinary resume for the exact PR. Default disabled automatic merging does not revoke it. Separately authorized own-PR merges and empty/null review decisions are accepted only with current clean repository merge readiness and successful CI. The recipe fixtures additionally carry the original combined instruction through own-PR routing, interruption, cleanup and final CI repair before merging the final published head, recovering a lost response without duplicate dispatch. Iteration-only scenarios still send no merge. Publisher regressions retain required-review, requested-change, unknown-decision, CI, target and mergeability blockers and the self-approval prohibition. These fixtures exercise deterministic boundaries; they do not establish native interpretation of the combined instruction.

The [bounded native iteration exercise](native-iteration-exercise.md) compares fresh agents on pinned equivalent resumed tasks and records observed reads, calls, byte sizes, task outcomes and retained counters. Its offline domain stand-ins and narrow scenario remain distinct from complete-recipe fixture coverage; unavailable host/token/context evidence is unknown.

[Private workflow reporting](private-reporting.md) governs every skill and all
handoffs, including direct publication. Package checks reject a skill that omits
its contract. Offline regressions check inheritance and conflicting instructions,
findings-only review transport, unavailable publication evidence and uncertain
writes with durable private diagnostics and single-dispatch recovery. They do
not classify model-authored prose. The separate [bounded native reporting
exercise](native-private-reporting-exercise.md) checks public payloads and private
user reports for failed/missing reviewers, inaccessible sources, incomplete
verification, exhausted budgets and publication uncertainty. Ordinary tests do
not invoke a model or establish native adherence.
