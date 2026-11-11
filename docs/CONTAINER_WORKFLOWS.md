# Container workflow execution

The container runtime foundation provides typed image inspection, inert create,
attach/start/wait, exec creation/attachment/inspection, and verified removal.
Docker uses the official Go SDK with API version negotiation. Podman has a
separate CLI backend. Existing command/provider policies and legacy invocation
records retain their existing transport and cleanup behavior.

New runtime users retain a `containerapi.Selection` and the resolved
`containerapi.Image` before admission. Image tags can be inspected locally; the
create request requires the resolved `sha256` content ID. No operation pulls an
image, logs in to a registry, or provisions a runner. Docker execution currently
supports local Unix socket endpoints; remote TCP/SSH/TLS Docker endpoints fail
explicitly. Podman selects its executable and local environment once; named
connections require an explicit retained endpoint. These limits do not change
legacy invocation recovery.

`runtimeexec.LaunchContainer` records ownership intent before creating an inert
container and records its returned ID before start. A lost create response is
reconciled using the retained name and exact ownership labels. Cleanup verifies
the endpoint/daemon identity and ownership before removal, then verifies absence.
An API error cannot establish absence. Unconfirmed cleanup remains uncertain and
blocks new execution through the existing invocation journal.

The typed policy builder enforces a non-root user, read-only root filesystem,
dropped capabilities, no privilege escalation, PID/memory/CPU bounds, private
`/tmp`, explicit environment values, and permitted workspace/private-state mounts.
Networking defaults to `none`. The initial typed launcher explicitly rejects
`network-runtime`; the existing network-contained launcher remains supported.
Runtime sockets and ambient provider credentials are not mounted or forwarded.

An independent host guardian watches a private controller lifetime pipe and can
remove the owned container after controller death. Inside the container a static
Linux PID 1 guardian requires an advancing controller heartbeat before launching
work and stops descendants when that heartbeat expires. Ordinary Linux images
need no guardian/action labels. Existing guardian-equipped images remain usable.

Every distribution, including macOS, includes Linux amd64 and arm64 guardians in
`guardians/` plus `checksums.json`. Runtime selection verifies a regular executable
file, SHA256, ELF architecture and static linkage before mounting the selected
binary read-only at `/engine-guardian/guardian`. `make build` builds both variants
for development. Releases include and independently verify both variants.

The following stages add GitHub-style job-container/Docker-step authoring,
structured helper recovery and retained helper bundles. The runtime foundation
alone does not enable that YAML syntax. Reusable-workflow calls and the existing
typed Go action API are unchanged by this stage.

Deterministic tests use fake Docker HTTP APIs, fake Podman executables and
private temporary directories. Actual Docker/provider/publication conformance
is separate and opt-in.
