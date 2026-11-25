# Portable PR maintenance

[workflows/main.yml](workflows/main.yml) is the runnable entry point for PR
maintenance with the stock `agent-workflow` binary. It snapshots the request,
prepares immutable evidence, runs bounded maintenance cycles, checks completion
and readiness, and optionally merges with explicit authorization.

Python validates data and retains application evidence. The checked-in YAML owns
sequential stages, parallel read groups, independent finding verification and
fixes, same-session retries/checklists, CI observation/repair, and publication.
Execution creates no workflow YAML or agent configuration files.

Read the [setup and recovery guide](../../docs/PR_FIX.md) before use. Use an
independent clean Git clone with a `.git` directory, the PR head and target
objects, Docker, and an installed stock binary with its native Codex runtime.

Set the digest-pinned helper image in [agents.yml](agents.yml). It must already
exist locally and contain Python 3.11+, Git, `gh`, and `sleep`. Configure local Git
`user.name` and `user.email` for writing tasks. The native agent configuration
uses `workspace-write`; helper containers have a private home.

Commit the example and any configuration changes first. Create a private request
with the PR URL, exact original instructions, optional stable `logical_run`, and
explicit authorizations:

```sh
printf '\n/.pr-fix-state/\n/examples/pr-fix/request.json\n' >> .git/info/exclude
cp examples/pr-fix/request.example.json examples/pr-fix/request.json
```

With the existing GitHub credential in `GH_TOKEN`, run:

```sh
agent-workflow run \
  --config examples/pr-fix/agents.yml \
  --input request=examples/pr-fix/request.json \
  --secret-env github_token=GH_TOKEN \
  examples/pr-fix/workflows/main.yml
```

There is no separate preparation command. Preparation reads GitHub, stores an
immutable request and plan in `.pr-fix-state`, and dispatches no agents or remote
writes. Default task order, finalizers, reviewer groups, adapters, permissions,
settings, resources and limits are declared inputs in `main.yml`. Override them
with ordinary `--input NAME=JSON` values or a reusable wrapper. Agent definitions
live in [YAML](workflows/agents/); deployment settings belong in `agents.yml`.
The sample permissions and authorizations disable remote writes.

Names such as `read-feedback` select local adapter assignments. Explicit stage
objects select orchestration: `{"stage":"publication"}`, `{"stage":"reviews"}`
and `{"stage":"ci"}`. These selectors do not invoke installed plugin skills.
Legacy qualified names and `skill` objects remain accepted. Shared helper calls
use [helper.yml](workflows/helper.yml). [native.yml](workflows/native.yml) shares
one report contract across initial turns, eligible retries and checklist
continuations and selects fresh roles from a fixed admitted YAML catalog;
retry and checklist modes preserve the original native session. Result selection
and report acceptance share one retained helper operation.
[batch.yml](workflows/batch.yml) shares ordered task groups and their optional
independent verification/fix sequence across task and review stages.
[ci-poll.yml](workflows/ci-poll.yml) owns each CI
observation and delay within its bounded polling chunk.

A selected stage grants no publication authority. Every remote write needs its
configured permission and retained exact authorization. Merge additionally needs
current independent review, verification, final-head CI and repository readiness.
Readiness and merge dispatch each require complete current user and team review
requests. Any outstanding request holds merge, including a renewed request after
approval at the same head. Missing, unsupported or incomplete request data also
holds merge. The blocker identifies pending users and teams; merge authority does
not withdraw their requests or grant authority to approve. Once requests are
fulfilled or separately withdrawn through an authorized action, all ordinary
readiness requirements are checked again.
The `complete` and `ready` outputs remain distinct.

Resume an interrupted original run with stock `resume`. Engine snapshots and
retained helper bundles preserve the original graph, definitions, runtime and
sessions. Replacement executions for the same PR keep its `logical_run`, consumed
allowances and absolute CI deadlines. Unknown external effects must reconcile
before retry. Back up application state with the engine recovery bundle; preserve
historical generated closures belonging to existing admitted runs.
An uncertain merge is never resent merely because the PR still appears open;
its exact merged effect must be reconciled before the operation can complete.

For migration from the former JSON orchestration fields and deterministic
verification commands, see [the guide](../../docs/PR_FIX.md#state-backup-and-compatibility).
