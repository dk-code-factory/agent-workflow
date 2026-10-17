# Distribution and intentional release

`make dist` builds Linux/macOS amd64/arm64 archives locally, then smoke-tests the
native extracted archive from a disposable directory with fake providers only.
Archives contain the binary, guardian, examples, documentation, licenses,
conformance manifests/executable, bridge sources and exact dependency lockfile.
`agent-workflow version`, health, MCP and conformance share one engine version.
Release builds also record commit, dirty state and an exact source fingerprint.

Node 22+ and selected native provider CLIs remain explicit prerequisites.
`agent-workflow bridge setup` installs only locked npm dependencies with lifecycle
scripts disabled. It never installs/updates provider CLIs or refreshes login.

```sh
python3 scripts/build-release.py --smoke
python3 scripts/audit-release.py
python3 scripts/verify-release.py dist
python3 scripts/build-worker-image.py --runtime podman --arch amd64
python3 scripts/build-worker-image.py --runtime podman --arch arm64
```

Worker builds use the committed digest pins in `images/pins.json`, exact npm
lockfiles, a non-root default user and matching guardian/bridge labels. Bases
include Linux amd64/arm64 manifests. Image builds/save operations are explicit;
execution never pulls an image. Podman OCI archives and Docker image archives record immutable image IDs and
checksums in their metadata. Public base builds use an isolated empty registry auth file,
preserving the caller's native registry login and including no credentials.
`scripts/pin-worker-images.py` is an explicit maintenance action for refreshing
base pins; ordinary CI does not refresh them automatically.

`dependencies.json` and `sbom.cdx.json` inventory actual locked dependencies.
`scripts/audit-release.py` records Go call-graph and npm lockfile vulnerability
reports. Security reports are time-dependent evidence; deterministic test CI does
not silently fetch new dependency versions. Rebuild archives after changing
dependencies/toolchain/source, and regenerate checksums after all image/report
artifacts are present. Verify all artifacts before publication.

These scripts never upload releases or push registries. Publication requires an
intentional human release action after reviewing deterministic checks,
vulnerability reports, OS/runtime/provider evidence and all remaining gates in
`RELEASE.md`. Schema generation, fake gateways and cross-compilation do not
certify actual provider/platform support. Manual conformance is separate, refuses
CI, and requires explicit named-scenario authorization.
