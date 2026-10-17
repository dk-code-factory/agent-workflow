# Manual conformance

These manifests authorize nothing by themselves. Agents must execute a named
real-provider or GitHub scenario only after the user explicitly requests it.
The executable refuses every invocation when `CI` is present and requires
`--allow-live`. Publication additionally requires `--allow-publication`, an exact
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
Workflow scenarios clone an explicitly supplied local `--repository-path`
independently and require `--workflow` relative to that clone. The mixed-provider
template is `workflows/mixed-development.yml` in this directory; copy it into the
disposable fixture repository and configure planner, builder and independent
Claude/Codex reviewer roles. Publication uses the existing development workflow
and matching allowlisted policy in an explicitly supplied disposable repository.
Container owner-loss requires a container role with a long-running approved shell
tool; only the newly launched owned controller process is killed.

Session tests verify stable identity/context. Interrupted tests retain partial
usage. Discovery tests require managed policy and use native hook/MCP canaries.
Nested tests verify a child/grandchild tree at capacity one. The external MCP
uncertain-response test hosts a local effect fixture and drops its response;
passing requires one effect and `needs_attention`. Container tests require an
already available digest-pinned compatible image; none is pulled implicitly.
Unsupported upstream policy remains a failed gate, never a backend substitution.

Evidence records build/commit, OS/architecture, provider versions, runtime/image,
configuration fingerprint, checked assertions, usage completeness/reported cost,
and private artifact references. SQLite state and raw provider transcripts stay
private. Commit only reviewed sanitized evidence summaries; no credentials or raw
private transcripts belong in this repository. Failed/unverified assertions stay
false. Protocol observations useful as regressions should be redacted into
deterministic fixtures before committing them.
