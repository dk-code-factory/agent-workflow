# Configuration requests

Saved changes enter through `dk-workflows:dk-config` and its [dedicated native agent](configuration-agent.md). `dk-preferences` sends mutations through the same agent while retaining its preference admission rules. Inspection/explanation use a transient native configuration agent without a request ID; ordinary workflows use direct read helpers. Neither creates history.

Request history lives in a private local Git repository at `${XDG_STATE_HOME:-~/.local/state}/dk-workflows/v2/configuration-history`, outside plugin/working repositories, with no remote. Live configuration stays in its existing stores. Exact asks, necessary clarification excerpts, typed operations, setting-level before/after changes, assessment fingerprints and write receipts avoid repeated admission and uncertain writes after interruption. Full transcripts, effective configuration snapshots and review caches are not copied here. Existing legacy values remain valid with unknown original intent.

Run `python3 PLUGIN_ROOT/scripts/config_requests.py COMMAND`. Keep input files private, outside working repositories.

| Command | Result |
| --- | --- |
| `start --ask-file ASK.txt --checkout REPO [--repository HOST/OWNER/REPO] [--upstream HOST/OWNER/REPO] [--pr URL]` | Stable request ID; main agent supplies it to native delegation. |
| `prepare --request-id ID --file PROPOSAL.json` | Validate operations and return admission fingerprint, changed setting decisions and relevant prior decision references. |
| `clarify --request-id ID --file ANSWER.json` | Retain necessary `{excerpt, question?}` and invalidate prior preparation. |
| `apply --request-id ID` | Apply admitted operations or reconcile interrupted writes under original locks. |
| `cancel --request-id ID --reason TEXT` | Keep a discoverable cancelled request; uncertain/applied writes must be reconciled instead. |
| `resume --request-id ID` | Compact status, pending operations and whether admission is reusable; no writes or full asks. |
| `show --request-id ID --section ask\|context\|operations\|decisions\|assessment\|progress\|status` | Explicit targeted evidence read; default status. |
| `history [--repository ID] [--domain DOMAIN] [--status STATUS] [--limit 20]` | Bounded compact results, including cancellations. |

A proposal starts as `{"operations":[OPERATION]}`. Reprepare saved operations with `{"operations":"retained"}`; helpers reconstruct exact inputs without asking the model to rebuild settings. Operations resolve all destinations internally and reject arbitrary paths, commands, unsupported domains and upstream/effective write scopes. Whole-workflow replacements retain a sparse edit and exact operation binding, so unrelated settings are not copied into history. One operation per destination per request; combine changes to the same workflow file in a `workflow` operation. Batch independent stores together for recoverable application.

| Domain | Operation fields |
| --- | --- |
| `workflow` | `action:set\|reset`, `scope:repo\|user`, `value` for set (existing validated workflow JSON). |
| `skill` | `action:set\|reset`, `scope:repo\|user`, fully qualified bundled `skill`, `value` for set. |
| `rules`, `drafting`, `commit-policy` | `action:set\|reset`, `scope:repo\|user`, existing domain JSON `value` for set. |
| `completion` | `action:set\|reset`, `scope:repo\|user`, exact text `value` for set. |
| `pr-size` | `action:set\|reset`, `scope:repo\|user`, partial settings `value` for set; optional `keys` list for reset. |
| `ci-exceptions` | `action:add\|remove\|reset`, `scope:repo\|user`, validated exception `value` for add; `id` for remove. |
| `pr-partition` | Existing partition `action`, `reason`, optional exact `source`, `instructions` for instructions action. Scope is the canonical PR from request context. |
| `preferences` | `action:commit`, `proposal` from existing `preferences.py prepare`; context records are not retained again. |

Example first preparation:

```json
{"operations":[{"domain":"skill","action":"set","scope":"repo","skill":"dk-workflows:dk-ci","value":{"timeout_seconds":900}}]}
```

The preview returns a `fingerprint`, `decisions` and `candidates`. Fetch original asks and diffs as needed. After native semantic assessment, prepare the same operations with:

```json
{
  "operations":[{"domain":"skill","action":"set","scope":"repo","skill":"dk-workflows:dk-ci","value":{"timeout_seconds":900}}],
  "assessment":{
    "fingerprint":"RETURNED_FINGERPRINT",
    "summary":"Requested repository timeout; checked applicable instructions and earlier choices.",
    "reviewed":[],
    "relations":[],
    "unresolved":[]
  }
}
```

`reviewed` must list every returned candidate as `REQUEST_ID/DECISION_ID`. For conflicts, `relations` entries are `{request, decision, new_decision, choice, evidence}`; choice is `replace`, `narrow`, `exception` or `preserve`, and evidence quotes or references the user's actual choice. Replacement/narrowing supersedes only that setting decision, after successful application. Exceptions preserve broader decisions. An unresolved question in `unresolved` blocks writes. Semantic assessment cannot be performed by the deterministic helper.

All existing user-facing saved-write commands (`config.py set/reset/skill-set/skill-reset`, mutating `pr_config.py` subcommands and `preferences.py commit/remove`) now require `--request-id ID`. The command's context and normalized operation must exactly match the prepared request; batches apply through `config_requests.py apply`. Read syntax and stores are unchanged. A completed request returns its original outcome and never replays a write after later settings changes.

Apply rechecks configuration, governing resources, applicable instructions, preference context and relevant earlier decisions under locks. Changed inputs require fresh preparation. During partial recovery, preparation retains the original operations, intents and receipts; it cannot change their preconditions or overwrite unexpected values. Clarification invalidates admission while preserving that progress. On recovery, original before/after hashes distinguish unapplied operations, already applied values and unexpected edits. Git intent precedes writes; a missing receipt is completed from observed desired values. Completed siblings never repeat. If relevant inputs change during partial recovery, report blocked rather than overriding them. The agent's original interpretation can be reused only while its recorded admission fingerprint remains current.

These are offline-tested contracts. Fixture native dispatch verifies routing and unavailable-delegation behavior; it does not establish actual native adherence or measured token savings. No native token evidence was collected for this change.
