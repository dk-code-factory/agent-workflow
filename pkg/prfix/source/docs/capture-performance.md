# Git capture implementation and measurements

Developer reference for capture sharing and reproducible process-count probes. Agent commands and recovery rules are defined in [task state](task-state.md).

One `status` or `start-batch` command shares Git calculations for identical full commit IDs,
checkout context and Git options. Each task still captures its own resources,
instructions, input files and options, including `cacheable:false` tasks. A new
status, start, start-batch or record gets a fresh capture boundary. Mutable refs, abbreviated
IDs and revision expressions always resolve independently. Unsupported Git
layouts or indirect configuration, replacements, grafts and shallow repositories
also fall back to independent capture. Ordinary repositories and linked worktrees
check configuration, environment and affected-path attributes before reuse.
System and global paths come from the running Git installation. Git versions
without the required path variables and relative XDG configuration paths use
independent capture; the process probe skips unsupported Git versions.
Alternate attribute sources, configured worktree roots and signature-verifying
history also fall back. Index context retains a fixed-size content hash.
Sorted packed refs use fresh binary searches for ambiguous names and replacements;
unsupported packed-ref formats fall back rather than enumerate all refs.
Observed context files and their ancestry retain identity/version guards until
capture or reuse completes. Detected mutation declines sharing, including a
file restored to its earlier bytes or an optional file created and removed.

Reproduce the capture-process comparison with:

```sh
python3 -m pytest -q -s tests/test_state.py -k status_batch_process_count_comparison
```

The probe completes two reviewer specs with identical pinned Git inputs, then
runs initial status, two independent reuse starts, and final status. It compares
independent captures (42 Git processes) with batched status captures (28).
The fixture isolates Git configuration and environment so user includes and
other deliberately unsupported contexts do not disable the measured fast path.
Each optimized status uses two Git path-variable probes, two changed-path
probes and three calculations: batched commit resolution, batched tree resolution
and the binary diff. Each separate start still performs seven Git processes.
The mixed content/history/head-sensitive probe with three distinct pins measures
66 independent processes versus 54 with status batching. Reproduce it with
`python3 -m pytest -q -s tests/test_state.py -k status_batch_mixed_modes_process_count_comparison`.
Counts exclude Store initialization, initial
result recording, publication and CI. No reviewers or new cycles launch during
the measured recipe. This measures subprocess count, not elapsed time or tokens.

The batch-start comparison uses that same two-completed-reviewer recipe with
one initial status, one `start-batch` and one final status:

```sh
python3 -m pytest -q -s tests/test_batch_state.py -k start_batch_process_count_comparison
```

It measures 42 Git processes with independent captures and single starts,
28 with status batching and single starts, and 21 with status and start batching,
in ordinary and linked worktrees. The recipe requires four helper commands with
single starts and three with batch start. Setup and record commands are excluded;
the deterministic spec generator itself uses no Git processes. Tasks still
check their own resources and instructions, even when noncacheable. Later
recording and final status remain separate fresh boundaries. No elapsed-time
or token savings are claimed.
