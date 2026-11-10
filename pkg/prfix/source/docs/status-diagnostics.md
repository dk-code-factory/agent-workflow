# Status output details

Routine coordination uses `resume`, `result` and `progress` under [task state](task-state.md). Resume recaptures retained definitions, replaces finding bodies with `finding_count` and includes saved Git pins for comparison with the current PR. It is read-only and does not establish completeness of the current selected task set. Missing-source/removed-occurrence tasks report `refresh_required`; legacy entries report `definition_required` under `unmanaged`. Supply those current definitions once through prepare to attach replay information without resetting results or budgets. `--key` restricts saved tasks, while unfinished claims remain PR-wide.

`result --key KEY --section findings|normalized|original` validates the saved envelope and reads the requested section. It exposes historical evidence, not a current-coverage verdict. `progress --run RUN_ID --after OFFSET --limit N` returns original progress records, a total and the next offset. Read every page relevant to dispositions, acceptances and recovery. None of these views persists a rendered summary or a second index.

The remaining sections describe compatible low-level status output and `status --explain`.

## Compact status

Compact status displays saved state; it does not assess readiness. Before assessing dispositions, acceptances or recovery, read necessary original evidence and relevant progress pages. Low-level integrations can still use full `status --run RUN_ID`. Include other runs when evidence spans runs.

`progress_ref` is a JSON Pointer to the same progress array in the manifest. In run names, decode `~1` as `/` and `~0` as `~`. Each validated `result` path points to an immutable JSON envelope:

- `normalized` contains findings and skill-owned fields.
- `original` contains the exact original output in base64. Decode with `base64.b64decode(envelope['original'], validate=True)`.

Older envelopes remain in the manifest's sibling `results/` directory. Attempt history in `entries` references their filenames. A missing or damaged current envelope produces a null result path and cannot be reused in either view.

Both status views use the same lock and integrity checks. Neither changes manifests, claims or counters. Compact size does not grow with progress bodies, but still includes requested tasks, budget maps, runs and all unfinished claims. It does not reduce manifest I/O or replace current input, CI or publication checks.

## Explained status

Explained and ordinary status use the same capture and reuse decision. `blockers` reports changed inputs, explicit invalidation, noncacheable tasks or results, unfinished claims, absent results, missing artifacts, damaged results, and failed or incomplete results. Several blockers may apply.

`previous_capture` identifies the last captured components. These may belong to an unfinished attempt rather than the saved result. Compare `previous_capture_fingerprint`, `current_fingerprint` and `result_fingerprint` to distinguish them. `result_fingerprint_mismatch` also explains a current capture with an older result. The ordinary `result` field still points to validated original evidence.

A task without prior capture reports `no_previous_capture` and additions; it does not imply a change from an earlier review. Missing governing resources still block capture before claim or budget changes.

Diagnostics do not change fingerprints or coverage, allocate cycles, recover claims, or authorize reuse, publication or completion. Without `--explain`, detailed comparisons are omitted and compact output keeps its existing size limit.
