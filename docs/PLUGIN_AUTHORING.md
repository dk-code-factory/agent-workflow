# Workflow plugin authoring contract

`agent-workflow plugin init [--workflow FILE] NAME` creates a new directory named
NAME. `agent-workflow plugin add-skill --plugin DIR --workflow FILE NAME` imports
another skill into an existing project. Flags precede positional arguments. These
commands work offline without host CLIs, provider credentials or dependency
installation. Initialization refuses an existing destination. Import preserves
existing skill, workflow and resource files and creates new paths exclusively.
`add-skill` atomically rewrites `plugin.yaml` to append its binding; descriptor
comments and formatting can change.

The version 1 descriptor uses this format:

```yaml
schema-version: 1
name: example-plugin
version: 0.1.0
engine-version: 0.2.2
hosts: [codex, claude]
skills:
  - name: example-plugin
    path: skills/example-plugin/SKILL.md
    workflow: workflows/main.yml
resources: []
```

Plugin and skill identities use lowercase letters, digits and hyphens, starting
with a letter. Each skill has a distinct identity, SKILL.md path and workflow
binding. Both versions are exact semantic versions without a `v` prefix; the
plugin version can differ from its required engine version. Initialization pins
the engine release compiled into the CLI. Unknown fields and schema versions,
duplicate identities/bindings, invalid paths and missing metadata fail with the
affected file in the diagnostic.

Each SKILL.md has YAML frontmatter containing `name` and `description`, followed
by its instructions. Its name must match the descriptor binding. Workflow
`on.workflow_call.inputs` declarations own input types, defaults and requiredness;
there is no second input schema in the descriptor. An ordinary manual entry
workflow may omit workflow_call; called reusable workflows must declare it.

Import discovers the local reusable-workflow closure with the engine's existing
parser and validator, including expanded reusable step definitions. It includes
shared YAML agent definitions (including literal selection choices and directory
references) and the helper files explicitly declared on job containers and Docker
steps. It visits nested blocks and lifecycle handlers. It preserves relative
agent/helper resolution, declared input contracts and executable modes. Reusable
job references are rewritten into a skill-specific `imports/NAME/` namespace.
For a workflow with reusable calls, import checks the exact declared references
at ancestor roots and requires one root containing the complete closure. This
preserves workspace-relative namespaces, including sibling namespaces. Ambiguous
roots fail instead of choosing different dependencies. Without reusable calls,
the source root is the parent of the nearest `workflows/` ancestor of FILE, or
FILE's directory for a standalone file. Parent-relative agent references must
stay inside the selected root. Missing dependencies, remote or dynamic workflow refs,
symlinks, path escapes, unsupported definitions and output collisions fail.
Import copies no undeclared repository files. The project-level `resources` list
can explicitly declare additional contained files or directories for packaging.

Developer-owned sources are separate from future generated platform output.
Scaffolds include a portable native-agent example, provider configuration
examples in `config/` and scripted fixture source data in `fixtures/`. Imports
receive an empty fixture template that needs cases for the imported behavior.
`add-skill` creates that template at `fixtures/NAME/example.yaml`.
Fixture execution and platform generation are later delivery stages; these
commands do not claim live host activation or provider acceptance.

Runtime configuration, credentials and authority belong to the target workspace's
`agents.yml`. The examples are templates only: no target configuration is created
or changed. A host target does not select an execution provider. Claude SDK
execution requires a separately installed locked bridge and an explicit token or
API-key environment reference. No login or credential refresh occurs implicitly.

Agent definitions use the shared engine YAML contract: required `name`,
`description` and `instructions`, optional `model` and `reasoning-effort`. New
source admissions reject Codex TOML and Claude Markdown with conversion guidance.
Use `agent-workflow agent convert --from codex|claude --input FILE --output FILE.yaml`
offline; review omitted options and required workspace runtime-policy changes.
Repeat `--acknowledge-loss FIELD` for each explicitly accepted omission. Conversion
preserves instruction text and compatible metadata, never runs imported hooks,
and never overwrites source or existing output. Historical admitted snapshots
retain their original bytes and runtime bindings without source migration.
