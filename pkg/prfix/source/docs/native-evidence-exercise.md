# Bounded native PR evidence exercise

Issue #127 makes the existing shared initial handoff the normal coordinated path. This opt-in exercise compares two fresh native coordinators analyzing equivalent feedback, Decisions and read-comments tasks. Agents choose their collection, view and acceptance-verification operations from frozen guidance; the interface never chooses the next operation.

Both arms freeze production Python and original source evidence at `0393ad669dee3f27fdda8bd5ab2f7a5dfebb2c62`. Before uses that revision's guidance; after uses the working checkout's guidance. The fixture records interface, runtime, guidance and source hashes. The old guidance already permits sharing, so the before arm may choose it too. Equal counts are a valid observation.

## Reproduce

Choose two new directories outside repositories and initialize them from this checkout:

```sh
python3 tests/fixtures/native_evidence_exercise.py --root "$EVIDENCE_BEFORE" init before
python3 tests/fixtures/native_evidence_exercise.py --root "$EVIDENCE_AFTER" init after
```

An optional full revision after `before` or `after` selects the frozen runtime and before guidance; use the same revision for both arms. Keep each arm's frozen interface unchanged. Supply the same assignment to separate fresh native agents, substituting only `ARM`:

> Use only `python3 ARM/frozen/interface.py --root ARM COMMAND ARGS` for every read or action. Read `exercise.json`, `guidance/runtime/pr-iter.md`, `guidance/runtime/pr-review.md` and their evidence, contract and selected consumer references as needed.
>
> Coordinate feedback, Decisions and feedback read analysis in one shared initial analysis stage. Preserve complete original discussion, exact file and direct instructions, work items, settled choices, deferred work, source URLs and dispositions. Choose collection, view and verification operations from frozen guidance. Verify independently immediately before accepting each task. Reread direct instructions.
>
> Do not modify files, use a network or launch agents. Use `finish JSON` to save an accurate structured assessment: completion, tasks, work items with sources and dispositions, exact instructions, decisions with sources, and blockers. Report `summarize` metrics. Perform analysis only, without code fixes, publication or CI.

The scenario contains an unresolved retry-bound request, a resolved metrics suggestion with a deferred-work reply, a review body and a settled top-level choice to retain the public limit and avoid new configuration. Exact file instructions include CRLF bytes; direct instructions remain in `exercise.json` and must be reread separately.

| Operation | Result |
| --- | --- |
| `read PATH` | JSON with full original text, byte count and SHA-256, including instruction artifacts. |
| `collect` | Complete production handoff plus a separately stored compact receipt file. |
| `view ROLE RECEIPT_FILE` | Production analysis view; read-comments uses the feedback view. |
| `verify RECEIPT_FILE` | Production live-source and file-instruction comparison. |
| `finish JSON` | Save the native coordinator's analysis without prescribing its contents. |
| `summarize` | Observed operations, bytes, source-read counts and saved assessment. |

## Measurement boundaries

Only the GitHub JSON transport is replaced. Production collection, artifact integrity, complete pagination and consecutive-snapshot stability loops run unchanged. The transport supplies two thread pages, a paginated reply, slurped review/comment pages and metadata/head reads. One complete collection or verification performs 14 mocked transport calls: eight thread/metadata calls and six conversation/head calls. These count helper transport invocations, not individual HTTP requests hidden inside `gh --paginate --slurp`.

Initial collections and per-consumer acceptance verifications are reported separately. Every verification still reads complete stable sources; sharing an initial artifact does not eliminate those freshness reads. Tool-return bytes count serialized interface output without its trailing newline. Transport-return bytes count serialized mocked JSON. Neither measures model tokens, hidden context reads, elapsed-time improvements or production performance.

Keep private full traces, original assignments/results and source artifacts outside repositories. Compare task outcomes, source references, exact instructions, settled/deferred choices and dispositions before attributing any difference in counts to sharing. The interface accepts arbitrary native assessments; its counters alone do not establish semantic correctness or full recipe adherence. Initialization and command-line parsing failures are outside the operation trace.

This bounded analysis exercise does not exercise publication, final completion, CI or retained budgets. Those remain covered separately by the full recipe and domain suites. Existing evidence tests cover invalid artifacts, changed instructions and discussion drift at an unchanged head. No new evidence protocol or lifecycle is introduced.

## Observed sample

The committed [report](evidence/127-native-evidence-exercise.json) retains a controlled pair, its interface/source/guidance pins, operation order, original-assessment hashes and reviewed semantic outcomes. Original full traces and native assessments remain private; the report normalizes arm paths. Both agents read complete original discussion and retained equivalent meanings and sources, although they grouped work items differently. Direct and file instruction text was compared with original bytes, including CRLF and the final newline. Neither agent claimed code correctness or implementation completion.

| Controlled native metric | Before | After |
| --- | ---: | ---: |
| Initial collections | 1 | 1 |
| Independent acceptance verifications | 3 | 3 |
| Mocked transport reads | 56 | 56 |
| Mocked transport returned bytes | 16,024 | 16,024 |
| Interface returned bytes | 82,579 | 81,167 |
| Failed operations | 0 | 0 |

Both arms chose sharing, already permitted by the old guidance. This sample establishes no reduction in initial collections or transport reads; returned-byte differences are observations of these two traces, not production efficiency claims. Model/effort settings, token use and hidden context reads were not independently observed. The scenario remains a bounded analysis exercise, rather than complete native recipe acceptance.

The report also retains metrics from an earlier pilot. Both pilot agents preserved correct original artifacts but added one terminal newline to their quoted instruction text, so exact-text adherence was not established. The interface now returns reads as JSON containing original text, byte count and SHA-256; a regression check distinguishes the terminal newline from source bytes. The controlled pair used fresh agents after that correction. Pilot agents also attempted to read absent `retry.py` and explicitly retained the resulting code-verification limitation; the controlled pair made no code-verification claim.
