---
name: build-workflow
description: Design, implement, validate and test Agent Workflow YAML workflows or distributable plugins after clarifying requirements and obtaining explicit plan approval.
---

Use the installed plugin's absolute `scripts/agent-workflow` path (two directories
up from this skill), with `exec --` before engine arguments. Check prerequisites
with `check`; use `ensure` when installation is needed and authorized by this task.
Read [syntax and examples](../../references/authoring.md) before designing YAML.
For a Claude host, add `--host claude` to every check/ensure/exec invocation
before the `--` separator. This host profile never selects the workspace runtime.
The plugin is self-contained; no separately installed workflow skills are needed.

Follow this order:

1. Inspect repository instructions, existing workflows, YAML agent definitions,
   tests and available execution tools. Discover facts from files and tools.
2. Clarify unresolved inputs, output contracts, agent responsibilities and
   dependency architecture, permissions and allowed edits, external effects,
   execution environment, time/turn/iteration limits, failure recovery and
   acceptance criteria. For a plugin, also settle its identity, host targets, exact
   engine version, bound skills, declared resources, fixture cases and distribution
   destination. Host targets do not choose the execution provider. Ask focused
   questions and continue until no material
   requirement or architectural decision remains open. An incomplete request
   requires clarification; do not treat default examples as answers.
   If edit scope, output/review requirements, execution authority or limits are
   unspecified, the first response must ask focused requirement questions.
   A request to approve an inferred plan is not requirement clarification.
   Repository facts establish available mechanisms, not the user's permitted
   edit scope or acceptance criteria. Resolve those decisions before presenting
   the complete plan for approval.
3. Present a complete implementation and test plan: file paths, jobs and
   dependencies, agent definitions and permissions, data contracts, bounds,
   approval gates, recovery behavior, tests and completion evidence.
   **Wait for explicit approval of that plan before creating or changing any
   workflow files, YAML definitions, plugin projects, scripts, fixtures or
   implementation files.**
   Read-only inspection is allowed while waiting. Requesting a workflow or
   approving an earlier incomplete outline does not approve a newly supplied plan.
4. Implement the approved scope. Default to `workflows/<name>.yml` and associated
   YAML agents in `workflows/agents/`, following repository conventions. Keep
   independent jobs independent, use `needs` for actual dependencies, and specify result schemas
   when results are consumed as structured data. Never invent schema fields.
   For a distributable plugin, use `exec -- plugin init NAME` for a new portable
   project, or `exec -- plugin init --workflow FILE NAME` to import an existing
   workflow closure. Use `exec -- plugin add-skill --plugin DIR --workflow FILE NAME`
   for another binding. Review `plugin.yaml`, developer-owned SKILL instructions,
   canonical YAML agents, declared resources and fixtures within the approved
   scope. Imported fixture templates need actual cases; their empty template is
   not behavioral coverage. Do not hand-author generated host manifests/launchers.
5. Run `exec -- validate --workspace <repo> workflows/<name>.yml`, then appropriate
   deterministic tests of the actual YAML control flow, including conditions
   and approval decisions; testing helpers alone does not test the workflow.
   Execution with models or external effects needs the
   user's authorization and an existing credential route. Fix workflow defects
   within the plan. For a plugin run `exec -- plugin check --json DIR` and
   `exec -- plugin test --json DIR`, then `exec -- plugin build --target codex,claude
   --output OUTPUT DIR` with the approved targets. OUTPUT must not exist and its
   parent must exist. Test the actual YAML branches, gates, failures and recovery
   through deterministic fixtures; successful builds do not run those fixtures.
   Return to clarification and approval when discoveries
   materially change the requirements or architecture.

Agent definitions cannot grant permissions. Workspace `agents.yml` selects a
configured role with `agent-runtime` (default `codex`), using `codex-app-server`
or `claude-sdk` within its supported capabilities. Workspace runtime policy owns
authentication, tools, isolation and limits. Claude SDK requires Node and an
explicitly prepared locked bridge plus a token/API-key reference; it does not
require a Codex executable. Preserve existing limits and honor the agreed test/run allowance, including nested agents. Do not
add publication, login, dependency installation or unbounded retries implicitly.
Report artifacts, validation and actual test evidence, including unresolved
failures. Validation alone does not establish live acceptance.

For package commands, exact-version setup and recovery, read the packaged
[plugin authoring guide](../../references/plugin-authoring.md). Keep local host
installation and native model acceptance separately authorized. An authorized
package run uses `exec -- plugin run --plugin-root DIR --workspace REPO
--request-id ID [--input NAME=VALUE] [--secret-env NAME=ENV] SKILL`; retain its run
ID and recover it through status/resume instead of starting a replacement.
