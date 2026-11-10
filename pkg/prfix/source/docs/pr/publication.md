# Publication and recovery

Follow [private workflow reporting](../private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

Confirm that the caller authorized the concrete action. Inspect every exact public text field against the private reporting contract before preparing the payload and expected live identity. Keep private reports separate from publication requests; the helper does not classify prose. Save intent before dispatch. After an uncertain write, reconcile read-only before preparing or attempting a replacement. Never reset pending intent after a timeout. Compare actor, target, exact payload, head, lease and remote receipt. Missing or ambiguous objects remain unresolved. Keep confirmed publications recorded even if readiness changes. They do not authorize another write.

Run `python3 PACKAGE_ROOT/scripts/publish.py prepare --file REQUEST.json` to save intent and get an operation path. `publish --operation FILE` checks current state and sends at most once. `reconcile --operation FILE` only reads. Repeating publish for pending or verified operations also only reads. Return the operation path and reuse it on resume. Other publishers may reuse `runtime/shared/publication.py` with their own check, send and observe callbacks.

For preparation, pass the caller's `--upstream HOST/OWNER/REPO` and publisher `--options FILE` when provided. If a merge request omits its method, resolve `default_merge_method` with those scopes and local options; an explicit method wins. Retain the resulting method in the original intent. Reconciliation uses that intent even if configuration changes, without creating a replacement action or refreshing an uncertain payload.

Requests:

```json
{"kind":"edit-pr","pr":"https://github.com/owner/repo/pull/7","head":"FULL_HEAD_SHA","fields":{"title":"Reviewed title","body":"Reviewed body"}}
```

```json
{"kind":"reply-inline","pr":"https://github.com/owner/repo/pull/7","head":"FULL_HEAD_SHA","parent":123,"source":{"kind":"inline_comment","id":456,"url":"https://github.com/owner/repo/pull/7#discussion_r456","thread_id":"PRRT_original"},"body":"Verified answer"}
```

Select reply destinations from the structured original PR evidence. An actual inline review comment uses `reply-inline` in its originating thread; `parent` is the root comment's database id, including when answering a later reply. Preserve that later comment's own id and exact URL as `source.id` and `source.url`. Retain the thread node id as `source.thread_id`. Do not substitute a review id for an inline comment database id.

A review body or top-level comment without an inline parent uses `reply-comment`. Begin its exact body with `In Reply To: [descriptive source label](https://github.com/owner/repo/pull/7#pullrequestreview-789)`, followed by a blank line and the substantive response. Preserve the original URL and fragment. Embedded file/line references, code links and quoted inline findings do not create an inline parent; unrelated inline comments belonging to that review do not route a response to its review body.

```json
{"kind":"reply-comment","pr":"https://github.com/owner/repo/pull/7","head":"FULL_HEAD_SHA","source":{"kind":"review","id":789,"url":"https://github.com/owner/repo/pull/7#pullrequestreview-789"},"body":"In Reply To: [Original review](https://github.com/owner/repo/pull/7#pullrequestreview-789)\n\nVerified answer"}
```

Reply callers supply a `source` object with `kind` (`inline_comment`, `review` or `issue_comment`), the original database `id`, and exact `url`; inline sources also carry `thread_id`. The caller verifies identity, membership and destination from complete evidence before preparation, refreshing missing or ambiguous evidence. The publisher routes by `kind` and `parent`; it journals the supplied source record without independently classifying or validating it. The helper saves the actor and existing reply ids to distinguish this attempt from older identical replies.

Retain source identity, routing parent, head, exact body and operation path through handoffs and resume. Reuse the operation for reconciliation; do not infer a different destination from prose, replace the source URL or regenerate an uncertain reply's attribution. Exact-body observation must preserve the heading as well as the response.

## Recover an uncertain review request

A pending `request-review` has an unknown delivery outcome. An empty current
reviewer set does not prove it was never delivered: the reviewer may have
completed review or the request may have been removed. Preserve the original
intent, failure and any returned response. Do not resend it, add an action ID,
change the reviewer set or edit the journal to bypass duplicate protection.

Run `python3 PACKAGE_ROOT/scripts/publish.py inspect-review-request --operation FILE`.
It reads current PR/head and requested reviewers, and paginated timeline/review
history for the intended reviewers. Its `historical_failure`, available creation
and dispatch times and native session/run references describe the retained
operation; `current_observation` describes this inspection. Missing legacy
provenance and root cause remain explicitly unknown. Timeline requests, removals
and submitted reviews supply context, without attributing them to this operation
or proving delivery/non-delivery. Read errors cannot establish a disposition.
The dispatch timestamp records persisted dispatch intent, not proof that a network
request reached GitHub.

New requests retain execution phase, exit status and available HTTP status in
safe failure diagnostics. Request payload evidence comes from the original
reviewer arrays. Failed-command response evidence uses a content hash/length,
excluding raw exception response bodies and headers; credential-bearing error lines
are redacted. A response returned before a later observation failure stays retained
with the original intent.
Reconciliation errors are recorded separately and preserve the first failure.
An error such as `unexpected end of JSON input` alone does not establish whether
request parsing, response parsing, transport or GitHub caused the failure.

Reassess whether another review request is needed against current code and
discussion. When no further request is needed, save an explicit private decision
with the inspected fingerprint:

```json
{"decision":"no_resend","reason":"The reviewer completed the requested review; the current code and discussion need no further request.","observation":"INSPECTED_FINGERPRINT"}
```

Run `python3 PACKAGE_ROOT/scripts/publish.py dispose-review-request --operation FILE --file DISPOSITION.json`.
Then reconcile the original operation. The result reports `status:disposed`,
`historical_status:pending`, `delivery:unknown` and `review_obligation:no_resend`.
The original intent and failure stay intact and no receipt is invented. Changed
PR/head, current reviewer set or retained historical reference fields requires
inspection and reassessment. The fingerprint covers request/removal IDs, event
types, times and reviewer identities, plus review IDs, states, submission times,
commit IDs and reviewer identities. It does not cover review bodies or other
discussion: reassess substantive discussion changes before reusing a decision.
Each publication's own live gates still apply.
There is no automatic replacement path; even a changed action ID remains blocked
by this pending review intent. If another request is still required and delivery
cannot be established, report that obligation as incomplete.

Independent authorized `reply-inline` and `reply-comment` actions may proceed
while an unrelated review request is pending, without changing or replaying it.
Both preparation and dispatch enforce this exception and each reply's own live
checks. Other actions remain blocked until reconciliation or a current
`no_resend` disposition; their own publication gates still apply. Pending writes
of other kinds retain the existing target-wide barrier.

## Recover a previously verified reply

A saved verified receipt establishes a historical publication. If that exact comment later becomes unavailable or its body changes, preserve its original request, response and receipt. Another identical comment cannot substitute for the original receipt. A missing reply with an uncertain initial write remains pending and cannot use this recovery path.

Run `python3 PACKAGE_ROOT/scripts/publish.py inspect-reply --operation FILE`. This reads the current PR, actor, original feedback, complete reply list and original comment by id, and saves a separate private observation. Inline recovery also verifies the retained source and routing parent in the original thread. HTTP 404 with accessible PR/source and no listed original reply records `unavailable`, without inferring deletion or its cause. A matching original object with changed body records `edited`. Other access failures, inconsistent list/individual responses or ambiguous membership cannot settle the obligation. Restore complete evidence before proceeding.

Reassess the original feedback against current code, validation and discussion. Decide whether an answer is still required; a missing historical comment alone is not permission to post again. Save a private disposition file with the inspected `fingerprint` as `observation`:

```json
{"decision":"no_replacement","reason":"The original question is obsolete because the affected code was removed; current code and discussion were reassessed.","observation":"INSPECTED_FINGERPRINT"}
```

Run `python3 PACKAGE_ROOT/scripts/publish.py dispose-reply --operation FILE --file DISPOSITION.json`. Use `no_replacement` only when the current answer obligation is settled without another post; use `replacement_required` when a supported answer remains necessary. The reason records the concrete reassessment privately. Neither command writes to GitHub. Dispositions retain prior observations and decisions and bind the live actor, PR/head, exact original source, destination and original reply state. Repeated identical disposition is idempotent. Changed evidence requires fresh inspection and reassessment; an uncertain linked replacement must be reconciled before another disposition.

After `no_replacement`, reconcile the original operation. Successful reconciliation reports `reply_obligation:no_replacement` and allows the reply finalizer to finish with its historical receipt intact. It does not claim that the old reply is currently visible. It does not resolve an inline thread or relax the resolution publisher's live verified-answer requirement.

If the original reply becomes visible again with its exact historical body after a disposition, inspect and reassess it again. Record a fresh `no_replacement` decision when that answer settles the current obligation, then reconcile. Its original request, response and receipt remain intact; a restored original cannot receive a `replacement_required` disposition. Reconcile any uncertain linked replacement before changing the decision.

After `replacement_required`, prepare a fresh reply request with the current head, exact original `source` and destination, a reviewed current body, `replaces` containing the original operation path, and `authorization` recording the separate authorization for this concrete replacement. Existing caller authorization may cover the replacement; obtain it only if absent. Never replay the old operation or add an action id to evade uncertainty. Preparation links one replacement operation to that disposition. Recover that path on resume; repeated preparation reuses it and another replacement for the same disposition is rejected. Publish/reconcile the linked operation normally, then reconcile the original. The original obligation finishes only after the linked answer is verified and currently visible. Source, destination, actor and head checks run again before dispatch. A stale undispatched replacement can be superseded by a fresh disposition without erasing its history; its old dispatch remains blocked.

Keep observations, dispositions, authorization evidence and operation paths private under the reporting contract. For a retained legacy reply without a structured original source, recovery remains blocked until its provenance can be established; do not edit a journal to invent it.

```json
{"kind":"push","checkout":"/owned/checkout","remote":"https://github.com/owner/repo.git","branch":"pr-branch","head":"FULL_VERIFIED_HEAD_SHA","expected_head":"FULL_REMOTE_LEASE_SHA"}
```

Before preparing a push, verify branch and repository ownership, fetch and push URLs, authorship, target tip, tests and tree. Preparation saves the candidate tree. Dispatch uses an exact force-with-lease; observation checks the saved head. If the lease changes, inspect the remote change. Never update the lease and retry automatically. The publisher does not use a shell.

Other actions use the same commands and journal. Requests include canonical `pr` and captured `head`:

| kind | Additional request fields | Fresh validation and receipt |
| --- | --- | --- |
| ci-approve | base, run_id, attempt, evidence | Require [current incoming-review evidence](ci-approval.md) and the exact fork gate. Recheck before dispatch; observe execution permission for the same attempt. Never resend uncertain writes. |
| ci-rerun | run_id, attempt, reason | Require the current completed failed attempt at this head. Rerun only failed jobs. Observe exactly attempt + 1. |
| resolve-thread | thread_id, reply_id | Read every thread page. Require this PR and head and your verified posted answer. Observe resolved state and unchanged answer. |
| request-review | reviewers, team_reviewers | Use login and team-slug arrays with at least one entry. Observe the requested set. Already satisfied requests send nothing. |
| create-draft-pr | head_branch, branch_sha, base_branch, base_sha, title, body | Require unchanged candidate and default target refs, and no PR for the branch. Observe exactly one matching draft by the actor and its full payload. |
| post-review | event, body, comments | Use COMMENT, APPROVE or REQUEST_CHANGES. Comments use path, line, side and body and map to changed lines. Observe the exact review and comments by the actor at this head. |
| merge-pr | base, merge_method | Require separate permission, complete clean review and verification, satisfied repository review requirements, an open non-draft mergeable PR, and successful CI at the reviewed head and target. Bind the request to the head. Observe merged state, actor and exact merge-commit parents. |

For `merge-pr`, set `base` to the reviewed target SHA and `merge_method` to `squash` or `merge`. The caller establishes permission and complete review and verification. The publisher independently rechecks target, head, actor, author, repository readiness and successful checks. It calls GitHub's [merge endpoint](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request) with the reviewed head SHA. It never bypasses protections.

An explicit user request to merge the exact PR supplies merge permission even with the default `processes.pr-review.permissions.merge_pr.enabled:false`. That setting disables automatic incoming-review merges; it does not veto a direct instruction. Merging your own PR is permitted when separately authorized; the self-approval prohibition applies to posting `APPROVE`. A current `reviewDecision` of `APPROVED`, empty string or null is accepted only with REST `mergeable_state:clean` and GraphQL `mergeStateStatus:CLEAN`. Required reviews, requested changes, missing or unknown decisions and other readiness failures block dispatch. Do not post an approval solely to make merging possible.

The endpoint binds the head but cannot atomically bind the base. Reconciliation checks merge-commit parents against the reviewed base and method. A changed target or another actor's merge remains a blocker even if the remote PR merged. An uncertain outcome permits reads only. Never repeat the merge. Invoking `dk-workflows:dk-pr-fix` alone does not authorize merging; carry a separate explicit merge request through finalization to this handler after current review, verification and CI gates pass.

If preparation or dispatch is blocked, report the exact guard diagnostic, affected PR and next required evidence privately. Distinguish a rejection before dispatch from a pending operation with an uncertain outcome. A generic statement that guards rejected the merge does not explain the blocker. Preserve any existing operation path for read-only reconciliation.

Example CI rerun request:

```json
{"kind":"ci-rerun","pr":"https://github.com/owner/repo/pull/7","head":"FULL_HEAD_SHA","run_id":123,"attempt":1,"reason":"Verified transient infrastructure failure"}
```

Reserve the caller's rerun budget before publication. Pass the verified operation to `ci_wait.py --rerun-operation FILE`. It opens one new attempt window and preserves it on resume. A lost response, delayed or advanced attempt, or ambiguous receipt permits read-only reconciliation only. Do not send again. [GitHub's failed-job endpoint](https://docs.github.com/en/rest/actions/workflow-runs#re-run-failed-jobs-from-a-workflow-run) reruns only failed jobs and dependencies.

For a new branch, use `kind:push`, `create_branch:true` and `expected_head:null`. The lease requires the remote branch to be absent, preventing overwrites. Capture and verify the candidate tree as for an existing branch.

Preparation deduplicates identical payloads in a private journal for each target, even if the caller lost the operation path. A pending publication blocks other writes to that target until reconciled, subject to the uncertain-review-request recovery and independent-reply exception above. Independent targets have separate journals. Use a stable optional `action_id` only for a distinct, newly authorized action. Changing it cannot bypass a pending write. Already satisfied description edits and identical replies by the same actor return verified no-ops after current checks.

The publisher serializes dispatch per target and saves original intent before sending. Reused receipts still require current checks. Keep observed writes recorded even if readiness later fails. Inaccessible, changed or ambiguous remote results remain unresolved.

Review observation reads every comment in the matching review and fetches each individual comment by id. It verifies membership, PR, actor, original reviewed commit and exact payload, including the original location when a later push has moved or outdated a comment. Missing list locations do not invalidate matching individual evidence; conflicting or insufficient evidence remains pending. Reconcile an existing pending operation using its saved path without resubmitting or editing its intent. A confirmed historical submission is recorded as verified even if current PR metadata has changed; that readiness failure is reported separately and subsequent publications require fresh preparation and live checks.
