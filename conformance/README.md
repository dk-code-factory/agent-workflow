# Manual conformance

These manifests authorize nothing by themselves. Agents must execute a named
real-provider or GitHub scenario only after the user explicitly requests it.
Outside the tagged release workflow, the executable refuses CI and requires
`--allow-live` plus fresh named-scenario authorization. `--release-ci` permits
only the expected repository's `release.yml`, stable version-tag push, clean
matching harness build and explicitly named nonpublication scenario. PR/main CI
and the deterministic standalone pre-release workflow have no live credentials. Publication additionally requires `--allow-publication`, an exact
existing `--disposable-repository owner/repo`, and matching configured policy.
It never creates repositories, refreshes login, merges, or deletes remote data.

Build separately with `go build -o bin/workflow-conformance ./cmd/workflow-conformance`.
Run from an intentionally private evidence location outside this repository:

```sh
workflow-conformance --scenario /engine/conformance/scenarios/native-claude-smoke.json \
  --config /private/providers.yml --role claude --allow-live \
  --engine-commit ENGINE_COMMIT --evidence /private/conformance-evidence
```

Use the packaged matching `--engine` binary for nested/CLI/MCP scenarios.
CLI smoke and session scenarios call their selected provider adapters directly.
Managed scenarios use a conformance controller with registered Claude actions and
the shared production controller/worker/broker wiring at capacity one. Native
`agent/run` remains restricted to Codex definitions. Mixed workflows verify both
Claude and Codex physical invocation identities and versions.
Workflow scenarios clone an explicitly supplied local `--repository-path`
independently and require `--workflow` relative to that clone. The mixed-provider
template is `workflows/mixed-development.yml` in this directory; copy it into the
disposable fixture repository and add native `builder.toml` and `reviewer.toml` Codex definitions; the registered
Claude action selects the admitted `claude` SDK role. Publication uses the existing development workflow
and matching allowlisted policy in an explicitly supplied disposable repository.
Container owner-loss requires a container role with a long-running approved shell
tool; only the newly launched owned controller process is killed, after observing
provider usage. The release orchestrator supplies `--runtime-control` for its own
disposable daemon/service. It verifies outage uncertainty, restart, cleanup recovery
and rejected stale ownership, and emits both runtime-recovery records. It never
stops the runner's default Docker daemon or uses shared Podman storage.

Session tests verify stable identity/context. Interrupted tests retain partial
usage. Discovery tests require managed policy and use native hook/MCP canaries.
Nested tests verify a child/grandchild tree at capacity one. The external MCP
uncertain-response test hosts a local effect fixture and drops its response;
passing requires one effect and `needs_attention`. Container tests require an
already available digest-pinned compatible image; none is pulled implicitly.
Unsupported upstream policy remains a failed gate, never a backend substitution.

Claude SDK roles can use `authentication.mode: native` with the operator's own
`CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`; `credential-env` can select a
different token variable. An API endpoint is required only for `mode: gateway`.
SDK native authentication keeps managed settings/discovery isolation. Reports
record the selected mode; the historical `claude-gateway-structured` scenario ID
also exercises structured output through native authentication.

Evidence records build/commit, OS/architecture, provider versions, runtime/image,
configuration fingerprint, checked assertions, usage completeness/reported cost,
and private artifact references. SQLite state and raw provider transcripts stay
private. Commit only reviewed sanitized evidence summaries; no credentials or raw
private transcripts belong in this repository. Failed/unverified assertions stay
false. Protocol observations useful as regressions should be redacted into
deterministic fixtures before committing them.

## Shared network runtime

The Linux/rootful Podman and normal-Chrome HTTP/2 check is a separate explicit
opt-in: `node conformance/network-runtime/acceptance.mjs --run-privileged`.
It uses uniquely owned fixtures, no models and no GitHub publication. See
[network runtime verification](../docs/NETWORK_RUNTIME.md#verification).

The separate [production Codex plugin experiment](network-runtime/codex-plugin.md)
uses installed Codex, production assets and the real Go helper with disposable
configuration/trust, local fake Responses/API services and no external route.
Its root-authenticated control/status fixture is fake; the Podman harness above
separately verifies the production helper against an actual prepared runtime.
Hook skip/failure/bypass remains a routing limitation of the plugin. The
independent BPF LSM socket boundary prevents host Unix API fallback, and admission
rejects saved non-Unix container API routes. Deterministic policy fixtures do not
certify LSM attachment or syscall interception; the kernel test and Podman/Chrome
harness remain separate opt-ins.

## Tagged release automation

`release_e2e.py` installs no login flows and never writes GitHub secrets. The
workflow installs the locked Claude CLI, Codex CLI and packaged SDK dependencies,
extracts the actual release binaries, then runs seven smoke/session records
before the nine remaining provider/runtime records. Each scenario has one
attempt and a five-minute deadline; the job has a 120-minute limit. Runtime
fixtures load the scanned distribution archive by its exact image ID.
Only the sanitized aggregate is uploaded. Credentials, workspaces, journals,
transcripts and runtime storage live under an exclusively created runner-temporary
root and are removed on completion. See the [release runbook](../docs/RELEASE_PIPELINE.md)
for the separate human signature/publication-evidence boundary.
