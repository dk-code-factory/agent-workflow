# Exact-version installation

Python 3.11+ and GitHub CLI with attestation verification are required. The
supported Codex version is declared in `../compatibility.json`. The default
launcher profile checks that version; a Claude host uses
`--host claude` on every check/ensure/exec invocation before `--`, without a
Codex prerequisite. The launcher never installs or changes Codex, logs in, or
uses an engine from PATH.

Register the marketplace at an exact release tag, for example:

```sh
codex plugin marketplace add dk-code-factory/agent-workflow --ref v0.2.2
codex plugin add agent-workflow@agent-workflow
```

On surfaces using the Plugins Directory, select this marketplace and install
Agent Workflow there. The separate network-runtime plugin is optional and has
its own installation. Upgrades require installing a newer tagged plugin; the
engine version always equals the plugin version. See the official
[plugin packaging documentation](https://developers.openai.com/plugins/build/plugins).

The portable release ZIP unpacks to `agent-workflow/`, containing `plugin.json`,
the `.codex-plugin/plugin.json` and `.claude-plugin/plugin.json` manifests, skills,
references and Python scripts. An engine distribution contains the same plugin
under `plugins/agent-workflow` and a local marketplace catalog.

`check` performs local inspection only. `ensure` downloads assets exclusively
from `dk-code-factory/agent-workflow` at the packaged version tag. GitHub CLI
verifies release-manifest attestation against `.github/workflows/release.yml`,
the exact tag and commit. The manifest authenticates the platform archive digest;
build.json and the executable's version identity must agree before installation.
The entire extracted distribution is retained and checked on every use.

Cache: `${XDG_CACHE_HOME:-~/.cache}/agent-workflow/release/<version>/<os>-<arch>`.
A valid cached distribution needs no network access. Corruption fails explicitly;
remove only the affected version/platform directory before reinstalling.
Installation locks and atomic rename prevent partial installs. Stale staging
from killed downloads is removed under the next install lock. The cache root
must be private (0700), owned by the caller, with no symlink path components.

## Development candidate

Only after explicitly selecting a local archive and its expected SHA-256:

```sh
python3 /plugin/scripts/agent-workflow ensure \
  --candidate-archive /local/agent-workflow-0.2.2-linux-amd64.tar.gz \
  --sha256 EXPECTED_SHA256
python3 /plugin/scripts/agent-workflow exec \
  --candidate-archive /local/agent-workflow-0.2.2-linux-amd64.tar.gz \
  --sha256 EXPECTED_SHA256 -- version
```

This reports **UNCERTIFIED CANDIDATE** and uses
`candidate/<version>/<os>-<arch>-<digest>`. It verifies local digest and build
identity but makes no release certification claim. Release mode never searches
this cache and never switches to it on a download or verification failure.

## Claude host and offline conversion

The repository also contains a Claude marketplace catalog. For a requested local
installation, register the distribution directory as a local Claude marketplace
and install `agent-workflow@agent-workflow`. This does not select the execution
provider; workspace `agent-runtime` does that separately. Explicit
`ensure --host claude --claude-bridge` needs Node/npm and prepares locked SDK
dependencies in a separate writable cache. Use its reported bridge path and an
explicit credential environment reference in the target workspace; no Codex
executable or credential refresh is required for the Claude SDK route.

`check --cached-only` inspects a verified release cache without external tool
discovery. `exec --cached-only -- agent convert ...` runs only the offline converter
from that cache. It refuses missing/corrupt/mismatched receipts, candidates,
setup/bridge requests and other engine commands. It never downloads an engine.
See [plugin authoring](plugin-authoring.md) for the full flow and migration boundary.
