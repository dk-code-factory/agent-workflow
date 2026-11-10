# Commit cleanup

The inspection helper requires Git 2.45 or newer. Its explicit `--no-lazy-fetch`
option makes older Git reject the command before repository inspection, rather
than ignore an unsupported environment safeguard.

Use `python3 PACKAGE_ROOT/scripts/commit_series.py --checkout REPO --target TARGET
--head HEAD` for pinned read-only inspection. It returns resolved commit pins,
a unique merge base, reviewed tree and ordered commit metadata, parents, complete
messages and changed paths as JSON. It never fetches, creates recovery refs or
rewrites. Missing or ambiguous history blocks rewriting. Read every relevant
patch and the final diff; metadata cannot replace content assessment.

For cleanup, first resolve or verify [the effective commit policy](../commit-policy.md), including on direct invocation. Inspect every commit from merge base to head against the final reviewed diff. Before rewriting, save original head and tree, target tip, remote branch lease, ownership and a recovery ref. Require a clean worktree and safe ownership by the same author. Stop if authorship or branch ownership is ambiguous. Apply explicit grouping instructions within those safeguards. Without a configured policy, preserve logical boundaries, split mixed commits, fold fixup commits into the commits they repair, and reorder only when useful. One logical commit is valid. Without explicit grouping instructions, do not rewrite just to change the count.

Draft messages through `dk-workflows:dk-commit-message` with the policy's message instructions, required repository templates and current user constraints. Explicit policies replace built-in stylistic defaults. Without explicit message instructions, use recent human conventions and the message skill's configured fallback subject, body-wrap and line limits (72, 72 and 80 by default). Review boundaries and messages together. Stop at the first unchanged pass or after the owning cleanup skill's configured `max_passes`, at most five total passes. Report concerns left after the limit. Run relevant tests and verify that the final tree exactly matches the reviewed tree.

Before pushing, recheck target tip, published source head, remote lease and
governing context. Changed files require content review. History-only changes
require fresh CI and publication checks for the new head, plus refreshed tasks
that use history. Use `scripts/publish.py` for authorized pushes with exact leases.
A changed remote lease or target tip blocks publication until reassessed.

Return the shared task result plus policy evidence, PR identity, reviewed and final heads and
trees, target tip, commit count, per-commit coverage and boundary reasons,
source-to-destination mapping, validation, recovery refs, publication operation
paths and receipts. For unchanged commits the mapping is identity. Include
resulting-series coverage after a rewrite. Use these cleanup outcomes:

| Outcome | Completion |
| --- | --- |
| `no_op` | Every commit and boundary was assessed and the series needs no change; an unchanged SHA alone is insufficient. Complete only when no required publication remains. |
| `prepared` | A local rewritten series passed validation and preserves the reviewed tree. May use `complete:true` for a preparation-only assignment. Use `complete:false` when the assignment requires publication, even if pushing is not authorized yet. |
| `published` | Current checks and a verified receipt establish publication of the validated final head. Include its original operation and receipt paths. |
| `blocked` | Always `complete:false`, with concrete blockers, retained original and current pins, recovery evidence and the next safe step. |

An uncertain push is blocked pending read-only reconciliation of its original
operation. Never prepare or send a replacement to discover whether it applied.
Retain verified receipts even when a later readiness check fails; that failure
still blocks completion. These are cleanup-owned outcomes, not a change to the
global task-state schema. Return this result to the caller for coverage and CI
checks within its saved budget. Do not claim the caller's iteration is complete.
