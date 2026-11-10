# Native invocations

Follow [private workflow reporting](private-reporting.md). Operational reports, blockers, diagnostics, handles and receipts go only to the invoking user or private caller, never into external text. Pass this governing contract downstream; publication permission does not authorize disclosure.

New skill-owned lists and compatibility process calls use [invocation.schema.json](../schemas/invocation.schema.json). The shared Python validator is `runtime/invocation.py`. Process `coordinator` calls, purpose-based `tasks` (including `verify_findings`), `options.reviewers`, hooks, finalizers, drafting selections and commit policy providers all reference this schema instead of defining their own agent formats.

A string selects an installed skill:

```json
"dk-workflows:dk-pr-description"
```

The full object supplies a prompt, skill options and optional agent controls:

```json
{
  "skill": "code-review-ad:code-review-ad",
  "instructions": "Review authentication changes and check permission boundaries.",
  "options": {"focus": "security"},
  "agent": {"model": "gpt-6.1-sol", "reasoning_effort": "high"},
  "required": true
}
```

| Field | Contract |
| --- | --- |
| `skill` | Exact installed `plugin:skill` identity; qualification is mandatory. Paths and `/skills/` prefixes are rejected. |
| `instructions` | Original nonempty task prompt, preserved verbatim. It specializes the assignment alongside governing skill and process instructions. |
| `options` | Object passed to the selected skill. Reviewer selections inside `options.reviewers` use this same invocation format. |
| `agent` | Presence requests a separate native agent. An empty object delegates with inherited host defaults. |
| `agent.model` | Exact model supported and available on the native host. |
| `agent.reasoning_effort` | Supported effort for the selected or inherited model. |
| `required` | Defaults to true. Incomplete optional work must be reported and does not satisfy a required process obligation. |

Supply `skill`, `instructions`, or both. A bare string always means a skill name, never a prose prompt. For instruction-only work use `{"instructions":"Read current Decisions."}`. Without `agent`, the process follows normal execution; native reviewers and finding verifiers still use independent agents where their recipes require them. Selecting a different model requires a separate native agent, not an inline prompt that purports to switch the current model.

Shared `skill_defaults` contain the same settings fields except `skill`, which is selected by the map key. Objects merge recursively; local values override shared values and lists replace. A model or prompt change is a governing-input change. Distinct task responsibilities remain distinct even when they select the same skill. An unavailable or renamed installation must be resolved explicitly; helper code does not inspect a host installation registry.

## Native dispatch

1. Resolve the skill with native tools and record its exact identity, resource paths and selected options. Verify any delegation, model and reasoning request using [configuration preflight](configuration.md). Before claiming independent review work, also resolve occurrence-specific execution requirements and verify current host evidence transport with [native evidence preflight](native-review.md#evidence-capability-preflight). Require `ready:true`; delegation alone does not establish observation support. An unavailable or ambiguous selection blocks work, including an optional selected skill. Never silently drop unsupported controls or substitute another model.
2. Read the selected skill and its governing references. Supply the original process and user instructions, exact custom prompt, pinned inputs, scoped preference handoff, assigned responsibility, permissions and completion contract. Skill options and model controls are separate; do not forward agent settings as ordinary skill options.
3. Claim managed work with `prepare --file REQUEST.json` through [task state](task-state.md). New lists use descriptors with `kind:"skill"`, owning `skill`, `selection:"tasks"|"finalizers"|"reviewers"` and flattened `index` under [skill-owned configuration](skill-configuration.md), preserving invocation-local options and the retained run for finalizers. Use managed `kind:"hook"` descriptors for [legacy hooks](cycle-hooks.md) and `kind:"finalizer"` for legacy saved finalizer plans. The helper captures defaults, schemas and resolved options, adds the exact custom prompt alongside the original assignment, and returns each assignment, claim and checklist. Use the [policy helper](commit-policy.md) for provider context and handoff checks. Definitions supply native `skill_identity`, resources, task responsibility and assignment, instruction/input maps and execution options. Submit original output through `finish` and satisfy the returned checklist follow-up before accepting completion. Low-level spec/start/record commands remain [compatibility APIs](task-state-low-level.md) for integrations; they do not attach managed checklists on their own.
4. On `invoke`, honor `options.invocation.agent` with a separate native agent when present, passing its exact model and effort to supported native controls. Use a fresh task-scoped context when the host requires it for model overrides. If the host or a fixed agent role cannot honor the requested settings, report the blocker before launch. Otherwise follow the process's normal execution. An instruction-only invocation uses the owning process identity without recursively launching the full process.
5. Save the native handle and actual execution outcome in operational progress. Wait for terminal completion. Preserve original output, explicit completion, findings and blockers. Reconcile interrupted agents and uncertain writes before recovery. The [native handoff contract](native-review.md) applies whenever a separate agent is used.

The helper never launches a CLI model session or implements a second agent runner. Native host tools own execution. `options.invocation` retains the complete normalized call; `options.configured` contains selected skill options; `options.execution` contains caller execution context. Process limits are also captured and retained by the task store. A successful definition or preflight is not an execution receipt.

Named task specs retain `options.configured_task` and select by `--task NAME`, so task-map ordering and unrelated task edits preserve eligible reuse. When a definition omits `responsibility`, the task name supplies it; `verify_findings` uses the established `finding-verification` responsibility. Explicit responsibilities must distinguish separate duties. Regenerate affected specs after task, prompt, options, model, permission or consumed-context changes. Only the selected invocation and applicable policy enter its configuration fingerprint.

For standalone tasks with explicitly supplied definitions, preserve the same normalized invocation in `options.invocation` and capture this contract, runtime and schema. The shared validator applies at task creation and capture. Independent verification uses the `finding-verification` responsibility and the verification-only assignment in the owning incoming-review skill; it must not edit, publish or recursively coordinate a new review.
