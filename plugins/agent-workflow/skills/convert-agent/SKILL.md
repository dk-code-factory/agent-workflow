---
name: convert-agent
description: Convert Codex TOML or Claude Markdown agent definitions to canonical Agent Workflow YAML using the offline engine converter, with explicit review of omitted native options.
---

Use this skill for native agent-definition migration. Identify the source format,
source file and new `.yaml` or `.yml` destination from the request and files.
Both formats use the same engine YAML contract: required `name`, `description`
and `instructions`, with optional `model` and `reasoning-effort`.

Resolve the installed plugin root two directories above this skill. Run
`python3 <plugin>/scripts/agent-workflow check --cached-only` by absolute, quoted
path. Use `exec --cached-only --` only when `check` reports an installed verified
engine; otherwise
report the missing prerequisites or engine and use the setup skill only when
installation is requested. Conversion itself is offline and needs no provider,
workspace initialization, host settings or credentials. Do not install a host
plugin, log in or refresh credentials for conversion.

Use the deterministic CLI, rather than translating definitions yourself. Flags
precede positional arguments; this command accepts flags only. For Codex TOML:

```sh
python3 "<plugin>/scripts/agent-workflow" exec --cached-only -- agent convert \
  --from codex --input "<source>.toml" --output "<destination>.yaml"
```

For Claude Markdown with YAML frontmatter:

```sh
python3 "<plugin>/scripts/agent-workflow" exec --cached-only -- agent convert \
  --from claude --input "<source>.md" --output "<destination>.yaml"
```

Start without loss acknowledgements. Inspect the exit status and JSON report,
including `incompatible-options` and `runtime-policy-changes`. Show every
omitted field and required policy review; a failed conversion is not a completed
migration. When the user explicitly accepts an omission, repeat the command
with `--acknowledge-loss FIELD` for that exact reported field. Repeat the flag
for each accepted omission. Do not invent acknowledgements, silently drop
options, or bypass errors by manually writing replacement YAML.

Runtime permissions, tool policy, authentication and limits belong to the
target workspace's `agents.yml`; conversion does not grant authority or edit
that configuration. Never execute imported hooks. Preserved model and reasoning
settings still need compatibility with the selected workspace runtime; YAML
conversion does not prove provider acceptance or full backend feature parity.

Preserve the original file and existing outputs. The converter refuses to
overwrite a destination; select another requested path when one already exists.
Report the created path and actual converter diagnostics. Converting a file
does not authorize changing workflow references, running a workflow or publishing.
New admissions require YAML definitions. Previously admitted native/TOML
snapshots recover with their original bytes, identities and runtime bindings;
do not rewrite retained snapshots as part of migration.
