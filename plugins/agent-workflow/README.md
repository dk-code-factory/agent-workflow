# Agent Workflow for Codex

This plugin packages four skills: `agent-workflow:setup`,
`agent-workflow:build-workflow`, `agent-workflow:run-workflow` and
`agent-workflow:report-bug`. It requires Python 3.11+, GitHub CLI with attestation
verification, and the Codex version in [compatibility.json](compatibility.json).

Install from an exact release tag:

```sh
codex plugin marketplace add dk-code-factory/agent-workflow --ref v0.2.2
codex plugin add agent-workflow@agent-workflow
```

Run the setup skill, then ask Codex to build or run a workflow. Authoring first
resolves requirements and architecture, presents a complete test plan, and waits
for your explicit approval before implementation. Runs retain durable identity,
completed work and admitted limits through disconnection and recovery. Bug
reports are sanitized drafts until you authorize posting.

The engine is downloaded only from the identical release version, verified
against release-manifest provenance, and installed in a private user cache.
Cached installations work offline; no plugin files or normal Codex installation
are modified. The separate network-runtime plugin remains independently available.
See [installation and candidate mode](references/installation.md),
[authoring syntax](references/authoring.md), [recovery](references/recovery.md)
and [bug reports](references/bug-reporting.md).

Manual live acceptance is separate from ordinary CI and release certification.
The [acceptance protocol](https://github.com/dk-code-factory/agent-workflow/tree/main/conformance/plugin) describes bounded
candidate runs. Release readiness also requires the gated tag pipeline and an
actual tagged-marketplace installation; local candidate checks cannot certify it.
