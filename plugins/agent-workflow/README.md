# Agent Workflow for Codex and Claude Code

This plugin packages five skills: `agent-workflow:setup`,
`agent-workflow:build-workflow`, `agent-workflow:run-workflow`,
`agent-workflow:report-bug` and `agent-workflow:convert-agent`. The conversion
skill uses the offline engine CLI to migrate Codex TOML and Claude Markdown
definitions to YAML, reporting omitted options for explicit review. It requires
Python 3.11+ and GitHub CLI attestation verification for engine setup. The default
Codex prerequisite profile uses the version in [compatibility.json](compatibility.json);
a Claude host uses `--host claude` on every launcher invocation.

Install from an exact release tag:

```sh
codex plugin marketplace add dk-code-factory/agent-workflow --ref v0.2.2
codex plugin add agent-workflow@agent-workflow
```

Both host catalogs discover the same five skills. For a requested Claude install,
register this marketplace in Claude Code and install `agent-workflow@agent-workflow`.
Run the setup skill, then ask the host to build or run a workflow. Authoring first
resolves requirements and architecture, presents a complete test plan, and waits
for your explicit approval before implementation. Runs retain durable identity,
completed work and admitted limits through disconnection and recovery. Bug
reports are sanitized drafts until you authorize posting.

The engine is downloaded only from the identical release version, verified
against release-manifest provenance, and installed in a private user cache.
Cached installations work offline; no plugin files or normal Codex installation
are modified. The separate network-runtime plugin remains independently available.
See [installation and candidate mode](references/installation.md),
[authoring syntax](references/authoring.md), [plugin authoring](references/plugin-authoring.md), [recovery](references/recovery.md)
and [bug reports](references/bug-reporting.md).

Manual live acceptance is separate from ordinary CI and release certification.
The [acceptance protocol](https://github.com/dk-code-factory/agent-workflow/tree/main/conformance/plugin) describes bounded
candidate runs. Release readiness also requires the gated tag pipeline and an
actual tagged-marketplace installation; local candidate checks cannot certify it.

The toolchain generates both host targets; native activation/provider acceptance
is separate evidence. Full managed runtime parity remains tracked in issue #134.
