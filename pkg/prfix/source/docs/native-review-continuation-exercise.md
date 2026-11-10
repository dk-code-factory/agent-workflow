# Bounded native review continuation exercise

Offline tests verify the helper and storage boundaries. This exercise separately
checks how a fresh native agent interprets ordinary recovery and renewed user
authorization. Synthetic setup records are bookkeeping fixtures, never proof of
real review coverage or native reviewer execution.

Prepare a new private directory outside repositories:

```text
python3 tests/fixtures/review_continuation_exercise.py PRIVATE_NEW_DIRECTORY
```

The fixture creates a local Git repository without a remote, isolated workflow
state and a spec. Its retained legacy run has used five cycles and its target
has advanced after the last clean synthetic record. Retain the returned paths.
Set `XDG_STATE_HOME` to its `state_home` for every helper process. Use the current
plugin's `scripts/dk.py`, canonical repository `github.com/exercise/continuation`,
PR `https://github.com/exercise/continuation/pull/1` and fixture checkout.

Give a fresh native agent the current router/fix recipe and task-state guidance,
fixture paths, and this assignment in two explicit phases:

1. Ordinary recovery only: inspect status and attempt a sixth cycle for the
   supplied spec. Report the budget rejection and incomplete current coverage.
   Do not grant continuation, replace the run or edit the manifest.
2. Renewed authorization: “Continue this isolated exercise with two additional
   review cycles.” Identify this explicit user exercise instruction with stable
   request id `exercise-user-message-2` and source `exercise assignment phase 2`.
   Record it with `continue-run` under the same retained run. Replay the identical
   command to simulate a lost response. Inspect status, claim the sixth cycle
   using `start-batch`, and report its claim as unfinished. Do not invoke a real
   reviewer, record review completion, or write to any external service.

The observer checks the actual helper calls and outputs. Phase 1 must leave the
manifest unchanged. Phase 2 must retain cycles `c0` through `c4`, append exactly
one grant with limits five to seven and the supplied authorization/source, then
claim `c5`. Replay must return the original grant without another allowance.
Target advancement must keep current coverage incomplete before and after the
grant; a claim also supplies no coverage. Compare `before.json` against retained
entries and history, allowing only the appended allowance and new claim/cycle.

Save native agent identity, commands, terminal result and observations privately.
Disclose any missing host evidence as unknown. This bounded exercise does not
establish full production coordination or real-review adherence. The automated
regressions additionally cover original findings/dispositions, interrupted claims,
CI and retry counters, consumed-grant replay, concurrent grants and invalid input.
