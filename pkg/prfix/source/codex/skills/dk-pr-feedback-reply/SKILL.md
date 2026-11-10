---
name: dk-pr-feedback-reply
description: Reply to affected PR feedback and resolve fully answered inline threads when authorized.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-pr-feedback-reply` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

First decide whether the author needs to respond under [feedback response obligations](../../../docs/pr/feedback-responses.md). Leave questions for other participants and discussion requiring no author response unanswered and unresolved; optional replies do not block completion or require user approval to omit. Honor explicit user instructions to reply and handle separately actionable author requests in mixed threads. Return source references and reasons privately, without creating publication operations for omitted replies.

Read the original thread, comment or review, plus current code and validation. Reply only with a supported repository answer. If internal failures prevent that answer, withhold the affected reply and report the reason privately; never substitute a diagnostic reply. For a needed or explicitly requested reply, select the destination from the structured original source in the complete PR evidence, following [publication and recovery](../../../docs/pr/publication.md):

- For an actual inline review comment, use `reply-inline` inside its originating thread. Set `parent` to that thread's root comment database id, even when answering a later reply. Preserve the answered comment's own id and exact URL separately from this routing parent.
- For a review body or top-level comment without an inline parent, use `reply-comment`. Begin the exact body with `In Reply To: [descriptive source label](https://github.com/owner/repo/pull/7#pullrequestreview-789)`, then a blank line and the substantive response. Use the original review or comment URL, including its fragment. A file/line reference, quoted inline finding or code link embedded in a review body does not create an inline parent. Other inline comments attached to the same review do not change the review body's destination.

Use `scripts/publish.py` for both reply kinds. Include the structured `source` record described in the publication contract. Verify source identity and thread membership from the original evidence; if missing or ambiguous, refresh the evidence before choosing a destination. Save the source kind, original id and exact URL, routing parent/thread where applicable, actor, head, exact body and returned operation path. On resume, reuse that operation and reconcile pending writes before retrying; do not reconstruct the source or reformat the saved attribution.

If a previously verified reply is now unavailable or edited, follow the publication contract's `inspect-reply` and `dispose-reply` recovery procedure. Preserve its original receipt and reassess the original feedback against current code, validation and discussion. Record `no_replacement` with a concrete private reason only when no further answer is needed; successful reconciliation then settles this reply finalizer. If an answer remains required, record `replacement_required` and prepare a separately authorized current answer linked through `replaces`. Recover the linked operation on resume and finish only after its current verification. Never silently replay the old write, infer deletion from a 404, dispose an uncertain initial submission, or clear unrelated pending operations. Missing or ambiguous read access remains a blocker.

Honor `resolve_answered_threads`: false leaves threads unresolved after replying. When true and authorized, resolve only fully answered inline threads, leaving questions for other participants open even after answering the author's part. Use the `resolve-thread` publisher with the thread node id and the database id of your verified posted reply. Top-level comments cannot be resolved. Return the original source, reply URL and resolution evidence for published replies; a no-reply assessment can complete without a reply URL or receipt.
