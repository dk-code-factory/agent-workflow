# Workflow plugin authoring contract

`agent-workflow plugin init [--workflow FILE] NAME` creates a new directory named
NAME. `agent-workflow plugin add-skill --plugin DIR --workflow FILE NAME` imports
another skill into an existing project. Flags precede positional arguments. These
commands work offline without host CLIs, provider credentials or dependency
installation. Existing files are never replaced by scaffolding or import.

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
native agent TOML definitions (including literal selection choices and directory
references) and the helper files explicitly declared on job containers and Docker
steps. It visits nested blocks and lifecycle handlers. It preserves relative
agent/helper resolution, declared input contracts and executable modes. Reusable
job references are rewritten into a skill-specific `imports/NAME/` namespace.
The source root is the parent of the nearest `workflows/` ancestor of FILE, or
FILE's directory for a standalone file. Parent-relative agent references must
stay inside that root. Missing dependencies, remote or dynamic workflow refs,
symlinks, path escapes, unsupported definitions and output collisions fail.
Import copies no undeclared repository files. The project-level `resources` list
can explicitly declare additional contained files or directories for packaging.

Developer-owned sources are separate from future generated platform output.
Scaffolds include a portable native-agent example, provider configuration
examples in `config/` and scripted fixture source data in `fixtures/`. Imports
receive an empty fixture template that needs cases for the imported behavior.
Fixture execution and platform generation are later delivery stages; these
commands do not claim live host activation or provider acceptance.

Runtime configuration, credentials and authority belong to the target workspace's
`agents.yml`. The examples are templates only: no target configuration is created
or changed. A host target does not select an execution provider. Claude SDK
execution requires a separately installed locked bridge and an explicit token or
API-key environment reference. No login or credential refresh occurs implicitly.
