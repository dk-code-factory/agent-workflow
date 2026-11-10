# Partition and commit cleanup

Read current scope, original guidance, Decisions, complete feedback and the user's exact constraints. Count added lines in the complete diff. Read private partition settings with `scripts/pr_config.py` before proposing work. The default threshold is 1,000 added lines. Moving unrelated work and splitting related work each have a separate mode: automatic, manual or disabled. Repeat the size check after every published code change.

Honor a whole-PR opt-out until explicitly re-enabled. Declining one proposal does not disable partitioning for the PR. Apply exact conditional instructions. Save their source with the reasoned decision to retain or split work.

For partitioning, separate unrelated work from required scope. Split related work only at boundaries that can be reviewed and tested independently. Automatic mode permits guarded draft-PR publication when the caller authorized iteration. Manual mode requires approval of the concrete action. Disabled mode keeps that work in the source PR.

Create branches from the verified default target. Preserve dependency order and a recovery ref. Verify that the combined resulting trees preserve the intended work. Create draft PRs with descriptions and cross-links, then remove the moved work from the source PR using its original exact lease. Save publication intent before each remote action. Reconcile uncertain creation before retrying. Do not merge, create issues or discard other agents' edits.
