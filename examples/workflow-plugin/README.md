# A portable brief workflow plugin

This maintained example creates a text draft, waits for a durable approval, and
continues the same agent session only for `approve`. Decline skips finalization.
It returns the initial draft as the callable output; inspect the final turn in
run logs. The three-turn allowance covers both turns and is never replenished.
The package declares `references/scope.md`; config templates and fixtures remain
development sources. The workflow performs no file edits or publication.

From a clean repository checkout with its pinned Go toolchain, build the CLI and
exercise both runtime configurations without credentials or host tools:

```sh
repo_root="$PWD"
plugin_demo_tmp="$(mktemp -d)"
go build -o "$plugin_demo_tmp/agent-workflow" ./cmd/agent-workflow
"$plugin_demo_tmp/agent-workflow" plugin check --json "$repo_root/examples/workflow-plugin"
"$plugin_demo_tmp/agent-workflow" plugin test --json "$repo_root/examples/workflow-plugin"
"$plugin_demo_tmp/agent-workflow" plugin build --target codex,claude --output "$plugin_demo_tmp/first" "$repo_root/examples/workflow-plugin"
"$plugin_demo_tmp/agent-workflow" plugin build --target codex,claude --output "$plugin_demo_tmp/second" "$repo_root/examples/workflow-plugin"
cmp "$plugin_demo_tmp/first/brief-workflow-0.1.0.zip" "$plugin_demo_tmp/second/brief-workflow-0.1.0.zip"
(cd "$plugin_demo_tmp/first" && sha256sum -c SHA256SUMS)
```

On macOS, use `shasum -a 256 -c SHA256SUMS` for the last step. OUTPUT directories
must be new and their parent must exist. No generated manifest or launcher edits
are needed. `scripts/test_plugin_authoring.py` runs this path, plus starter
initialization, exact checksums and extracted payload inspection in disposable
paths containing spaces. It uses actual CLI commands and the real interpreter.
The Go maintained-example test additionally captures the generated runtime payload and its declared assets
through the production package-admission boundary; plugin check is a developer-source
check, not a command to rebuild/validate an installed output tree.

Fixtures cover both `codex-app-server` and `claude-sdk`, stale evidence,
interruption/reopen, runtime configuration changes, retained session identity and
allowances, completed-effect counts, decline, missing required input and provider
failure. Expected states use `succeeded`/`failed`. Scripted provider results do
not establish native host activation, model quality or real provider acceptance.
The fixtures never discover host executables or read provider credentials.

For your own project, `plugin init NAME` creates a portable starter. Use
`plugin init --workflow FILE NAME` to import an existing closure, or
`plugin add-skill --plugin DIR --workflow FILE NAME` for another skill. Imported
fixture templates are empty and must receive behavioral cases before tests pass.
Read [the descriptor and fixture contract](../../docs/PLUGIN_AUTHORING.md).

## Requested local installation and execution

The first build contains the marketplace directory
`$plugin_demo_tmp/first/brief-workflow` and its ZIP. Extractors must restore Unix
executable modes. Inspect artifacts before requesting installation. For a
requested local install, register that absolute marketplace directory and install
`brief-workflow@brief-workflow` using the chosen host:

```sh
codex plugin marketplace add /absolute/path/to/first/brief-workflow
codex plugin add brief-workflow@brief-workflow
```

Or in Claude Code's terminal UI:

```text
/plugin marketplace add /absolute/path/to/first/brief-workflow
/plugin install brief-workflow@brief-workflow
```

See [Claude Code installation](https://code.claude.com/docs/en/discover-plugins)
for host installation scopes. These procedures change host state and are not run
by the deterministic walkthrough or CI. No new remote repository is needed.

Engine setup is explicit: the generated payload's
`brief-workflow/plugins/brief-workflow/scripts/agent-workflow` launcher supports
`check`, then authorized `ensure`, and `exec -- ...`. It downloads only its exact
engine release after provenance/digest/build verification. Third-party plugin
version `0.1.0` deliberately differs from engine `0.2.2`.

For a Claude-only machine, install Python 3.11+, GitHub CLI attestation support,
Node/npm and the locked SDK bridge through explicit `ensure --claude-bridge`.
Use the reported writable bridge-cache path in the target workspace's `agents.yml`.
No Codex executable is required by the generated bootstrap or Claude SDK path.
Copy and review `config/agents.claude.example.yml`, setting the existing
`CLAUDE_CODE_OAUTH_TOKEN` environment reference. A gateway API-key route instead
uses `authentication: {mode: gateway, endpoint: https://YOUR_GATEWAY,
credential-env: ANTHROPIC_API_KEY}`. Never put token/key values in files.

For Codex, review `config/agents.codex.example.yml` and provide its explicit
`OPENAI_API_KEY` reference, or choose an existing native authentication route.
`agent-runtime` selects the configured workspace role and defaults to `codex`;
plugin host selection never changes it. Agents omit model/reasoning overrides
and use text outputs. Existing adapter restrictions still apply: this example
does not promise Codex managed tool subsets, tool-free repair, native filesystem
isolation for Claude SDK or full managed parity (tracked separately in #134).

An authorized invocation uses the generated launcher:

```sh
python3 /absolute/payload/scripts/agent-workflow exec -- plugin run \
  --plugin-root /absolute/payload --workspace /absolute/target-repository \
  --request-id brief-request-1 --input 'task=Draft a release note' brief
```

Retain the returned run ID. Inspect status/inputs and the retained draft before
answering `approve` or `decline` with the exact current gate/evidence IDs. After a
lost acknowledgement use `status --workspace REPO --request-id brief-request-1`;
after interruption use `resume --workspace REPO RUN`, not a new package request.
Resume after package upgrade/removal uses retained assets and admitted runtime
settings, session identity and allowances. Changed workspace configuration cannot
redirect the admitted session. Inspect uncertainty before any explicitly approved
retry. Read [the recovery contract](../../docs/PLUGIN_AUTHORING.md#packaged-execution).

Convert Codex TOML or Claude Markdown sources with `agent convert --from
codex|claude --input FILE --output FILE.yaml` before a fresh admission. Review
reported incompatible options and acknowledge each accepted loss; runtime policy
remains workspace-owned. Historical snapshots recover unchanged. Native host and
model acceptance remain a separate, explicitly requested procedure. This example
and its fixture results do not publish or certify a release.
