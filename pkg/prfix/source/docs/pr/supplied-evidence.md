# Shared PR evidence

When two or more bundled feedback, Decisions or feedback read tasks share an analysis stage, use one complete handoff collected by the coordinator. Pass its compact receipt to each task. Use its analysis view instead of collecting separate initial snapshots. Tasks at different analysis stages need fresh evidence. Standalone tasks without a handoff collect complete, stable snapshots with `fetch_unresolved_review_threads.py` and `fetch_pr_conversation.py` under `runtime/dk-pr-feedback-address/scripts/`.

## Collect

Run `python3 PACKAGE_ROOT/runtime/dk-pr-feedback-address/scripts/pr_evidence.py --cwd REPO collect --pr PR_URL`. The canonical URL includes the upstream hostname. Optional `--threads-file FILE` and `--conversation-file FILE` accept full snapshot JSON or artifact receipts. Pass `--instructions-file FILE` for each original governing instruction file. The handoff preserves its exact bytes, path and hash. Keep direct user instructions separately and reread them when checking freshness.

Snapshots must be complete and match each other, the canonical PR and the assigned head. Missing, corrupt, incomplete or mismatched evidence requires the coordinator to collect it again and reassess affected tasks. Do not accept partial evidence or silently collect separate initial snapshots for shared analysis.

Both fetch helpers support `--artifact` for private storage and compact receipts; otherwise they return full JSON. The conversation helper's `--snapshot-file` accepts full thread JSON or a thread receipt. Receipts contain PR and repository, head, completeness, counts, source references, byte count, SHA-256 and the original absolute artifact path. Full thread evidence includes resolved threads, all paginated comments and the PR description with Decisions.

## Analyze

Run `python3 PACKAGE_ROOT/runtime/dk-pr-feedback-address/scripts/pr_evidence.py --cwd REPO view --handoff PATH --sha256 HASH --pr PR_URL --head HEAD --role feedback` or use `--role decisions`. Feedback read tasks use the feedback view. Save the expected hash from the compact receipt separately. The helper checks it before loading manifest fields or instruction references.

The view contains complete threads and discussion, original PR metadata and instruction references. Feedback also includes unresolved thread references. A selected comment or review remains identified within the full discussion. Read required original evidence from artifact paths. Preserve work items, sources, settled decisions, deferred work, exact instructions and dispositions. Unresolved comments are not automatically actionable. Suggestions are not settled decisions.

## Verify before acceptance

Each task independently runs `python3 PACKAGE_ROOT/runtime/dk-pr-feedback-address/scripts/pr_evidence.py --cwd REPO verify --handoff PATH --sha256 HASH --pr PR_URL --head HEAD` immediately before accepting or recording its result. It also rereads current direct instructions. Verification reads every page until consecutive snapshots are stable, then compares full sources, including description, resolved discussion and original instruction files.

Integrity and an unchanged head do not prove freshness. Discussion, Decisions and instructions can change without commits. Changes require new collection and reassessment. Keep prior dispositions only while sources still support them. Repeat verification at each required later boundary and after intervening changes. A saved success receipt does not prove later freshness.

Evidence and verification do not authorize publication. Publishers check current state and reconcile writes. Completion rereads live scope, instructions, feedback, Decisions, head and target. Consuming skills verify immediately before workflow `record`; task state remains independent of PR-specific checks.
