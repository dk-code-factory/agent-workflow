# PR configuration

Saved mutations use the [dedicated configuration agent](../configuration-agent.md) and [request history](../configuration-requests.md). Every saved-write command requires `--request-id ID` bound to its exact prepared operation; read commands are unchanged.

Use `dk-workflows:dk-config` for requests to change, inspect or explain these policies and [workflow settings](../configuration.md). During PR execution, read the policies needed by the current stage directly with the commands below: `show` for rules, drafting, completion, commit policy and per-PR partition settings; `list` for size settings and CI exceptions. No configuration-skill invocation is required. Keep existing helpers, scope precedence and private storage paths so saved choices remain effective.

Run `python3 PACKAGE_ROOT/scripts/pr_config.py --cwd REPO [--pr PR_URL] COMMAND`:

- `pr-size list|set|reset`: thresholds and separate modes for moving unrelated work and splitting related work.
- `pr-partition show|enable|disable|instructions|clear-instructions|reset`: exact per-PR instructions, opt-outs and history.
- `ci-exceptions list|add|remove|reset`: explicit test or pipeline matchers and reasons at repository or user scope. Matchers belong to the PR base repository. Other repository guidance uses fork, upstream, then user precedence.
- `rules show|set|reset` and `drafting show|set|reset`: PR-owned JSON settings.
- `completion show|set|reset`: original plain-text completion prompts.
- `commit-policy show|set|reset`: one JSON prompt or selected native skill controlling commit grouping and messages. See [commit policies](../commit-policy.md).

For `rules`, `drafting` and `completion`, use `set --scope repo|user --file FILE --request-id ID`. Their `show` commands use effective precedence and return source paths. Preserve conflicting rules and exact original instructions. A generated summary must not replace them. Drafting values identify native skills. Completion instructions cannot expand publication permission or restore actions omitted from the effective [finalizer list](../finalizers.md).

For `commit-policy`, use `set --scope user|repo --file FILE --request-id ID` with a literal `{"prompt":"original instructions"}` or a shared provider invocation: a skill name or an object with `skill`, `instructions`, `options`, `agent` and `required`. See [commit policies](../commit-policy.md). The whole policy inherits repository → upstream → global → built-in; roles and individual fields do not merge. `show` returns effective `value`, `source` and the private `file` when configured. `show --scope user|repo` inspects that scope. `reset --scope user|repo --request-id ID` removes the scoped policy and restores inheritance. Empty prompts, invalid identities, unknown keys and mixing literal prompt with provider fields are rejected. Policies live in a separate `commit-policy.json` alongside private drafting settings under XDG_DATA_HOME; changing them preserves all other settings and rules.

Every drafting value uses the shared [invocation schema](../../schemas/invocation.schema.json): a skill name or an object with `skill`, `instructions`, `options`, `agent` and `required`. This is the same format as process calls, reviewers and hooks. See the [invocation contract](../invocations.md) for native execution and model support. The [drafting schema](../../schemas/drafting.schema.json) allows `$schema` metadata; metadata never becomes a drafting role. Structural schemas for the other private stores are listed in the [schema catalog](../../schemas/README.md).

Private rules, drafting, size, per-PR partition, CI exceptions and flaky-test records live under XDG_DATA_HOME. Bundled PR skills own these settings and completion prompts. Drafting roles inherit separately. Private rules use the first nonempty repository, upstream or user record, in that order. Preserve user acceptances, conditions and opt-outs. `dk-architecture` owns architecture catalogs and settings. Use `dk-workflows:dk-config` to select reviewers.

Without custom completion instructions, verify review coverage and selected finalizer results against the exact current head and target. The default finalizer plan includes commit cleanup, description/Decisions updates, feedback replies, final CI and authorized re-review requests. A custom list replaces those finalization actions. Report skipped duties and missing readiness evidence separately from selected-plan completion, with current receipts or blockers. Default completion never includes merging.
