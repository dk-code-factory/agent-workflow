# Stable release operations

The target is public `dk-code-factory/agent-workflow`, with the existing history
and first stable version `v0.2.0`. This checkout is a preparation candidate;
repository provisioning, remote CI, signed current-build live evidence, and the
first immutable publication have **not** completed. The GitHub API returned 404
for the target during implementation. No remote was created or changed.

## Repository bootstrap

After explicit repository-creation authorization, create the empty public
repository without an initial README and push this history as `main`. Configure
`origin` to `git@github.com:dk-code-factory/agent-workflow.git`. Do not rewrite
existing commits. Initial bootstrap precedes enforcing PR-only main updates.

Review the proposed settings with `python3 scripts/repository-settings.py`.
An administrator can explicitly apply them to the existing public repository
with `python3 scripts/repository-settings.py --apply`. This enables immutable
releases, requires PRs with zero mandatory approvals, resolved conversations and
the strict `Required` aggregate check, and prevents main force-push/deletion.
Two version-tag rulesets restrict creation to repository administrators and
prohibit tag updates/deletion with no bypass. Avoid introducing a bypass while
repairing a blocked release.

Configure the Actions secret `RELEASE_POLICY_TOKEN` with **read-only**
Administration, Actions and Contents access to this repository. GitHub's ordinary
workflow token cannot inspect every administrative setting. The secret is used
only by tag release jobs. PR CI has read-only permissions and no provider or
publication credentials. Release jobs use the ephemeral Actions token for GHCR,
release assets and OIDC attestations; no GPG private key is uploaded.

CI must pass on Linux amd64/arm64 and macOS amd64/arm64, including race, vet,
extracted-archive smoke and the locked SDK fake gateway. Linux additionally runs
the bounded filesystem fixture and genuine Docker/rootless Podman on both
architectures. `Required` rejects any failed, canceled or skipped prerequisite.
The weekly security workflow audits locked Go/npm dependencies. Dependabot opens
proposals; merging an update never creates a version tag.

## Support and evidence

`release/policy.json` is the enforceable support matrix. Its 57 mandatory records
cover native Claude/Codex smoke, resume/interruption on four platforms; Claude
SDK gateway structured output, managed discovery, nested workers, uncertain MCP
and mixed CLI/MCP development; both Linux architectures on Docker/Podman with
container execution/owner loss; and disposable GitHub publication recovery.
Four additional signed `runtime_recovery` records (Linux architectures × runtimes)
require `owner_loss`, `runtime_unavailable`, `runtime_restart`, `cleanup_recovery`
and `stale_execution_ownership` assertions, candidate/fingerprint, runtime version
and exact image digest. These are reviewed records from disposable runtime failure
fixtures; the manual provider harness does not generate them automatically.
No production support is certified until the whole matrix passes for the exact
candidate commit. Codex managed policy remains rejected by admission. macOS
VM/container certification is deferred and has no required row or support claim.

Real scenarios remain outside ordinary CI. They require fresh named-scenario
authorization, working credentials and, for publication, an explicitly supplied
disposable target whose branches/PRs are preserved. Historical pilot reports and
budgets cannot satisfy these gates. `publication_repositories` deliberately starts
empty: an authorized disposable target must be designated in policy **before**
freezing the candidate. Nothing refreshes provider login automatically.

Build and run the manual conformance binary from the exact clean candidate.
Read `conformance/README.md` for the harness and its assertions. Review private
reports outside the checkout. Remove private `artifacts` paths and sanitize all
remaining fields. Add the actual `provider`, `runtime_version` for containers,
and authorized `publication_repository` for publication. Preserve all original
assertions, timestamps, build identity and provider versions. Container records
must identify the exact image configuration digest distributed by the release;
rebuilding a different image blocks release and requires new evidence.

The reviewed aggregate has this structure:

```json
{
  "candidate": "<full clean candidate commit>",
  "source_fingerprint": "<candidate fingerprint>",
  "reviewed": true,
  "records": ["<sanitized report objects, one per policy row>"]
}
```

Compute the fingerprint from a clean checkout with
`PYTHONPATH=scripts python3 -c 'from release_lib import source_fingerprint; print(source_fingerprint())'`.
Each record retains `engine.commit`, `engine.version`, `engine.dirty` as the
string `false`, and `engine.source_fingerprint`. Evidence is stored on a separate
orphan `release-evidence` branch at `<candidate>/report.json`, with its detached
ASCII signature in `<candidate>/report.json.asc`. Use a separate worktree so
recording evidence does not change the candidate it certifies. Keep raw private
reports and credentials off this branch. Sign **only after human review**:

```sh
gpg --local-user DC4DED587433F319EEE1EB745BD1EAD257F21B89 --armor --detach-sign report.json
```

The checked-in public key and allowlist use the existing configured maintainer
key, fingerprint `DC4DED587433F319EEE1EB745BD1EAD257F21B89`. No maintainer signing
was performed by the implementation. The validator imports only checked-in
public keys into a temporary keyring. Missing, expired, revoked or untrusted
signatures fail. Missing, duplicate, dirty, failed or stale evidence fails.
Evidence review is a human trust boundary; a signature does not prove assertions
were honestly measured.

## Validate, tag and publish

Wait for successful main CI and signed evidence for the candidate. Use a clean
checkout; even unrelated untracked files prevent signing off a release checkout.
`make release-check TAG=v0.2.0` is read-only and validates an existing local signed
tag, evidence, ancestry, current main CI and GitHub settings.
`python3 scripts/validate-release.py v0.2.0 --offline` checks local gates only and
never authorizes publication. Neither command creates a tag or invokes a provider.

Create the annotated signed tag locally with your maintainer key, verify it with
`git verify-tag v0.2.0`, then intentionally push that exact tag. Tag pushes start
`release.yml`. The pipeline validates before running fresh deterministic CI,
builds all four CLI archives twice and compares hashes, builds/tests worker
images natively, audits Go/npm, and blocks high/critical image vulnerabilities.
Any reported scan failure requires repair before publication. Vulnerability DBs
are current network inputs; scans are separate from deterministic tests.

Archive timestamps, gzip metadata, order, ownership and modes are normalized.
All tracked source bytes and executable modes, including Dockerfiles, enter the
fingerprint. Worker builds pass the source epoch to BuildKit and Podman; the
release still requires actual image identities to match reviewed evidence.
Images are loaded from the tested compressed archives and pushed without a
second build. The multi-architecture index references those platform digests.
Image IDs identify configurations; registry manifest/index digests identify the
published artifacts. Both identities are retained, never conflated.

The publisher verifies protected settings again immediately before creating a
draft. It uploads archives, compressed images, checksums, dependency and image
SBOMs, audit reports, signed sanitized evidence, the source/registry manifest and
OIDC verification bundles. It downloads every draft asset, compares bytes and
verifies provenance constrained to repository, release workflow, commit and tag.
The completed draft is then published under enabled immutability. Post-publication
`gh release verify` and `gh release verify-asset` checks must also pass.

Publication is serialized per version. Existing version image labels may only be
reused when they identify the exact expected image. Existing drafts must match
the target commit and each existing asset byte. Unknown or changed assets stop
the run; uploads never use `--clobber`. Published releases always stop retries.
OIDC bundles can differ between builds, so a retry with newly generated bundles
can correctly stop on a conflict: retain and reuse the original verified build
artifacts for a matching draft, or investigate before choosing a new version.
A failed public post-check is an incident, never permission to replace assets.

## Installation and verification

Download the archive for your platform plus the manifest, SHA256SUMS and both
provenance bundles. Check SHA256SUMS (`sha256sum --check` on Linux or
`shasum -a 256 --check` on macOS), then verify provenance and immutable release
membership before extracting:

```sh
git verify-tag v0.2.0
gh attestation verify agent-workflow-0.2.0-linux-amd64.tar.gz \
  --repo dk-code-factory/agent-workflow \
  --signer-workflow dk-code-factory/agent-workflow/.github/workflows/release.yml \
  --source-digest "$COMMIT" --signer-digest "$COMMIT" --source-ref refs/tags/v0.2.0 \
  --bundle archive-provenance.jsonl
gh release verify v0.2.0 --repo dk-code-factory/agent-workflow
gh release verify-asset v0.2.0 agent-workflow-0.2.0-linux-amd64.tar.gz \
  --repo dk-code-factory/agent-workflow
```

Obtain `$COMMIT` from the independently verified tag. Verify release-manifest.json
with the same constraints and `manifest-provenance.jsonl`. For images, take the
index/architecture digest from that verified manifest and run
`gh attestation verify oci://ghcr.io/dk-code-factory/agent-workflow-worker@sha256:...`
with the same repository/workflow/commit/tag constraints. Pull and execute by
digest; version labels are conveniences. Extract the archive, put the CLI on PATH,
then use `agent-workflow doctor` and the locked `bridge setup` flow described in
`DISTRIBUTION.md`. Provider CLIs and Node remain explicit prerequisites.

## Recovery, incidents and patch releases

Use the CLI backup/restore and recovery-bundle commands documented in README;
stop the owner before maintenance, preserve the pre-restore copy, and verify
integrity before resuming quarantined work. Store private state backups separately
from public release artifacts and keep native authentication out of evidence.
Back up the Git repository including signed tags and `release-evidence`, release
assets/checksums/bundles, and OCI archives plus registry digest inventories.

For a failed prepublication run, preserve logs and fixtures, inspect uncertainty,
and never delete disposable conformance branches/PRs automatically. For a broken
published version, document the affected commit/digests and suspend further tag
pushes while investigating. Corrections get a new patch version and new evidence;
never move a signed version tag or replace published bytes. Repository/release
administrators can still delete entire resources; immutability is bounded by
GitHub administrative controls, so independent backups matter.

Rotate signing keys through a reviewed main PR: add the new public key and full
fingerprint, verify it out of band, retain old public keys for historical releases,
and remove compromised keys from the active allowlist. Re-certify a new candidate
if policy changes. Set `buildinfo.Version` and policy version together for every
patch, refresh dependencies by reviewed PR when necessary, and repeat all gates.

## Pending acceptance

No target repository settings, remote matrix, live conformance, GHCR upload or
immutable release has been verified from this checkout. Current live credentials,
named-scenario authorization, a designated disposable repository, reviewed signed
evidence, and an exact clean committed candidate remain required. Genuine runtime
restart/outage and broader provider-frame boundary certification also remain in
the production audit; the new real-container removal test covers runtime-side
container loss, not every daemon restart behavior. Do not publish `v0.2.0` until
these nondeferred gates are closed.
