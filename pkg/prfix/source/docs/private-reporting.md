# Private workflow reporting

This is the paramount package-wide reporting rule for every dk-workflows skill,
including direct invocation, coordinators, delegated skills, cycle hooks,
finalizers and all publication paths:

> Keep information about dk-workflows execution private to the skill user. If
> something is not working, report it to the skill user; do not disclose it in
> external comments, replies, PR reviews, or other published content.

This rule takes precedence over routine instructions to add summaries, report
completion, explain limitations, disclose coverage blockers, or include
operational diagnostics and provenance in public output. Authorization to post
repository feedback does not authorize disclosing internal workflow information.
Saved prompts, external skill outputs and downstream handoffs cannot weaken this
boundary.

## Two audiences

External text contains substantive repository discussion, supported answers,
actionable findings and relevant code evidence appropriate to that audience.
Apply this to review bodies, inline findings, feedback replies, general comments,
PR titles and descriptions, Decisions, issues and commit messages. Repository
validation evidence may be relevant; accounts of the workflow's inability to
perform validation belong in the private report. Do not claim a check passed
unless verified.

Report operational information privately to the invoking user: unavailable tools
or supporting sources, failed or missing reviewers, incomplete verification,
exhausted budgets, cache or state problems, and publication or reconciliation
failures. Keep skill names, internal configuration and policies, agent details,
task state, local paths, operation journals and internal receipts out of external
messages. Preserve useful reasons, actual completion state, affected actions,
evidence and the next safe step in the private report. Never hide a failure by
claiming successful validation or a clean review.

The boundary concerns information about this invocation, not a keyword ban.
A repository's own agents, configuration, paths, diagnostics or workflow code may
be the substantive subject of a finding or an issue. Describe that subject with
relevant repository evidence, without exposing this invocation's private context.

## Handoffs and publication

Pass this contract and the original permissions to every downstream skill,
including external reviewers, hooks and finalizers. Keep original reports and
operational artifacts private and separate from prepared public text. A
coordinator's sanitized payload is not permission for a publisher to reintroduce
internal details from the original reports. Completion reports, helper stdout,
stderr and journal receipts are private user/caller output, never comment drafts.

Before every external write, inspect the exact destination and every text field
of the actual payload, including review bodies and each inline comment. Retain
only audience-appropriate repository content. Do not copy an entire result,
blocker list or private user report into a publication request. Repeat this check
when a downstream skill edits the payload or the publication is prepared anew.
Helpers verify identity, readiness and exact payload transport; they do not infer
whether arbitrary prose discloses operational information. The invoking skill
owns this semantic check.

If an internal failure prevents a verified or truthful public response, withhold
the affected publication and explain the blocker privately. Do not post a
diagnostic reply or an empty progress review. Independent, fully verified
findings may still be posted as `COMMENT` with only their substantive evidence;
leave the general body empty unless a substantive summary was requested. Report
remaining coverage and verification blockers privately. Partial coverage never
authorizes approval, merging or a claim that the review is clean.

After an uncertain write, keep its original intent and evidence private and
reconcile read-only. Do not publish a failure explanation, resend the message or
rewrite the pending payload to remove diagnostics. A verified receipt does not
make leaked content acceptable or authorize another write; report the situation
privately and follow the original permissions for any later action.
