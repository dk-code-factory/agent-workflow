# Production Codex plugin integration

This is an explicit opt-in, model-free check of the production plugin and shared
Go helper with installed stock **Codex 0.164.0**. `make test` and `make check`
never invoke it. It requires Linux, Go, Python 3, `ip`, `mount`, `unshare`, and
existing passwordless `sudo` authority for the uniquely owned namespace fixture.
It refuses CI and an initial root caller. Run it only when the current session
authorizes this isolated privileged check:

```sh
python3 conformance/network-runtime/codex-plugin.py \
  --run-isolated-local-fake \
  --codex /absolute/path/to/codex
```

The driver creates a fresh mode-0700 evidence directory; optionally select its
parent with `--evidence-parent /private/evidence`. It builds the current
`agent-workflow-runtime` and copies the production `plugins/network-runtime`
assets unchanged. Installed Codex discovers the copied assets in an isolated
plugin cache. Only the current fingerprints in that disposable cache are trusted.
It also runs native `plugin marketplace add` and `plugin add` against a copied
local distribution, in a separate disposable configuration, and checks the
installed trampoline bytes. The driver does not install, enable, trust, or modify
the normal user's plugin, configuration, or authentication.

The root fixture makes mount propagation private before mounting temporary
filesystems over the existing `/run` and `/var/lib` directories. The protected
launcher therefore exists only in this fixture's private registry. A private
passwd bind mount redirects the caller's login home to disposable shell profiles
that deliberately overwrite the public endpoint selectors. No host socket
placeholder or persistent runtime enrollment is created. The child PID/network
namespace has only loopback and no external default route. Codex runs with the
original caller UID/GID/groups and a fresh Git project, HOME, CODEX_HOME, XDG,
configuration, authentication and trust context. Environment inheritance begins
from an explicit fixture dictionary. Fake Responses requests use only loopback,
`requires_openai_auth = false`, and zero token usage; no real provider, login,
model, GitHub, image pull, or remote publication is involved.

The **control/status service is a fake**, listening as root on the canonical
Unix-packet endpoint. It accepts only `check` with the exact full reference and
reports the current network namespace. The selected API is also a local fake
Unix HTTP service. This tests native plugin discovery, trusted hook execution,
protected launcher availability, real Go helper protocol/peer/reference/namespace
validation, and the resulting native shell execution. It does **not** establish
real Podman runtime admission, namespace joining, API fencing, or runtime service
health. Pair it with the separate actual-runtime checks in
[`acceptance.mjs`](acceptance.mjs) and the runtime verification evidence.

The fourteen native cases check restrictive `include_only`, native `--profile`,
stale login/snapshot selectors repaired using the independent admitted reference
(the fake root peer observes only the helper's nonsecret routing/reference
markers; an untrusted baseline confirms the tool's selectors remain filtered),
code-mode nested shell dispatch, native workspace/read-only/network restrictions,
native approval request and denial, original exit status, missing/nonzero or
unrecognized helper output, and reference/namespace mismatch. The tool records
that the three routing values were repaired while an unrelated filtered variable
stayed absent and UID/GID/cwd stayed unchanged. Direct production-protocol checks
also assert exact original command suffix and preservation of every input option,
and valid denial for malformed input and ordinary helper/validation failures.
The fixture registers no PermissionRequest hook and never approves native
permission requests.

`report.json`, app-server frames, fake Responses bodies, and native CLI output
stay in the private evidence directory. `sanitized-summary.json` contains the
bounded assertions, case counts, zero-real-model provenance and hashes of the
Codex binary, helper, driver, marketplace and copied assets. A failed assertion remains a failure;
inspect private evidence instead of broadening provider access. No transcript or
user configuration belongs in the repository.

These observations do not prove mandatory enforcement. A skipped, disabled,
untrusted, modified, unspawnable, forcibly killed, or timed-out hook can leave
execution unrepaired; a later trusted rewrite can replace the prefix. Direct
`command/exec` RPC and native Codex MCP startup bypass this shell hook. Rewriting
also changes approval-rule matching. Historical interrupt experiments let a long
command continue with and without the plugin; they establish no new per-turn
termination guarantee. Saved outside API connections and all-path fencing remain
[issue 38](https://github.com/dk-code-factory/agent-workflow/issues/38). The
separate Go invocation guardian retains its own lifecycle responsibility.
