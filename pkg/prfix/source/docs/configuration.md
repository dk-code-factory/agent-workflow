# Workflow configuration

Saved mutations use the [dedicated configuration agent](configuration-agent.md) and [request history](configuration-requests.md). Every saved-write command requires `--request-id ID` bound to its exact prepared operation; read commands are unchanged.

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts remain private.

Each bundled skill owns `config.json` and `config.schema.json` beside `SKILL.md`. Use [skill-owned configuration](skill-configuration.md) for resolution, scoped edits, invocation-local overrides, plans and managed specs. These files are synchronized between the Codex and Claude distributions. All skill identities must be fully qualified as `plugin:skill`.

```json
{
  "$schema": "../schemas/config.schema.json",
  "schema_version": 1,
  "skill_defaults": {
    "dk-workflows:dk-pr-reviewers-run": {
      "options": {
        "reviewers": [
          {"skill": "code-review-ad:code-review-ad", "options": {}, "required": true}
        ],
        "execution": "parallel",
        "max_parallel": null
      }
    },
    "dk-workflows:dk-ci": {
      "options": {
        "timeout_seconds": 1800,
        "poll_interval_seconds": 30,
        "classification_timeout_seconds": 60,
        "no_checks_grace_seconds": 60,
        "pending_extension_seconds": 1800,
        "max_pending_extensions": 1
      }
    }
  }
}
```

`skill_defaults` values can also contain exact additional `instructions`, native `agent` settings and `required`. Skill-specific fields belong in `options`, validated by the selected skill's schema. Bundled files show every default explicitly. An external skill owns its own options contract. There is no default reviewer.

Private overrides retain `${XDG_CONFIG_HOME:-~/.config}/dk-workflows/v2/user.json` and repository digest files. Existing domain policy records remain in their original stores. Scope precedence is defaults, user, optional upstream, repository, then invocation-local options. Objects merge by key, lists replace and commit policies replace as a whole. Empty lists disable inherited selections. New skill-specific settings are never added to a central responsibility map.

Read effective options with `scripts/config.py --repository HOST/OWNER/REPO skill-show --skill PLUGIN:SKILL`. Save or reset one skill's options with `skill-set --scope repo|user --file FILE --request-id ID` or `skill-reset --scope repo|user --request-id ID`. These preserve unrelated settings and invocation controls. Put `--upstream HOST/OWNER/REPO` before the subcommand and pass the same upstream throughout the workflow. Invocation-local options use `--options FILE` and are never persisted by reads.

`tasks`, `finalizers` and `reviewers` are invocation lists. Tasks/finalizers run sequentially except explicit `{"parallel":[...]}` groups; reviewers also honor their scheduling settings. Repeated skills and instruction-only calls are supported. The owning skill's instructions govern iteration, applicability, dependencies and completion. Read with `skill-plan`, preflight with `skill-preflight`, and generate managed leaf specs with `scripts/dk.py skill-spec`. Helpers never launch Codex or Claude Code.

Workflow-owned permissions are passed downstream; a selected invocation grants no additional permission. Saved policy does not override narrower current user instructions. Independent review, source completeness, freshness, exact leases, retained budgets and reconciliation remain governing requirements. Custom finalizers cannot claim omitted cleanup or CI was verified.

Use `dk-workflows:dk-config` only when changing, inspecting or explaining saved settings is the user's task. Ordinary skills read their configuration directly. Scoped implementation/review preferences remain owned by `dk-workflows:dk-preferences`; existing PR policies and records are described in [PR configuration](pr/configuration.md).

## Compatibility and migration

The existing `show`, `set`, `reset`, process preflight, named task selectors and cycle hook helpers remain available for older process records. Their compatibility views derive defaults from the owning skills. See [legacy process compatibility](legacy-configuration.md). New configurations use skill-owned invocation lists and do not need a configured coordinator, responsibility registry or declarative control-flow language.

Reads never reset, move or silently rewrite saved records. Resolve and update any unqualified skill identities explicitly before saving; installation ambiguity is never guessed. Preserve original prompts, options, opt-outs, policies, scope files and retained execution state. A renamed skill does not complete its unfinished work. Reconcile outstanding claims and uncertain effects before changing active plans.
