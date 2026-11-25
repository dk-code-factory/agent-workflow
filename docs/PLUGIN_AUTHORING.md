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
Workflow YAML is reserialized. Decoded defaults, prompts, command arguments
and lifecycle values remain unchanged, including leading blank lines.
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

Developer-owned sources are separate from generated platform output.
Scaffolds include a portable native-agent example, provider configuration
examples in `config/` and scripted fixture source data in `fixtures/`. Imports
receive an empty fixture template that needs cases for the imported behavior.
`add-skill` creates that template at `fixtures/NAME/example.yaml`.
Fixture execution is a later delivery stage; these commands do not claim live
host activation or provider acceptance.

## Build a portable marketplace

```sh
agent-workflow plugin build --target codex,claude --output ./dist/example ./example-plugin
```

The output directory must not exist, and its parent must exist. Both targets must
be declared in `plugin.yaml`; use `--target codex` or `--target claude` for one
host. The command captures and validates the declared source closure once. It
does not execute fixtures, install tools, download an engine, contact providers,
log in or publish.

The output contains `NAME/`, an installable marketplace with
`plugins/NAME/`, the portable root manifest and requested native manifests.
Codex uses `NAME/.agents/plugins/marketplace.json`; Claude Code uses
`NAME/.claude-plugin/marketplace.json`. Both catalogs reference the same plugin
and canonical `skills/SKILL/SKILL.md` identities. `NAME-VERSION.zip` contains that
entire marketplace directory. `SHA256SUMS` covers the ZIP, build report and every
marketplace file. `build.json` records requested targets, supported engine
platforms, minimum Python version, exact engine version, source provenance digest
and final runtime package digest. It stays outside the
runtime payload to avoid a self-referential digest.

Generated skills combine developer instructions with shared engine invocation and
durable recovery guidance. Canonical source skills receive the guidance directly.
For a source at another path, the generated entrypoint explicitly requires reading
`workflow-instructions.md` beside the original source's declared resources before
invoking the engine. Original instruction bytes and their relative resource
context are preserved; the original SKILL.md is replaced so it is not discovered
as an additional host skill. A conflicting `workflow-instructions.md` fails the
build. Developer instructions describe the workflow; generated guidance delegates
execution to `plugin run` rather than asking the host to interpret workflow YAML.

Only declared resources, discovered workflow/YAML-agent dependencies and named
generated assets are shipped. Config examples and fixtures are omitted unless
explicitly declared. Generated paths are reserved, and additional unbound
`skills/**/SKILL.md` files fail rather than silently extending host discovery.
All output paths share runtime containment, Unicode/case collision and the
complete 4096-path limit. The 32-MiB content limit applies to the runtime payload;
the distribution also contains its ZIP and reports. Paths containing line breaks
are rejected because the checksum file uses one entry per line. Accepted paths
keep their spelling.

Directories and executable files use mode 0755; other files use 0644. ZIP members
are sorted with fixed 1980-01-01 timestamps and explicit Unix modes. Identical
captured sources and target sets produce identical artifacts, independently of
target argument order, source timestamps, installation path and host umask.
Extractors must preserve the ZIP's Unix executable modes. Linux and macOS are
supported bootstrap platforms; this does not claim Windows runtime support or
live host/provider acceptance. The generated compatibility file pins the official
engine repository/workflow and an exact X.Y.Z engine release; prerelease/build
engine versions fail before output is created. Third-party plugin versions remain
independent. The built-in instruction-only release plugin keeps its separate
four-skill inventory, licensing, version coupling and certification gates.

## Packaged execution

Run a selected binding against a separate target workspace:

```sh
agent-workflow plugin run --plugin-root ./example-plugin --workspace /path/to/repo --request-id review-42 --input count=2 --input 'label="001"' --secret-env token=REVIEW_TOKEN example-plugin
```

Flags precede SKILL. Inputs use the existing run parser: a complete JSON value
keeps its type and number precision; other text is a string. Quote numeric-looking
strings as JSON strings. Secret flags retain environment references, not values.
Packaged execution requires a request ID and the durable workspace controller.
The running engine must exactly match `engine-version`.

Admission captures the validated declared closure once, before request-ID
comparison. Its versioned digest covers exact descriptor, SKILL, workflow, agent
and resource bytes, original permissions and declared empty directories. Another
fresh launch with the same request ID must capture identical bytes and match all
other request fields. Changed or missing installations fail that fresh launch.
For a lost acknowledgement, use `agent-workflow status --workspace REPO
--request-id ID` without a run positional argument. This read-only lookup returns
the original run; ordinary `resume --workspace REPO RUN` then uses its retained
binding, definitions, runtime settings, effects and turn allowances. Recovery
does not reselect the package or replenish budgets.
If recovery starts a controller from retained runtime settings, that controller
accepts recovery operations only. Stop it and start normally with reviewed current
workspace configuration before admitting fresh work.

`${{ package.root }}` names the verified retained resource tree for this run;
`package.name`, `package.version` and `package.digest` expose its admitted identity.
Relative workflow and agent references resolve inside that tree. Commands and
provider sessions still work in the target workspace. Concurrent packages have
private execution contexts. Retained files are read-only (0444, or 0555 if any
source execute bit was set), and directories are 0555; the manifest preserves
original modes separately. Resource paths must be canonical relative UTF-8 paths.
To support Linux/macOS materialization, admission conservatively rejects names
that collide after Unicode canonical normalization and full case folding,
including inferred parent directories, while preserving accepted spellings.
The complete tree is limited to 4096 files/directories, excluding only its root,
and 32 MiB of file content. This does not claim Windows runtime support.

Reusable children retain references to the same package assets. SQLite backup
contains snapshots and asset references, not external asset bytes; recovery
requires those bytes or the recovery bundle. Missing or corrupt retained assets
fail before execution. Existing uncertain-effect acknowledgements still apply.

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

## Shared verified bootstrap

Builders can call `plugins.BootstrapFiles(exactEngineVersion)` to obtain fresh
`scripts/engine_bootstrap.py` and `compatibility.json` bytes. Copy the maintained
script unchanged. A package-relative Python entrypoint sets
`sys.dont_write_bytecode = True` before import and calls
`engine_bootstrap.main(Path(__file__).resolve().parents[1], argv)`. Host plugin
versions may differ from the engine version. Generated/remapped instructions or
normalized modes change output identity: compute the final runtime package digest
from its actual files, independently of source provenance.

The invocation contract is `check`, `ensure`, and `exec -- ENGINE ARGS...`.
`check` reports an already verified cache; `ensure` obtains the exact tagged
official release and verifies digest, repository/workflow provenance and build
identity before atomic cache publication; `exec` uses that cache offline. There
is no latest lookup or PATH-engine fallback. Explicit local candidate archives
require their expected SHA-256 and use a separate cache. Python 3.11+ and GitHub
CLI are prerequisites; authentication remains explicit.

Claude SDK setup is an explicit `ensure --claude-bridge` operation with Node 22+
and npm installed. It copies the verified locked bridge sources to a separate
writable cache and invokes the engine's locked setup with lifecycle scripts
disabled. The returned `claude_bridge` path can be used in reviewed target
workspace configuration. Subsequent setup verifies the retained bridge receipt;
it never mutates the verified engine distribution, selects a provider, writes
workspace configuration, installs provider CLIs or refreshes login implicitly.
