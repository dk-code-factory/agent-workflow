# Scoped preferences

Saved mutations use the [dedicated configuration agent](configuration-agent.md) and [request history](configuration-requests.md). Every saved-write command requires `--request-id ID` bound to its exact prepared operation; read commands are unchanged.

Use `dk-workflows:dk-preferences` to add, update, list, inspect or remove personal and repository preferences. Mutations route through the configuration agent, which executes preference admission directly without recursive delegation. Preferences are concrete instructions admitted against existing records and repository instructions. Conflicts are settled with the user before saving. Clear requests need no additional approval.

One versioned catalog lives privately at `${XDG_CONFIG_HOME:-~/.config}/dk-workflows/v2/preferences.json`. It is separate from workflow settings and saved PR rules. Personal and repository records share a lock and atomic replacement, so a single transaction can replace both. Revision checks prevent concurrent admissions from silently overwriting each other. Storage inside the checkout or plugin is rejected. No upstream records are inherited. Repository identity comes from the existing checkout identity helper, including its local-path fallback.

## Records and selectors

```json
{
  "owner": {"type": "repo", "repository": "remote:github.com/owner/repo"},
  "prompt": "Use pytest fixtures for shared test setup.",
  "applies_to": ["implementation", "review"],
  "scope": {
    "include": [{"type": "directory", "path": "tests"}],
    "exclude": [{"type": "directory", "path": "tests/generated"}],
    "condition": null
  }
}
```

`prepare` assigns a stable `pref-…` ID to additions. Updates include the existing ID and full replacement record. Personal ownership is `{"type":"user"}`; its paths are relative to each repository root. Omitted `applies_to` defaults to both modes. A record contains one independently applicable instruction.

Paths are case-sensitive repository-relative POSIX paths. Absolute paths, parent traversal, backslashes, empty or dot components are rejected. A directory selector `.` covers the repository. File selectors match exactly; directory selectors match recursively. Includes use OR; any matching exclusion removes a path. Matching does not check filesystem existence. Renames supply both old and new paths.

Globs are anchored at the repository root. `*` stays within one component, `?` matches one character, and character classes such as `[ab]` and `[!a]` are supported. A complete `**` component spans zero or more directories: `tests/**/*.py` includes `tests/test_root.py` and nested tests. `**` embedded in another component is rejected. Dot-prefixed names are ordinary names.

`condition` is null or a prompt that narrows applicability. An empty include list requires a condition and expresses prompt-only scope; exclusions still apply. Path matching returns candidates and never evaluates conditions. Agents evaluate conditions against the actual task, retaining reasons and applicable paths. There is no runtime conflict detector, priority or precedence ranking. Admission covers available context in the current repository; other repositories and later instruction changes are outside that check.

## Commands

Run `python3 PLUGIN_ROOT/scripts/preferences.py --checkout REPO COMMAND`. All commands return JSON except `render`, which prints Markdown. Keep proposal, path and handoff files outside the repository.

| Command | Purpose |
| --- | --- |
| `list` | Current personal and repository records and catalog revision. |
| `show --id ID` | One persisted record in the current repository context. |
| `prepare --file changes.json` | Normalize changes, generate IDs and return changes, duplicate reuse, admission context and revision. Does not save. |
| `commit --file prepared.json --request-id ID` | Validate and save the complete change set under a lock if the expected revision still matches. |
| `remove --id ID --revision N --request-id REQUEST_ID` | Remove records with the observed revision; repeat `--id` for atomic multi-record removal. |
| `resolve --mode implementation\|review --paths paths.json` | Select current personal and repository records for the mode and match task paths. |
| `render --file result.json` | Render a list, show, commit or resolve result as Markdown. |

`changes.json` is `{"upsert":[RECORD],"remove":["ID"]}`; either list may be omitted. `prepare` returns `{repository, revision, context:{records:[…]}, changes:{upsert:[…],remove:[…]}, reused:[…]}`. Context includes all current personal and current repository records, including records outside the proposed paths or mode for semantic overlap inspection. The native skill separately inspects repository layout and governing instructions. Prepare and commit enforce structure, ownership and exact duplicate reuse; they cannot establish semantic admission. Duplicate identity compares normalized records without IDs. When an update duplicates an unchanged existing record, the existing record is reused and the updated record is removed in the transaction.

`paths.json` is an array of repository-relative strings and optional `{"old_path":"old/name.py","new_path":"new/name.py"}` rename pairs. Include added, removed and intended new paths. `resolve` returns `{repository, mode, paths, records:[{record,paths,requires_condition}]}`. Each entry contains the complete record and matched paths. Prompt-only candidates can also be returned for an empty task path set. Results sort by ID for stable serialization, conveying no priority. Catalog revisions are omitted from resolution so irrelevant catalog changes do not invalidate work.

## Workflow handoffs and reuse

Use `scripts/preferences.py --checkout REPO handoff --file REQUEST.json --output /private/task-preferences.json` to generate the deterministic handoff. The request supplies `mode`, complete `paths`, consumed `condition_context` and any new `decisions`. The helper reads the existing output, refreshes selection and retains decisions only for identical records, matched paths, mode and condition context. It returns only pending conditional records for agent evaluation. Resubmit their decisions; a complete result updates the existing artifact only when its content changes. Incomplete selection never overwrites a complete prior artifact and must block dependent work. No catalog-wide snapshot or separate decision cache is saved. Keep using the same output path on the next session.

Fix, incoming review, standalone review, finding verification and direct feedback implementation all use this procedure before dispatch or local implementation:

1. Identify the task's complete paths, including rename endpoints and planned files, and call `resolve` for `implementation` or `review`. Fix coordinators select implementation preferences for edits and review preferences separately for reviewers. Finding verification uses review preferences for its own scope.
2. Evaluate each returned condition in the actual task context. Preserve complete candidate records, matched paths and decisions, including rejected candidates. For conditional records save `{id, applicable, paths, reason}`; `paths` is the applicable subset of matched paths (empty for rejected candidates or pathless prompt-only tasks). An unresolved condition needs clarification before dependent work. Unconditional entries apply to their matched paths.
3. Save a deterministic handoff such as `{"selection":RESOLVE_RESULT,"condition_context":CONSUMED_CONTEXT,"decisions":[…]}`. Include the actual context used for conditions, without generated progress, timestamps, run or cycle IDs. Pass the complete handoff downstream alongside pinned evidence and original instructions. Keep scope intact: a path-limited preference cannot become an unrestricted instruction. Agents considering edits outside the handoff's paths return to the coordinator for refreshed selection before expanding work.
4. For managed tasks include the handoff file in `inputs.preferences` and capture this contract and preference runtime as governing resources. Standalone and iterative reviewers use identical assignments and preference inputs. Before claiming, recording or reporting coverage, resolve again against current records and task paths and check the consumed condition context. Reuse unchanged decisions when their records, matched paths and consumed context are unchanged. Reevaluate affected conditions on relevant changes, then recapture affected specs through existing task-state checks. Refresh before scope expands to new paths. Do not fingerprint the entire catalog or its revision: unrelated preferences must not invalidate a task.

The native coordinator owns refreshing the handoff; the task store hashes the explicitly supplied inputs and does not discover omitted preferences. Direct feedback calls perform selection and conditions themselves when no current complete caller handoff exists. New or changed repository instructions remain governing task inputs, independent of the catalog. If a contradiction appears after admission, surface it and resolve it before affected work; retrieval never silently chooses a winner.
