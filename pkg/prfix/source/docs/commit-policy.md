# Commit grouping and message policies

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

`dk-workflows:dk-commit-split` owns assessment, Git organization, validation and authorized publication. A commit policy supplies instructions for grouping and messages. With no configured policy, existing cleanup and message defaults apply. Explicit policies replace built-in stylistic defaults, including optional body and line-length preferences. Required repository conventions and current user constraints still govern. Settle material conflicts before rewriting.

Configure a policy through `dk-workflows:dk-config`, for example:

> Use dk-config to set my global commit policy to this prompt: squash each PR into one commit and use a short subject-only message.

> Use dk-config to select my-plugin:commit-policy as this repository's commit policy.

Save a literal policy or a provider invocation in a private JSON file:

```json
{"prompt":"Squash all PR changes into one commit. Use a short, descriptive subject without a body."}
```

or:

```json
{"skill":"my-plugin:commit-policy","instructions":"Return grouping and message instructions only.","agent":{"model":"chosen","reasoning_effort":"high"}}
```

Providers use the shared [invocation contract](invocations.md), requiring fully qualified skill identities, custom instructions, options and optional native model/effort controls. Instruction-only providers use the cleanup identity. Preflight provider installation and agent capabilities with the same shared invocation validator; unsupported requests block cleanup. Literal `prompt` policies are text and do not invoke an agent.

Apply through the [dedicated configuration agent and request history](configuration-requests.md). Existing saved-write commands require a request ID bound to the prepared operation:

```sh
python3 PLUGIN_ROOT/scripts/pr_config.py --cwd REPO commit-policy set --scope user --file POLICY.json --request-id REQUEST_ID
python3 PLUGIN_ROOT/scripts/pr_config.py --cwd REPO commit-policy show
python3 PLUGIN_ROOT/scripts/pr_config.py --cwd REPO commit-policy reset --scope repo --request-id REQUEST_ID
```

Use `--scope repo` to override the global policy. The entire policy resolves repository → upstream → global → built-in, using the existing PR repository identity resolver (`--pr PR_URL` supports fork/head and base identities). Reset restores inheritance. Show reports the effective value and its source. Malformed saved configuration blocks resolution; repair or reset it. Policies remain separate from rules, preferences, drafting selections and workflow finalizers. The owning cleanup skill also exposes `policy` in its options: null explicitly selects built-in behavior. Effective resolution combines old records and scoped options without moving either; within a scope the new option wins, and invocation-local options have highest precedence.

## Resolve and hand off

Before selected cleanup during finalization, or on direct invocation, read the effective policy. Reuse a current complete supplied handoff after verification. Otherwise resolve it here. A prompt supplies instructions directly. For a skill, resolve its exact native identity and all referenced resources through host tools; missing or ambiguous installations block cleanup. Never interpret identities as paths or shell commands.

The provider receives canonical PR identity; pinned target, diff base, head, full diff and commit history; repository requirements and templates; original user constraints and permissions; and any other context it consumes. Assign it only to define grouping and message instructions. It must not edit files, rewrite history, publish, invoke cleanup or call itself recursively. Require successful terminal native execution, original output and a report containing `complete:true`, a `findings` list, no blockers, and a nonempty `policy_prompt`. Empty, failed or incomplete reports block cleanup; do not silently fall back. Preserve the complete report and consumed sources.

Use `scripts/commit_policy.py` for deterministic context capture and handoff checks. It never invokes a provider or edits Git. Prepare a private definition with the [ordinary task fields](task-state.md). Set `skill_identity` to the selected provider, or `dk-workflows:dk-commit-split` for a prompt or the built-in policy. Include nonempty resolved `resources`, the exact read-only `assigned_instructions`, complete `instructions` and `inputs`, object `options`, `governing_inputs_complete:true`, and pinned `git` with `history:true` and `head_sensitive:true`. Include every consumed template, requirement, user constraint and indirect provider resource. Use immutable full SHAs and current context snapshots. The helper adds effective configuration and its source to the spec and captures a starting fingerprint; missing resources block preparation.

```sh
python3 PLUGIN_ROOT/scripts/commit_policy.py --cwd REPO --repository HOST/OWNER/REPO --pr PR_URL \
  spec --definition /private/policy-definition.json > /private/policy-spec.json
```

For a selected provider in a managed workflow, use the ordinary `prepare`/`finish` lifecycle in [task state](task-state.md). The task definition uses responsibility `commit-policy` and the starting spec's resolved provider identity, resource/instruction/input maps, assignment, full `options` object and Git pins. Include the private starting policy spec as an additional consumed input. Preserve its exact invocation controls when invoking the native provider. Use the retained run without review-cycle/retry flags. Check terminal execution, submit the original output and normalized report with `finish`, and satisfy the returned checklist follow-up with the same provider agent before accepting completion. On `reuse`, read and validate the original report; on `recover_required`, reconcile the unfinished claim. Regenerate the starting policy spec before preparation and validate the handoff below before accepting either new or reused provider evidence. Unchanged complete inputs permit reuse. Direct unmanaged calls still verify provider execution and report completion, and compare starting inputs before accepting the result.

Build a handoff from the starting spec and the provider's original structured report:

```sh
python3 PLUGIN_ROOT/scripts/commit_policy.py --cwd REPO --repository HOST/OWNER/REPO --pr PR_URL \
  handoff --spec /private/policy-spec.json --report /private/provider-report.json > /private/policy-handoff.json
```

Pass `--options /private/cleanup-options.json` before the command for invocation-local cleanup options, including prompt, provider or explicit null policy overrides. Preserve it on `spec`, `handoff` and `verify`. The spec retains those exact local values; changed local options block acceptance. For a prompt or built-in policy, omit `--report` and do not invoke a provider task. `policy_prompt:null` denotes built-in behavior. The helper rejects changed configuration or resources during execution and incomplete provider reports. It checks deterministic artifacts; the coordinator remains responsible for checking actual native execution and truthful consumed context.

Refresh the definition against live governing inputs before cleanup, then verify a supplied handoff:

```sh
python3 PLUGIN_ROOT/scripts/commit_policy.py --cwd REPO --repository HOST/OWNER/REPO --pr PR_URL \
  verify --definition /private/current-policy-definition.json --handoff /private/policy-handoff.json
```

Verification returns the current handoff or blocks on changes. Include the verified handoff in cleanup's `inputs.commit_policy`. Also copy all consumed provider resource, instruction and input maps into cleanup's corresponding maps with distinct labels, including the configuration source. Nested paths inside the handoff alone do not capture their live bytes. Include the returned instructions verbatim in cleanup's assignment or inputs and pass message instructions to `dk-workflows:dk-commit-message`. Regenerate and verify at claims, recording and final coverage boundaries. Resource, configuration, relevant context, requirements or constraint changes invalidate affected policy and cleanup evidence. Unchanged inputs preserve reuse; unrelated settings and eligible content reviews remain reusable.

## Apply and validate

Apply grouping instructions to the complete PR series and message instructions to every resulting commit. `dk-workflows:dk-commit-message` uses explicit message instructions ahead of its stylistic fallback. Required repository formatting, authorship, source integrity and current user constraints remain in force. A subject-only policy does not acquire a mandatory body from the built-in field conventions.

Follow [cleanup safeguards](pr/commit-cleanup.md): clean worktree, safe ownership, recovery refs, exact leases, reviewed-tree equality, relevant validation and authorized publication. The policy grants no rewrite or publication permission. It does not squash intermediate pushes. Custom finalizers continue to determine whether cleanup runs; omitting cleanup does not invoke its provider.

Keep the immutable starting handoff and reviewed head/tree as evidence of the cleanup that intentionally changed history. Verify the live resulting tree, head and receipts separately. A deliberate rewrite does not retroactively stale its own starting evidence or require an endless provider/cleanup loop. For a new assessment after further changes, refresh the policy against the new context. Preserve eligible content reviews after a tree-preserving rewrite, refresh history-sensitive work, and obtain final CI for the resulting published head. CI repairs refresh affected policy and selected cleanup before final-head observation, retaining run budgets and deadlines.
