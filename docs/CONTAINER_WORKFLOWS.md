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
binary read-only at `/engine-guardian/guardian`. The launcher rejects a guardian
whose platform differs from the admitted image before creating a container.
`make build` builds both variants
for development. Releases include and independently verify both variants.

Structured helper recovery and retained helper bundles are described by the
following stages. Existing typed Go actions remain supported.

Deterministic tests use fake Docker HTTP APIs, fake Podman executables and
private temporary directories. Actual Docker/provider/publication conformance
is separate and opt-in.

## Shared job containers and Docker steps

```yaml
jobs:
  build:
    runs-on: local
    container:
      image: ubuntu:24.04
    steps:
      - run: echo ready > /tmp/build-status
      - run: cat /tmp/build-status
      - uses: docker://ubuntu:24.04
        with:
          entrypoint: /bin/sh
          args: '-c "echo separate container"'
```

`container: ubuntu:24.04` is equivalent to the image mapping above. Admission
resolves locally available tags to immutable image content IDs and retains the
selected endpoint, daemon, platform and guardian. Execution never pulls images.
`runs-on` supports `local` only; `ubuntu-latest` is explicitly unsupported.

Commands share a lazily created job container through each job iteration. `run`
defaults to `sh` inside that container; explicit shells are respected. A Docker
step uses a sibling container, the same permitted workspace mount, and separate
private state and `/tmp`. Image ENTRYPOINT and CMD are preserved unless
`with.entrypoint` replaces the executable or `with.args` replaces CMD. Arguments
support single/double quotes and backslash escaping, preserving empty arguments
and whitespace, with no variable expansion, command substitution or globbing.
Docker overrides and image selections are static.

The first runnable command acquires one capacity slot for the job. Sequential
execs and sibling steps reuse it. Every exec is recorded before effects and runs
under its own descendant supervisor and heartbeat. Cancellation stops that
command's heartbeat; unconfirmed cleanup blocks later execution. Job teardown
verifies container absence before terminal job completion and releases capacity.

Shared job containers initially support sequential command/Docker steps and
sequence blocks. Parallel blocks, waits, providers and other action targets must
use a separate job. A reusable-call job cannot also declare a container: the
called workflow owns that selection, including nested reusable calls.

After owner loss, cleanup must be verified before another container is created.
An unfinished shared job that already completed ordinary commands requires a new
run: completed commands are never replayed to reconstruct transient state.
Structured helpers with validated checkpoints are added by the next stage.

In the editor, **Job container image**, **Docker entrypoint** and **Docker
arguments** support source-preserving edits, export and Undo/Redo. Converting a
job into a reusable call reviews removal of its container. Docker image changes
retain command overrides; conversion to another step kind reviews their removal.

## Structured helpers

A command in a job container, or a Docker step, can opt into the engine's durable
JSON lifecycle:

```yaml
schemas:
  CaptureOutput:
    type: object
    properties: {head: {type: string}}
    required: [head]
    additionalProperties: false
jobs:
  capture:
    container: ghcr.io/example/python-tools:local
    steps:
      - id: current
        argv: [python3, /opt/helpers/capture.py]
        protocol: action-v1
        effect: read_only
        input-schema:
          type: object
          properties: {repository: {type: string}}
          required: [repository]
          additionalProperties: false
        output-schema: CaptureOutput
        input: {repository: "."}
        env: {TOKEN: '${{ secrets.token }}'}
```

Both schema fields require an object schema, either inline or a name in the
owning workflow's `schemas` map. Names remain names in admitted definitions.
Unsupported schema keywords are rejected. Effects are explicitly `read_only`
or `external_mutation`. Omitted business `input` defaults to `{}`. Business data
renders once, preserving JSON numbers, arrays, objects, booleans, null and literal
expression-looking strings supplied by a whole expression. Commands and container
settings are static; structured commands cannot interpolate inputs or secrets.
Structured helpers cannot own or appear inside ordinary command lifecycle
handlers.

The engine sends exactly one JSON request on stdin. The helper writes exactly one
JSON response on stdout and diagnostics on stderr. Requests and responses are
limited to 1 MiB each; diagnostics are limited to 4 MiB. Extra documents, unknown
response fields, wrong versions/phases, oversized streams and nonzero exits fail
the phase. The configured timeout applies to each phase (default five minutes).
Recovery requests include input, bindings and the retained result within the same
1 MiB limit. The engine checks that combined budget before execution and durable
completion, reserving space for future ownership metadata. Oversized results fail
without releasing an admitted mutation for replay; recovery still requires
authoritative reconciliation. Keep bindings and results compact or use references
to application-owned evidence.

Every request has `protocol: "action-v1"`, `phase`, the original action `context`,
the current `runtime_owner` (run ID and generation), and schema-validated `input`.
Checkpoint phases also receive `checkpoint: {bindings, result}`. The context
contains stable operation key, original run/job/step/iteration, attempt identity
and generation. Current runtime ownership stays separate on recovery.

Responses always repeat `protocol` and `phase`. The phase contracts are:

| Phase | Response fields | Application behavior |
| --- | --- | --- |
| `bindings` | `capabilities`, `read_only_phases`, `bindings` | Read-only capture of stable resource bindings |
| `execute` | `result: {output, evidence?, receipt?}` | Perform the declared operation |
| `validate_checkpoint` | `valid: true` | Read-only verification of retained output/evidence/receipts |
| `reconcile` | `outcome`, `proof`, and `result` when completed | Read-only observation of an uncertain mutation |

Bindings must advertise `bindings`, `execute`, and `validate_checkpoint` in
`capabilities`, and `bindings`/`validate_checkpoint` in `read_only_phases`.
Mutation helpers must also advertise read-only `reconcile`. Reconciliation returns
`completed`, `no_effect`, or `unknown`; completed/no-effect outcomes need non-null
proof. Unknown observations are journaled and block another mutation attempt.
A valid JSON response alone does not prove that an external effect completed.

Helpers use the existing action journal: admission precedes execution, validated
completion precedes dependents, and all retained checkpoints are validated before
successful job/reusable-child shortcuts. Repeated/reusable calls retain distinct
logical operation identities. Lost responses or completion commits reconcile
without blindly replaying effects. Fully completed runs acquire a separate owned
validation execution before launching any verification container.

Execution phases can use the shared job container. Binding, validation and
reconciliation phases use independent containers with the same admitted image,
command and policy. All phases have invocation records, current ownership,
capacity, deadlines and verified cleanup. Do not use transient `/tmp` or `/state`
as durable evidence: receipts and bindings belong to application-owned resources.

`env` is an explicit mapping on command/Docker steps. Ordinary command values may
render expressions; structured helper values are static strings or a direct
`${{ secrets.NAME }}` reference. Declare/forward credentials through the existing
reusable-workflow secret interface. Only symbolic credential aliases are retained;
values resolve for the current execution and remain outside snapshots, contracts
and identity fingerprints. Existing redaction applies to results and diagnostics.
Environment declarations never interpolate credentials into the pinned command.

The editor's **Helper protocol and environment** section supports protocol,
effect, input and both schema forms. Export preserves names and mappings, and
following steps receive suggestions from structured output schemas. Business
fields use explicit output paths such as `${{ steps.current.outputs.head }}`;
dependency references use `${{ needs.capture.steps.current.outputs.head }}`.
