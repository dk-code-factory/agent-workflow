# Reusable step definitions

Declare a parameterized step once in `step-definitions`, then instantiate it
inside existing jobs with `use-definition`. The stock CLI expands each use
before validation and admission. There is no child workflow, additional workflow
nesting level, provider call or generated source file involved in expansion.

```yaml
step-definitions:
  review:
    parameters:
      prompt: {type: string, required: true}
      turns: {type: number, default: 2}
    step:
      uses: agent/run
      with:
        agent: agents/reviewer.yaml
        prompt: ${{ parameters.prompt }}
        turns: ${{ parameters.turns }}
        output: {format: text}
      continue-on-error: true
jobs:
  review:
    steps:
      - id: first
        use-definition: review
        with: {prompt: Review the changes}
      - id: second
        use-definition: review
        with: {prompt: '${{ steps.first.outputs.text }}'}
        continue-on-error: false
    outputs:
      report: ${{ steps.second.outputs.text }}
```

`parameters` uses the same types as workflow-call inputs: `string` (the default),
`boolean`, `number`, `object` and `array`. Parameters can be required or have a
literal default. Unknown bindings, missing required values and invalid literal
types are rejected during parsing. Expression bindings are checked against their
parameter types before their instance executes any effects. Optional omitted
parameters substitute YAML null when referenced.

Parameter references must occupy the whole YAML value: `${{ parameters.name }}`.
Substitution preserves arrays, objects, booleans and numbers. Inline textual
substitution, property access on a parameter and expression operators involving
parameters are unsupported. Pass the complete desired string or runtime expression
as the binding instead. Runtime expressions supplied by the caller remain ordinary
expressions in the expanded step. Fields that require static values, such as
helper executable arguments and file paths, still require static values after
substitution; fields decoded as booleans or integers require literal bindings.

Step structure and IDs must be literal; parameters customize step settings,
not structural child lists or identities. Every use requires an explicit valid `id`. Put the root ID on the use, not the
definition. A use can override `name`, `if`, `continue-on-error`, `handlerTimeout`
and any of the six lifecycle handler lists. Overrides replace the corresponding
field or complete handler list; they do not merge it. All other configuration
belongs in the definition or in its explicit parameter interface.

## Sequences and result scopes

A definition's `step` can be an existing `sequence` or `parallel` block. For example:

```yaml
step-definitions:
  capture:
    parameters:
      value: {type: string, required: true}
    step:
      sequence:
        - id: read
          argv: [echo, '${{ parameters.value }}']
        - id: copy
          argv: [echo, '${{ steps.read.outputs.stdout }}']
jobs:
  demo:
    steps:
      - {id: first, use-definition: capture, with: {value: first}}
      - {id: second, use-definition: capture, with: {value: second}}
    outputs:
      result: ${{ steps.second__copy.outputs.stdout }}
```

The expanded root keeps its use ID, and structural descendants get
`<instance>__<local-id>` IDs. In this example they are `first__read`,
`first__copy`, `second__read` and `second__copy`. Definition-authored references
are rewritten to their own instance. Statically named output schemas are copied
into each consumer before their definition-local references are rewritten. Anonymous descendants receive stable
physical-path IDs, also prefixed by the instance. Handler children retain their
existing local scopes and durable parent/handler coordinates.

Definition-authored local step references must name a step in the definition or
its handler scope. Pass caller results through parameters; those bindings are
not rewritten into the definition's scope. Names and other literal metadata are
not rewritten. Ordinary ordering rules still apply: a sequence sees preceding
children, parallel branches see only their incoming results, and descendants
become available outside a block after its join. Generated IDs that collide with
other job steps are rejected.

Definitions can use other definitions inside child step lists; cycles are
rejected even in unused declarations. Root aliases to another definition are
unsupported: use a sequence with an explicitly identified child. Expansion is
bounded to 16 nested definition uses, 4,096 total steps per workflow (including
handlers and structural blocks), and an 8 MiB admission materialization budget.
The existing YAML byte/depth limits also apply.

## Assets, recovery and inspection

Definitions are local to their containing workflow YAML. Agent definition paths
and helper file paths keep their existing resolution relative to that workflow.
For a helper in a shared job container, keep the bundle declaration on
`jobs.<id>.container.files`; a Docker step can include its own `files` declaration
in its reusable step body. Assets and native definitions pass through the same
admission and pinning as inline steps.

The immutable admitted snapshot retains declarations, expanded steps, native
agent definitions and helper implementations/assets. Resume executes the admitted
expansion and uses each instance's ordinary durable receipts. It does not reparse
changed source or reset retry/turn allowances. Each instance retains the existing
lifecycle and uncertain-effect recovery policies of its expanded step type.
Inspection records include `reuse` with the definition name, instance ID and
source lines; parameter contracts and original expression bindings belong to the
root use in the snapshot.

Expansion cannot relax placement restrictions. In particular, native agent tasks
cannot run inside shared job containers, structured helpers cannot appear in
ordinary lifecycle handlers, and structural blocks retain their existing rules
for failure propagation and supported fields.

The [PR-fix native workflow](../examples/pr-fix/workflows/native.yml) shares its
prepare and acceptance helper settings through `retained-helper`, retaining its
existing job containers, output IDs, caller-wrapper support and workflow nesting.

## Editor and alternatives

The editor preserves declarations, references and parameter bindings through
export and Undo/Redo. Imported uses show **Reusable steps** and **Step definition**;
edit declarations in YAML and bindings in YAML or **Advanced parameters**. Browser
checks cover basic names and bindings. Complete expansion, asset, type, placement
and generated result-scope admission belongs to the host runner.

Prefer reusable workflows for independent jobs, workflow-call contracts and
shared helper wrappers. Shared job containers share files and transient state;
named schemas and native agent files share contracts and instructions. Generated
YAML remains an external authoring alternative. YAML aliases and merge keys remain
unsupported, and registered Go actions still require an embedding application.
