---
name: setup
description: Check Agent Workflow plugin prerequisites and install its exact matching engine release on Linux or macOS.
---

Resolve this installed plugin's root from this SKILL.md (two directories up).
Use its `scripts/agent-workflow` launcher by absolute path, quoted in shell
commands. On a Claude host add `--host claude` to every check/ensure/exec
invocation before `--`, including the version verification below. Never use an engine found on PATH or resolve a latest release.

1. Run `python3 <plugin>/scripts/agent-workflow check`. This is read-only and
   checks Python 3.11+, GitHub CLI attestation support, the compatible Codex
   version and host platform. For a Claude host, use `check --host claude` to
   omit the Codex prerequisite; host selection does not select an execution runtime.
   Read `<plugin>/compatibility.json` for exact versions.
2. For a requested installation, run `python3 <plugin>/scripts/agent-workflow ensure`.
   Use `ensure --host claude` for a Claude host. Installation writes only a
   private, versioned user cache. It verifies the
   exact tag's release-manifest provenance, archive digest and build identity.
   An intact cached distribution works offline. Keep the plugin immutable.
3. Verify with `python3 <plugin>/scripts/agent-workflow exec -- version`.

Report missing dependencies, unavailable versions/releases and verification
errors explicitly. Do not run login, refresh credentials, change the normal
Codex installation, or substitute another version. Let the user select an
existing authorized credential route or explicitly install prerequisites.

For development only, read [candidate installation](../../references/installation.md).
Candidate mode requires both a local archive and expected digest, is visibly
uncertified, and uses a separate cache. Never use it as release recovery.

For a Claude SDK execution workspace, explicit `ensure --host claude --claude-bridge`
sets up locked Node dependencies in a separate writable cache and reports the bridge
path. This requires Node/npm, leaves the verified engine distribution unchanged,
and does not create workspace configuration or credentials. Use an explicit token
or API-key environment reference in the workspace. See [plugin authoring](../../references/plugin-authoring.md).
