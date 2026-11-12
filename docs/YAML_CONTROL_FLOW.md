# Parallel and sequential YAML step blocks

A job's `steps` can include `parallel` and `sequence` blocks. Each parallel
branch is one ordinary step or a block. Blocks can nest, and the next step in
the containing sequence starts only after a successful join.

```yaml
jobs:
  review:
    steps:
      - id: reviews
        parallel:
          - id: correctness
            uses: agent/run
            with:
              agent: review_correctness.toml
              prompt: Check correctness.
          - sequence:
              - id: security
                uses: agent/run
                with:
                  agent: review_security.toml
                  prompt: Check security.
              - id: verify
                uses: agent/run
                with:
                  agent: review_security.toml
                  prompt: 'Verify these findings: ${{ steps.security.outputs.text }}'
      - id: combine
        uses: agent/run
        with:
          agent: task.toml
          prompt: |
            Combine these findings:
            ${{ steps.correctness.outputs.text }}
            ${{ steps.verify.outputs.text }}
```

Blocks accept `id`, `name`, `if`, and exactly one nonempty `parallel` or
`sequence` list. Agent, command, action, input, and `continue-on-error` fields
belong to ordinary steps. A leaf's `continue-on-error` keeps its existing
meaning: an ordinary handled failure permits its branch to complete; storage,
integrity, ownership, and other hard failures still propagate. Existing jobs,
`needs`, repeats, and reusable workflow calls retain their syntax and behavior.

## Identity, scope, and results

All explicit and implicit step IDs, including blocks, must be unique within
their job. An anonymous top-level step keeps its existing `step_1` style ID.
Nested IDs follow their physical declaration path, for example
`step_1_parallel_2_sequence_1`. Explicit ancestor IDs do not change that path;
use explicit IDs for results that should remain stable when moving steps.

Each parallel branch receives a private snapshot of the inputs, event,
metadata, dependencies, and completed incoming step results. It cannot read
another branch's in-flight results, even if that other branch finishes first.
Within a sequence, later children can read earlier completed children. Static
result-reference admission checks these scopes, including legacy
`steps.<id>.outputs` references in jobs with blocks.

After a join, all recorded descendant results are exposed in the containing
job's `steps` map. Job outputs, repeat conditions, declared dependent jobs, and
workflow-call outputs can reference them through the existing result paths.
Skipped blocks have no descendant results. Each parallel block also exposes
`steps.<block-id>.outputs.branches`, an array of its direct branches' `id` and
`status` in declaration order, independent of completion order. The job's
`steps` map remains keyed by ID; it has no execution-order semantics.

## Failure, capacity, and recovery

An unhandled branch failure cancels sibling work and waits for every started
branch to stop before propagating the combined error. Successful results,
partial outputs, failed attempts, and uncertainty remain recorded. The next
sequential step is not admitted. A pending `input/wait` suspends its branch;
other branches finish and the joined block remains waiting until the gate is
answered. Completed sibling work is reused when the workflow resumes.

Structural blocks consume no execution slot. Ordinary agent and command
steps use the existing shared host capacity, including nested blocks and
reusable workflows; capacity one therefore cannot deadlock on a parent block.
Blocks do not grant additional tools, credentials, filesystem access, or
publication permissions.

Durable stores retain the admitted nested definition, per-child attempts,
and block admission/completion observations. SQLite marks block attempts as
effect-free orchestration. On restart, orchestration rebuilds the join from
child receipts; completed effects are reused. Failed or uncertain ordinary
child effects retain the existing explicit `--retry-uncertain` requirement.
A lost join checkpoint alone does not authorize replaying children and does
not require that acknowledgement. Journal implementations without the SQLite
block extension use the existing attempt interface and may conservatively
require acknowledgement after losing the orchestration phase write.
