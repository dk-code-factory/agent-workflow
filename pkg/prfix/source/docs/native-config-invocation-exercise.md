# Configuration invocation decision exercise

Ordinary PR tasks previously directed agents to read settings through `dk-workflows:dk-config`.
The revised guidance uses existing read-only helpers during execution and retains
the skill for requests to change, inspect or explain configuration. This exercise
checks that decision boundary separately from the offline helper tests.

The [case fixture](../tests/fixtures/config-invocation-cases.json) generalizes eight
collected request instances: five ordinary PR entry requests, including a merge
instruction, and three configuration questions. Repository names and additional
context are synthetic. Eight boundary cases cover runtime partitioning, incoming
review, explicit inspection, a settings change, preferences, a missing reviewer,
changed settings on resume and source development. Original session transcripts,
identifiers, personal settings and private operational evidence stay outside Git.

## Reproduce

Prepare two isolated package snapshots, including `codex/skills`, `docs`, `runtime`
and `scripts`. The recorded baseline is
`cf384cc1cc13295a8d264b56c1ee92f5b97e05c4`; use the revision containing the revised
guidance for the candidate. Copy the same case fixture into each snapshot as
`cases.json`. Keep expected choices and prior results outside both contexts.

Assign one fresh native agent to each snapshot, without inherited conversation or
access to the other arm. Use the same assignment with only the arm path changed:

> Read `ARM/cases.json` and the skill descriptions and applicable guidance in
> `ARM`. Treat that directory as the installed package. For each case independently,
> decide which skills, guidance and helper operations should fulfill the request.
> Read references as needed. Do not execute PR workflows, mutate personal settings,
> use the network or launch agents. Save a JSON array with one result per case:
> `id`, `skills_to_invoke`, `guidance_to_load`, `helper_operations`,
> `changes_saved_settings` (boolean), `blockers` and short `reasoning`. Include any
> configuration skill loading or invocation the guidance requires. Change only
> the assigned result file.

Score each original result with:

```sh
python3 tests/fixtures/config_invocation_exercise.py RESULT.json
```

The scorer requires complete, unique case coverage and typed result fields. It
checks configuration invocation, unnecessary proposed guidance loading, proposed
settings mutations and the missing-reviewer blocker. A failed decision returns
exit 1. Source inspection may read the configuration skill as repository content.
Configuration questions and explicit inspection remain configuration tasks.

Inspect the original helper plans separately: upstream identity must remain
consistent; current settings, scope and source evidence must reach downstream
stages; missing installations must block dependent work; changed settings need
fresh reads; an explicit merge instruction must survive disabled automatic merge
defaults without changing those defaults. The scorer does not interpret these
free-text plans or establish their execution.

## Recorded sample

The [normalized report](evidence/config-invocation-decisions.json) retains fixture
and original-result hashes and every scored decision. One independent Codex agent
evaluated all 16 cases in each arm; cases were not separate agent trials.

| Declared decision | Baseline | Revised |
| --- | ---: | ---: |
| Cases passing the scoring boundary | 7 / 16 | 16 / 16 |
| Passing collected request instances | 3 / 8 | 8 / 8 |
| Unnecessary configuration skill invocations | 9 | 0 |
| Configuration questions correctly using the skill | 3 / 3 | 3 / 3 |

The revised plans retain direct settings reads, preflight blockers, current-policy
refresh and separate explicit merge permission. Offline helper regressions verify
scope inheritance, unchanged store bytes and modification times, refreshed
selections, missing-skill results and exact per-PR partition instructions/opt-outs.

These are reported choices in a bounded native guidance exercise. They are not
actual invocation counts, a complete PR run, token measurements or an estimate of
production frequency. The revised agent necessarily inspected configuration
guidance while evaluating the collection; proposed per-case loading is separate
from that evaluator's own reads. Claude copies are synchronized, but this sample
does not establish Claude execution behavior.
