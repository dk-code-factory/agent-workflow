# Stable release operations

`.github/workflows/release.yml` is the **sole supported publication mechanism**
for public `dk-code-factory/agent-workflow`. Do not invoke the publisher locally
or introduce another workflow that publishes. The current policy version is
`v0.2.1`; production support remains Linux amd64, subject to complete evidence.
Linux arm64 and macOS live certification, API-key/gateway certification and
Codex managed policy are outside this change.

## Ordering and environments

A stable version-tag push follows this order:

1. Validate the signed annotated tag, version, clean source identity, main ancestry
   and successful exact-commit main CI. Candidate validation needs no evidence.
2. Run the reusable credential-free `pre-release.yml`: native race/tests/vet on
   four platforms, filesystem fixtures, Docker/Podman checks, locked SDK fake
   gateway, three browser projects and the `Pre-release Required` aggregate.
   Each platform's four PR-fix race shards run in separate jobs to avoid
   contention between database-heavy recovery fixtures. Native jobs run
   `make test-core check build`; `make test` still includes every race shard.
3. Build archives reproducibly and build, test and scan both worker images.
4. In one Linux amd64 `pre-release` job, use the built binaries and exact scanned
   distribution image for smoke/session E2E, then the full provider/runtime matrix.
5. Upload an unsigned sanitized aggregate, then open `release-certification` for
   human review. Failed, cancelled or skipped prerequisites block this gate.
6. After approval, verify the signed report against immutable same-run E2E bytes,
   pin the verified report, publish the tested artifacts, and verify the release.

`test.yml` and standalone `pre-release.yml` remain credential-free and do not
invoke live conformance. Deterministic validation does not certify a provider.
A manual `gh workflow run pre-release.yml --ref <candidate>` does not replace the
release workflow's mandatory invocation on the exact signed candidate.

An administrator sets up the existing repository once with:

```sh
python3 scripts/release-environments.py --apply
```

This idempotently configures tag-only `v*` policies on `pre-release` and
`release-certification`. The latter requires `dkropachev` and permits self-review.
Existing wait timers, reviewers, bypass preferences, custom protection rules and
unrelated secrets are preserved. Conflicting deployment branch policies stop
setup for explicit reconciliation. Existing credentials are preserved; explicit
rotation uses `--apply --replace-credentials`.

The default files are:

- `CODEX_AUTH_JSON`: `/home/dkropachev/.codex/accounts/dmitry.kropachev.dev+codex+1/auth.json`.
- `CLAUDE_CODE_OAUTH_TOKEN`: `/home/dkropachev/.config/agent-workflow/claude-oauth-token`.

Override paths with `--codex-auth-file` and `--claude-token-file`. Credential bytes
travel through stdin to `gh secret set`, never argv, logs or committed files.
Use [Codex file-backed authentication](https://learn.chatgpt.com/docs/auth) and
[Claude's subscription-token setup](https://github.com/anthropics/claude-code-action/blob/main/docs/setup.md).
The operator obtains a fresh Claude subscription token with `claude setup-token`
when rotation is necessary. The workflow never runs login or updates secrets.
Missing, expired, invalid or rejected credentials fail validation. Any provider's
normal in-job auth-cache changes stay private; secret rotation is always explicit.

Credentials are materialized with private permissions under
`$RUNNER_TEMP/release-provider-e2e`. The same private state survives both phases.
Credential components are masked. Provider adapters forward only the selected
provider's credential route; GitHub tokens stay outside provider subprocesses.
Only selected identity/assertion fields enter the uploaded aggregate. Raw provider
output, SQLite state, config, paths and transcripts remain private and are removed
with owned runtime storage by the always-run cleanup step. A cancelled runner is
ephemeral; never recover its private state into public artifacts.

Repository protections and release immutability are separately configured through
`scripts/repository-settings.py --apply`. Main requires the strict `Required` check,
PRs and resolved conversations. Version-tag rules restrict creation and prohibit
updates/deletion. Release jobs use the ephemeral workflow token, with write
permissions only in `publish`. No signing private key is uploaded.

## Required live evidence

`release/policy.json` owns the full matrix. Phase one requires seven records:
Claude/Codex CLI authenticated smoke, Claude SDK and Codex app-server resume and
interruption, and Claude structured output. Phase two requires nine records:
Claude discovery isolation, nested workers at capacity one, uncertain external MCP
responses, mixed development through CLI and MCP, and container execution and
owner loss on Docker and rootless Podman. Mixed scenarios require actual recorded
invocations and versions from both providers. `agent/run` accepts native Codex
definitions; conformance-only registered Go actions execute managed Claude turns.

The job installs locked provider dependencies, including the pinned Claude CLI
in `conformance/providers`, the existing Codex CLI and the packaged SDK lock.
It never rebuilds a worker image for certification. Runtime fixtures load the
scanned compressed distribution archive and verify its configuration digest.
Docker runs a separate daemon with private data/exec directories and socket;
Podman uses a separate rootless service, storage and run directory. Only these
owned services are stopped/restarted. No shared filesystem is exhausted.
Each named scenario gets one attempt and a five-minute deadline; the whole job
has a 120-minute limit. There are no automatic scenario retries.

Two runtime-recovery records additionally prove `owner_loss`,
`runtime_unavailable`, `runtime_restart`, `cleanup_recovery` and
`stale_execution_ownership`. Owner loss waits for observed model usage, kills
only the owned fixture controller, verifies that an unavailable runtime retains
cleanup obligations, restarts the same owned service, reconciles cleanup and
rejects a stale invocation owner.

GitHub publication conformance is **separate** and still requires current explicit
named-scenario authorization, an existing designated disposable repository and
`--allow-publication`. CI authorization never permits it. Preserve disposable
branches/PRs. Historical budgets authorize no calls and historical reports cannot
certify another candidate. No tool automatically creates repositories, refreshes
login, merges or deletes remote fixtures.

## Tag, inspect, sign and approve

Freeze a clean candidate, merge it to main, and wait for exact-commit main CI.
Create and verify the maintainer-signed annotated version tag, then intentionally
push it. Do not move an existing tag. Read-only candidate inspection is:

```sh
python3 scripts/validate-release.py v0.2.1 --phase candidate
```

`--offline` validates only local gates and never authorizes publication. Complete
validation remains the default and retains all signature/evidence requirements.
The candidate phase cannot pin evidence or authorize publication.

After both E2E phases succeed, download the release run's `provider-e2e-<run-id>-<E2E-attempt>`
artifact with `gh run download <run-id> --repo dk-code-factory/agent-workflow
--name provider-e2e-<run-id>-<E2E-attempt> --dir <private-review-directory>`. Inspect
`automated-evidence.json`: candidate, source fingerprint, originating run/attempt,
all assertions, observed provider identities/versions and both exact image IDs.
Do not edit automated records, their order, runtime recovery, origin or images.

Copy the aggregate to `report.json`, change `reviewed` to `true`, and append the
separately authorized, sanitized `disposable-github-publication` record to
`records`. That publication record must identify this exact candidate and an
allowlisted repository. Remove private `artifacts` paths and preserve actual
assertions, timestamps, versions and authentication mode. Review all fields.
Sign only after human review with the existing maintainer key:

```sh
gpg --local-user DC4DED587433F319EEE1EB745BD1EAD257F21B89 --armor --detach-sign report.json
```

In a separate worktree on orphan `release-evidence`, store the report and detached
signature as `<candidate-commit>/report.json` and `report.json.asc`. Push that
branch, then approve the pending `release-certification` deployment. Approval
alone cannot satisfy the signed-report check. No agent signs the human report.

After approval, certification fetches the evidence branch, verifies signatures
against checked-in keys and the signer allowlist, and validates every policy row.
It compares automated records exactly with the immutable E2E artifact, including
candidate/fingerprint, run/attempt and image IDs. Another candidate/run, a future
attempt, missing coverage or edited automated records fails. Verified report
bytes and signature are pinned in a separate certification artifact for publication.
Later evidence-branch edits cannot change those pinned bytes. The publisher
revalidates the pinned signature, automated match, source, tag and main CI before
registry effects and again before draft-release publication.

## Publication and verification

Archives have normalized timestamps, gzip metadata, ordering, ownership and modes.
The source fingerprint covers all tracked bytes and modes. Builds require the
exact pinned Go toolchain. Scans use current vulnerability databases; high or
critical image vulnerabilities block release.
Images are loaded and pushed from the tested archives without rebuilding. Image
configuration IDs and registry manifest/index digests are kept distinct in the
inventory. The multi-architecture index references the tested platform manifests.

The publisher uploads archives/images, dependency inventories, SBOMs, audit
reports, unsigned automated and signed certification evidence, checksums and OIDC
provenance. It downloads each draft asset, compares bytes and verifies provenance
constrained to repository, workflow, commit and tag. It publishes the completed
draft immutably and runs public release/asset verification.

Consumers should verify the signed tag, SHA256SUMS, manifest/provenance and immutable
release membership before extraction. For example:

```sh
git verify-tag v0.2.1
gh attestation verify agent-workflow-0.2.1-linux-amd64.tar.gz \
  --repo dk-code-factory/agent-workflow \
  --signer-workflow dk-code-factory/agent-workflow/.github/workflows/release.yml \
  --source-digest "$COMMIT" --signer-digest "$COMMIT" --source-ref refs/tags/v0.2.1 \
  --bundle archive-provenance.jsonl
gh release verify v0.2.1 --repo dk-code-factory/agent-workflow
```

Obtain `$COMMIT` from the verified tag. Verify `release-manifest.json` and images
with the same constraints and their manifest/image bundles. Install by verified
archive or registry digest; use `agent-workflow doctor` and the bridge setup flow
in `DISTRIBUTION.md`. Both static Linux guardians and their SHA256 inventory are
included in every archive, including macOS archives.

## Retry and recovery

For a failed certification job, preserve the successful same-run E2E artifact.
Repair/review/sign the report and push `release-evidence`, then **rerun failed jobs**
in that release run. The gate requires approval again. It accepts the original
successful E2E attempt from the same run and creates a new certification artifact
named with the current attempt. It never requires another provider run just to
repair certification. Rerunning all jobs incurs fresh provider calls and creates
another immutable artifact named with the new E2E attempt; old artifacts remain.
Expired/missing artifacts require full validation again; another run's report
cannot be substituted.

A provider failure blocks the human gate. Investigate private diagnostics and
credential rejection, explicitly replace secrets if necessary, and rerun the
failed job. Do not silently retry a scenario within a job or sign synthetic
coverage. A changed candidate needs a new signed version candidate and evidence.

Publication is serialized by version. Existing image labels and draft assets can
only be reused with identical expected bytes/identities. No `--clobber` is used.
A published release stops retries; failed public verification is an incident,
never permission to replace bytes. Retain original build/provenance artifacts
when repairing a matching draft, since regenerated attestations may conflict.
Never move a signed version tag or delete remote fixtures automatically.

Back up signed tags, the evidence branch, release artifacts, checksums, provenance
and OCI inventories independently. Keep private state backups separate. Use the
CLI stopped-owner backup/restore and recovery-bundle procedures; preserve the
pre-restore copy and inspect quarantined work before resuming. Rotate signing keys
through a reviewed main change, verifying the new fingerprint out of band and
retaining historical public keys. Corrections to a published version require a
new patch version and the entire release pipeline.

## Acceptance status

Implemented gates and deterministic fixtures are not live certification. Acceptance
requires both E2E phases to pass in the signed-tag release pipeline before the human
gate becomes available, followed by separately reviewed publication evidence and
signature. No current candidate is certified merely by implementing this runbook.
