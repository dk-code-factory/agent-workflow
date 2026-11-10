# Legacy cycle hooks

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

New skill-owned configuration places custom work directly in ordered task lists; see [skill-owned configuration](skill-configuration.md). This page documents the retained process-hook compatibility API. It does not add hidden hooks to skill-owned plans.

Hooks are ordered [invocations](invocations.md) before or after a new review cycle in `pr-fix` and `pr-review`, including routed calls. Standalone supporting skills do not run these hooks. Each list inherits independently; a supplied list replaces that phase and `[]` clears it.

```json
{
  "processes": {
    "pr-fix": {
      "hooks": {
        "before_cycle": [
          {"instructions": "Read current Decisions and identify constraints for this pass."}
        ],
        "after_cycle": [
          {
            "skill": "team:check-invariants",
            "instructions": "Check affected invariants and return evidence and completion.",
            "options": {"focus": "changed-code"},
            "agent": {"model": "gpt-6.1-sol", "reasoning_effort": "high"}
          }
        ]
      }
    }
  }
}
```

There are no user-supplied hook IDs. A string always selects a skill, such as `"team:check-invariants"`. Put a prompt in an `instructions` object. Presence of `agent` requests a separate native agent; otherwise instruction-only hooks run in the current process agent and skill hooks use normal native skill execution. Resolve every selected skill and verify requested agent controls in preflight before work.

## Boundaries

| Phase | `pr-fix` | `pr-review` |
| --- | --- | --- |
| `before_cycle` | After inspection, initial feedback fixes and scope assessment, immediately before review. | After inspection and pinning, immediately before discovery. |
| `after_cycle` | After successful review, finding dispositions, justified fixes, publication and scope refresh; before another cycle or cleanup. | After successful discovery, independent verification and finding dispositions; before review publication, approval or merging. |

Run each phase sequentially. Hooks are required by default. Failed, incomplete or interrupted required hooks block the next boundary. Report incomplete optional hooks separately; optional status never permits uncertain effects, stale coverage or incomplete mandatory checks to be ignored. `after_cycle` is a successful-cycle step, not a failure/finally handler. Save failed-cycle evidence without running its after phase.

Reuse current reviewer results without opening a new cycle or running hooks. Before a new cycle, check its retained budget and save its cycle ID before the first hook. An interrupted before phase resumes that occurrence even when review has not started. A pending after phase must finish even when reviewers are now reusable. Same-cycle reviewer retries do not repeat completed hooks. Later cycles and new runs have their own occurrences.

Hooks inherit the process's current constraints and cannot expand edit or publication permission. Pass those constraints and complete pinned inputs to the hook. Skill hooks receive their exact custom instructions and options alongside the selected skill guidance. After effects change source, scope, discussion or instructions, refresh affected inspection, scope, review, verification and CI inputs. An after hook that changes reviewed code requires another review pass within the remaining budget. Hooks never bypass cleanup or live completion checks.

## Define and resume

Prepare one hook at its zero-based list position through the shared [managed task lifecycle](task-state.md):

```sh
python3 PLUGIN_ROOT/scripts/dk.py --repository HOST/OWNER/REPO --pr PR_URL \
  prepare --file /private/hook-request.json
```

Supply the native resolved `skill_identity`, `resources`, `instructions`, `inputs`, execution `options`, and task-capture flags. For an instruction-only hook, use the owning process's resolved skill identity. For a selected skill, use its exact native identity. The helper supplies responsibility and custom assignment, records explicit live configuration source paths, and captures the invocation and configuration schemas and runtimes as governing resources. Preparation and finish recheck the effective invocation, policy and phase list without making unrelated config edits repeat completed hooks. Set the descriptor's `process` for `pr-review` (the default is `pr-fix`) and `upstream` when configuration inherits it. Save this request as `/private/hook-request.json`:

```json
{
 "run": "RUN_ID", "cycle": "CYCLE_ID",
 "tasks": [{"kind": "hook", "phase": "before_cycle", "index": 0, "definition": {
  "skill_identity": "dk-workflows:dk-pr-fix",
  "resources": {
    "skill": "/installed/dk-workflows/codex/skills/dk-pr-fix/SKILL.md",
    "hook_recipe": "/installed/dk-workflows/docs/cycle-hooks.md"
  },
  "instructions": {"user": "/private/user-instructions.txt"},
  "inputs": {"context": "/private/cycle-start-context.json"},
  "options": {},
  "governing_inputs_complete": true
 }}]
}
```

Capture all governing resources, including indirect guidance. Keep immutable cycle-start context separate from live files the hook changes. For Git-mutating hooks, use pinned Git inputs from an immutable checkout or describe the pinned starting state in an immutable context artifact. Use `cacheable:false` if governing capture is incomplete. Live captured-input drift rejects recording and requires explicit reconciliation.

Prepare hooks individually with the retained run and cycle in the request, without retry or CI budget flags. The helper uses the cycle for hook identity without consuming reviewer-cycle budget. The runtime generates an occurrence identity from process, run, cycle, phase and list position. Identical hooks at different positions are separate occurrences. The task fingerprint includes exact instructions, options, model and reasoning settings. Follow the returned lifecycle action; retain the original handle and output and submit explicit completion with `finish`. Follow its `recheck` gate before advancing. Check saved completion evidence before repeating noncacheable work.

The low-level `hook-spec` and `start`/`record` commands remain [compatibility APIs](task-state-low-level.md) for integrations. They do not attach a managed checklist on their own. Workflow agents use `prepare`/`finish`, including for legacy configured hooks.

## Changes to a retained list

The first claim stores the effective phase list. An unchanged list resumes from its saved task receipts. Inserting, removing, reordering or changing a hook in that occurrence blocks new claims until its prior effects are reconciled. Keeping the same position never makes changed instructions count as completed.

Inspect `status --run RUN_ID`, the original results and native handles. Confirm interrupted agents stopped and reconcile their writes. Record their actual outcomes or explicitly recover/invalidate their claims. Then acknowledge the new list with the observed snapshot hash from `status.hook_phases`:

```sh
python3 PLUGIN_ROOT/scripts/dk.py --repository HOST/OWNER/REPO --pr PR_URL \
  reconcile-hooks --process pr-fix --run RUN_ID --cycle CYCLE_ID \
  --phase before_cycle --expected SNAPSHOT_HASH \
  --reason "Stopped the earlier agent, inspected its effects and reconciled the changed list."
```

This replaces the current phase snapshot after an explicit reasoned acknowledgment, without appending duplicate lists, deleting original results, resetting budgets or claiming any hook completed. A stale snapshot or active hook claim blocks reconciliation. Removing every hook can be reconciled with the same command. Completed hooks that remain unchanged at the same position retain eligible receipts; changing a later hook does not repeat them. Changed or moved tasks may run again only after deliberate reconciliation. Restoring the original unchanged list also permits normal recovery. Never automatically replay a hook with uncertain effects.

Use managed preparation and finish with each hook’s prepared checklist. After its initial completed report, handle `recheck` through the shared [completion checks](task-state.md#completion-checks) with the same agent (the coordinator for instruction hooks). Advance in order only after acceptance. Mandatory checklist exhaustion blocks dependent stages; best-effort omissions never excuse hook execution failure.
