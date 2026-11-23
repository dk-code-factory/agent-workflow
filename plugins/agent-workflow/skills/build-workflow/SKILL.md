---
name: build-workflow
description: Design, implement, validate and test an Agent Workflow YAML workflow after clarifying requirements and obtaining explicit plan approval.
---

Use the installed plugin's absolute `scripts/agent-workflow` path (two directories
up from this skill), with `exec --` before engine arguments. Check prerequisites
with `check`; use `ensure` when installation is needed and authorized by this task.
Read [syntax and examples](../../references/authoring.md) before designing YAML.
The plugin is self-contained; no separately installed workflow skills are needed.

Follow this order:

1. Inspect repository instructions, existing workflows, agent TOML definitions,
   tests and available execution tools. Discover facts from files and tools.
2. Clarify unresolved inputs, output contracts, agent responsibilities and
   dependency architecture, permissions and allowed edits, external effects,
   execution environment, time/turn/iteration limits, failure recovery and
   acceptance criteria. Ask focused questions and continue until no material
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
   workflow files, TOML definitions, scripts, fixtures or implementation files.**
   Read-only inspection is allowed while waiting. Requesting a workflow or
   approving an earlier incomplete outline does not approve a newly supplied plan.
4. Implement the approved scope. Default to `workflows/<name>.yml` and associated
   agent TOML files, following repository conventions. Keep independent jobs
   independent, use `needs` for actual dependencies, and specify result schemas
   when results are consumed as structured data. Never invent schema fields.
5. Run `exec -- validate --workspace <repo> workflows/<name>.yml`, then appropriate
   deterministic tests of the actual YAML control flow, including conditions
   and approval decisions; testing helpers alone does not test the workflow.
   Execution with models or external effects needs the
   user's authorization and an existing credential route. Fix workflow defects
   within the plan; return to clarification and approval when discoveries
   materially change the requirements or architecture.

Agent definitions cannot grant permissions: the configured `codex-app-server`
runtime owns sandbox/authentication/tool policy and limits. Preserve existing
limits and honor the agreed test/run allowance, including nested agents. Do not
add publication, login, dependency installation or unbounded retries implicitly.
Report artifacts, validation and actual test evidence, including unresolved
failures. Validation alone does not establish live acceptance.
