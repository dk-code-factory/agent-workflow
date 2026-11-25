# Author a distributable workflow skill

Resolve requirements and obtain explicit approval of the complete implementation
and test plan before changing files. Agree whether the result is a workspace
workflow or a plugin; a plugin also needs its identity, exact engine release,
host targets, skill bindings, resources, fixtures and output location.

Use the installed absolute `scripts/agent-workflow` launcher with `exec --` before
engine arguments. For a Claude host, add `--host claude` to **every** launcher
check, ensure and exec invocation, before `--`. This selects host prerequisites;
workspace `agent-runtime` independently selects `codex-app-server` or `claude-sdk`.
The default Codex profile retains the pinned Codex compatibility check.

After plan approval, use these engine commands (flags precede NAME or DIR):

```sh
plugin init NAME
plugin init --workflow FILE NAME
plugin add-skill --plugin DIR --workflow FILE NAME
plugin check --json DIR
plugin test --json DIR
plugin build --target codex,claude --output OUTPUT DIR
```

Choose one initialization form. Existing destinations are refused. Import copies
the local declared dependency closure, including YAML agents and helper files;
it does not import workspace permissions or credentials. `add-skill` adds a
binding without overwriting existing sources. Imported fixture templates start
empty: author actual responses and assertions before `plugin test` can succeed.

Maintain `plugin.yaml`, SKILL.md instructions, workflow YAML, canonical YAML agent
definitions, declared resources and fixture cases. SKILL frontmatter owns discovery
metadata; workflow `on.workflow_call.inputs` owns input contracts. `plugin.yaml`
selects host targets and an exact engine version. Third-party plugin versions
are independent of the engine. Keep generated output outside the source tree,
with an existing parent and a new OUTPUT directory. Do not hand-edit generated
manifests, catalogs, launchers or appended invocation/recovery instructions.

`check` is structural. `test` executes the real workflow interpreter against
scripted provider/effect responses in disposable state. Assert success, rejected
inputs, failures, bounded retries, approval/decline and interrupted recovery.
Use actual engine states such as `succeeded`; `completed` is not a fixture state.
Require exact invocation counts and retained worker/session allowances during
recovery. Missing responses fail; no fixture falls back to real providers,
commands, containers or publication. Helper implementation tests and native
model/host acceptance remain separate evidence. `build` checks structure and
creates artifacts, but does not run fixtures.

The generated marketplace contains Codex and Claude catalogs pointing to the
same payload and skill identities. Its ZIP, `build.json` and `SHA256SUMS` are
local outputs; repeat builds from identical sources and targets must match.
Only declared resources and discovered dependencies are packaged. Configuration
examples and fixtures remain development sources unless explicitly declared.

## Setup and Claude-only machines

Python 3.11+ and GitHub CLI attestation verification are needed for explicit
engine setup. `ensure` verifies the exact official tag, manifest provenance,
archive digest and build identity before publishing a private versioned cache.
No latest/PATH engine fallback, provider login or credential refresh is allowed.
An explicitly selected local candidate needs both archive and digest and remains
uncertified in a separate cache; never substitute it for a verified release.

A Claude-only machine uses `check --host claude`, then explicitly authorized
`ensure --host claude`. For Claude SDK execution, Node/npm and explicit
`ensure --host claude --claude-bridge` prepare locked dependencies in a separate
writable cache and report the bridge path. Keep the verified distribution
immutable. Review the target workspace's configuration:

```yaml
agent-runtime: task
agents:
  task:
    provider: claude-sdk
    bridge: /absolute/path/reported/by/setup/bridge.mjs
    authentication: {mode: native, credential-env: CLAUDE_CODE_OAUTH_TOKEN}
    tools: []
```

Use an existing explicit subscription-token reference. An API-key gateway instead
requires `mode: gateway`, `endpoint` and `credential-env`; never put credential
values in YAML. Codex can use `authentication: {mode: api-key, credential-env:
OPENAI_API_KEY}` with `sandbox: read-only`; omitted `agent-runtime` selects `codex`.
A Codex host can execute Claude SDK and a Claude host can execute Codex when that
workspace's prerequisites are explicitly available. Neither host selection nor
plugin instructions grant runtime authority. Portable agents omit model/reasoning
overrides and use supported output settings, including `repair-attempts: 0` when
requesting structured output. Current tool, isolation and repair restrictions
remain; full managed parity is tracked separately in issue #134.

## Execution and durable recovery

Local host installation requires its own requested setup. Register the generated
marketplace directory in the chosen host and install `NAME@NAME`; do not mutate
host state merely to build or test a plugin. Native host/model acceptance is a
separately requested procedure, never an implicit PR test or release substitute.

For an authorized run, invoke the generated launcher by absolute path:

```sh
python3 /absolute/package/scripts/agent-workflow exec -- plugin run \
  --plugin-root /absolute/package --workspace /absolute/repo \
  --request-id stable-request-id --input 'task=Draft a brief' SKILL
```

For the built-in launcher's Claude profile, use `exec --host claude --` instead.
Inputs that are complete JSON retain types; quote numeric-looking strings as
JSON strings. Secrets use `--secret-env NAME=ENV`, never credential values.
Keep the installed plugin read-only. Retained assets resolve through
`${{ package.root }}`; working directories and execution policy belong to REPO.

Retain the returned run ID. After uncertain admission, inspect `status --workspace
REPO --request-id ID` before retrying. Resume the same run with `resume --workspace
REPO RUN`; do not re-run `plugin run` against an upgraded/missing installation.
Recovery preserves admitted definitions/assets, runtime settings, completed work,
gate evidence and consumed allowances. Use the current gate/evidence/answer IDs
for an explicit user decision. Repeating uncertain effects needs separate
permission; never replenish budgets or restart completed work.

## Native definition migration

Use `agent convert --from codex --input FILE.toml --output FILE.yaml` or
`agent convert --from claude --input FILE.md --output FILE.yaml`. Both produce
canonical YAML through the shared parser. Report every incompatible option and
workspace policy change. Repeat `--acknowledge-loss FIELD` only for explicitly
accepted omitted fields; imported hooks never execute. Sources and existing
outputs are preserved. New admissions reject native formats with conversion
guidance; historical snapshots retain original bytes/hashes/runtime bindings.

The convert-agent skill uses `check --cached-only` and `exec --cached-only --
agent convert ...`. This mode only verifies an existing release cache and runs
the offline converter, without gh/Codex discovery or downloads. It rejects other
engine commands, candidate caches, ensure and bridge setup. Missing/corrupt cache
is an explicit setup problem, not permission to install or bypass verification.
