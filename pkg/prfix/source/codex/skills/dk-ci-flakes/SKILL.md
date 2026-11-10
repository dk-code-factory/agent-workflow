---
name: dk-ci-flakes
description: Find flaky tests linked to open issues and verify CI exceptions for specific GitHub Actions attempts.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-ci-flakes` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Run `python3 PACKAGE_ROOT/runtime/dk-ci/scripts/flaky_tests.py --repo HOST/OWNER/REPO --cwd REPO discover`, using the PR's base repository. Discovery uses resolved `lookback_days`, `max_runs` and `max_issues`. Pass invocation-local options with `--options FILE` before the command, or override them deliberately with `--days`, `--max-runs` and `--max-issues`. Replace `discover` with `list` or `sync` to refresh saved issue evidence; `list --history` also includes observations.

Read JSON on stdout. `list` and `sync` return `entries`, `checked_at`, `unavailable_issues` and `issue_errors`; discovery nests these under `verification` and also reports `missing`, `limits_reached` and `evidence_file`. All commands include `unlinked_candidates` and `registry_file`. Exit 0 does not establish that every failure is covered. Helper errors exit 2 with text on stderr.

Save the repository, head, job, attempt, test and proof that the linked issue is open. Missing logs, inaccessible or closed issues, unsupported reports and unknown failures block acceptance. A known flaky test cannot exempt an entire job.

Configured exceptions are separate from flake records. Read them with `python3 PACKAGE_ROOT/scripts/pr_config.py --cwd REPO --pr PR_URL ci-exceptions list`. Report each exception's id and reason from `entries`, with the returned `source` scope and `file`. Create issues or rerun jobs only when authorized.
