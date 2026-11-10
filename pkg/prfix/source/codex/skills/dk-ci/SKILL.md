---
name: dk-ci
description: Check CI for the current PR commit, preserve wait deadlines and classify failures using verified flaky tests or exceptions.
---

Read and obey [private workflow reporting](../../../docs/private-reporting.md), the paramount package-wide rule for public text and private user reports.

Persist workflow state only when it reduces total API dollar cost, including the next run in a new session; follow [state admission](../../../DESIGN.md#objective-and-state-admission).

Set PACKAGE_ROOT to this plugin's root directory. Follow [the shared PR task contract](../../../docs/pr/contracts.md).

Resolve configuration for `dk-workflows:dk-ci` through [skill-owned configuration](../../../docs/skill-configuration.md) before work, including scoped and invocation-local options. This skill owns its [explicit defaults](config.json) and [option schema](config.schema.json). Preserve original additional instructions alongside the governing assignment.

Run `python3 PACKAGE_ROOT/runtime/dk-ci/scripts/ci_wait.py --pr PR_URL --head SHA`, using the full lowercase head SHA. Keep one observer per PR and head across interruptions. Use the resolved timeout, poll interval, classification timeout, absent-check grace and pending-extension settings. Pass invocation-local options with `--options FILE`; explicit CLI flags are deliberate overrides. Resume the same process when a tool yields. Read the current head and check attempts from GitHub.

The helper returns `success`, `known_flakes`, `accepted_exceptions`, `failure`, `timeout`, `no_checks` or `head_changed`. Only the first three satisfy CI, with current evidence. Restore access before using `--retry-denied`. Diagnose failures with `dk-workflows:dk-ci-flakes` and code inspection. Preserve the caller's repair and rerun budgets.

Automatic flaky-test discovery uses `dk-workflows:dk-ci-flakes` scoped settings. Preserve supplied `--upstream` for both timing and discovery; pass supplied discovery-local overrides separately with `--flake-options FILE`. Zero repair or rerun budgets disable those attempts without weakening CI acceptance.

Read the final JSON object's `result` on stdout: accepted outcomes exit 0; other terminal outcomes exit 1. Progress JSON goes to stderr. Helper exceptions exit 2 with `{"result":"error","message":"..."}` on stderr and no final stdout result. Preserve the error and treat CI as blocked.

Before rerunning a transient failure, reserve the caller's budget for that head. Use `scripts/publish.py` to prepare and publish a `ci-rerun` request with current failed-attempt evidence. Pass the verified operation to `ci_wait.py --rerun-operation FILE` to preserve the next attempt's wait window on resume. Do not rerun whole workflows as a fallback.

For managed CI and repair tasks, follow the prepared checklist and same-agent completion checks. Checklist acceptance never replaces current exact-head CI evidence, repair validation or authorized publication checks. Follow [completion checks](../../../docs/task-state.md#completion-checks).
