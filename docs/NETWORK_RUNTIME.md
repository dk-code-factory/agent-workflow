# Shared Linux network runtime

`execution.network-runtime` keeps agent and command networking effects in an
explicitly prepared, persistent runtime. It applies to local and container
execution. A top-level selection also contains controller-launched stdio MCP
processes; roles cannot override that selection by choosing local execution or
another runtime. HTTP MCP transport remains a controller connection.

```yaml
execution:
  network-runtime: development
agents:
  coder:
    provider: codex-cli
    sandbox: workspace-write
```

The runtime supports **Linux, rootful Podman, Netavark/Aardvark and slirp4netns**.
Docker Engine and other platforms are rejected when containment is selected.
Configurations without a selection retain their existing execution behavior.
This separates network effects; it is not a security sandbox against arbitrary
host-root access, an explicitly selected host Docker API, or an unrestricted
privileged container. Host-interface tests require a separate execution policy.

## Operator preparation

Build with `make build`, then explicitly enroll the existing operator and start:

```sh
sudo ./bin/agent-workflow-runtime prepare development --uid "$(id -u)"
sudo ./bin/agent-workflow-runtime start development
./bin/agent-workflow-runtime status development
```

Preparation verifies existing sudo authority to start `/usr/bin/podman system
service --time=0`; narrow read-only Podman permission is insufficient. The runtime
API carries the complete root service user's authority, as described in
[Podman's service security documentation](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html#security).
Preparation copies the launcher to a root-owned registry and enrolls that UID.
It installs no setuid bits or sudo rules, refreshes no login, and does not
bootstrap Podman access. Stop before preparing a new launcher or enrollment.

The host needs `/usr/bin/podman`, `/usr/bin/slirp4netns`, `/usr/bin/nsenter`,
`/usr/bin/setpriv`, `/usr/sbin/ip`, and namespace/mount privileges. Rootless
Podman, Docker, and macOS are outside this runtime's compatibility boundary.
Workflows never prepare or start the service automatically. Missing, changed,
or unhealthy runtime identity fails admission and launch without host fallback.

Root-owned registration, generation, membership receipts, forwarding ownership
and the installed launcher live in `/var/lib/agent-workflow/runtimes/NAME`.
Logs are in `service.log`. Unix sockets and private networking state live under
`/run/agent-workflow-runtimes/NAME/IDENTITY`. Unix peer credentials authenticate
both launch and API proxy connections. Only enrolled UIDs and root can connect;
caller messages cannot supply another workload UID.

The controller stays on the host. The workload and rootful Podman service join
persistent network and mount namespaces. Private mounts cover
`/run/containers/networks`, `/etc/containers/networks`, `/run/netns` and the
resolver. slirp provides outbound networking. Only the runtime TAP interface
sets `route_localnet=1`, allowing explicit slirp forwarding to runtime loopback;
no host interface, route, firewall or resolver setting is changed. Health checks
verify pinned namespace handles, private mounts, loopback/TAP, resolver responses,
slirp and the Podman API. A failed check latches the generation unhealthy.

## Standalone Codex and Go applications

Independent shells select the runtime by name; inherited shell state is not
required:

```sh
agent-workflow-runtime exec development -- codex
agent-workflow-runtime exec development -- codex exec --sandbox workspace-write 'Run the tests'
agent-workflow-runtime exec development -- make test
```

Launch a fresh provider process. Do not connect it to an existing host-side
provider/app-server daemon. The launcher preserves the original working
directory, UID, primary and supplementary groups, caller umask, environment, authentication
paths, arguments and standard streams. Existing approval and sandbox arguments
remain applicable. Every invocation has its own guardian and lifetime channel;
normal completion, cancellation or caller loss stops its descendants without
stopping the runtime or another invocation. Cleanup uncertainty remains visible.
The authenticated launch peer supplies its own umask; only its isolated workload
helper applies it, so concurrent callers cannot change the shared service mask.
Launch metadata travels through a consumed helper environment entry and a sealed
root-member memory file. Credentials stay out of helper/guardian arguments and
persistent membership records; transport entries never reach the workload.
Extra inherited file descriptors beyond standard streams are rejected.

When stdin and stdout are terminals, standalone `exec` allocates a
private PTY and gives the workload its own controlling terminal and foreground
process group. This supports `/dev/tty`, individual keystrokes, Ctrl-C and window
resize. The frontend temporarily relays the caller's terminal in raw mode and
restores its saved settings on completion, launch errors and handled signals.
Terminal diagnostics share that PTY; redirected stderr retains its original
descriptor and stays separate. Noninteractive streams and pipes retain their
original descriptors and separation. SIGTERM cancels only that
invocation; terminal children and detached descendants remain owned by its
existing guardian.

For an embedding application such as Code Factory:

```go
policy, err := runtimeexec.PinPolicy(ctx, runtimeexec.Policy{
    NetworkRuntime: "development",
}) // persist this reference in the application's admitted request
if err != nil { return err }
cmd := exec.CommandContext(ctx, executable, args...)
cmd.Dir, cmd.Env = workspace, providerEnvironment
cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, diagnostics
stop, err := runtimeexec.SupervisePolicy(ctx, cmd, policy)
if err != nil { return err }
defer stop() // call again after Wait if an earlier call requested interruption
return cmd.Run()
```

Pass a durable `runtimeexec.Binding` for application invocation journaling.
Reuse the admitted policy on recovery; `PinPolicy` checks a retained reference
instead of resolving a replacement. Code Factory's direct `codex.Start` calls
must adopt this boundary themselves. This repository does not change or protect
those separate-repository launches.

Local workload environments receive `DOCKER_HOST`, `CONTAINER_HOST` and
`AGENT_WORKFLOW_NETWORK_RUNTIME`. Conflicting context selectors are removed.
Codex CLI/app-server shell filtering explicitly retains these values. The
engine's container policy continues to exclude the runtime socket, host homes,
SSH agents and controller credentials from container mounts. Selecting a
runtime does not grant a restricted container access to the rootful API.

## Shared storage and forwarding

The existing rootful graph/image store and **container metadata are shared**.
No image cache is copied per runtime or invocation. Container names can collide
with host workloads and other runtimes. A host or runtime `prune`, `reset`, or
container removal can affect that shared metadata; do not treat these as
runtime-scoped operations. Network definitions and DNS helper state are private.
Engine cleanup verifies the admitted endpoint/store fingerprint, exact container
ID and all ownership labels. It never prunes unrelated resources.

Published container ports initially listen inside the runtime. Explicitly expose
an IPv4 TCP service only on host `127.0.0.1`:

```sh
# Host 127.0.0.1:8080 -> runtime 127.0.0.1:3000
agent-workflow-runtime forward development 8080 3000
```

This includes services bound only to runtime loopback. Mappings retain their
owner UID and survive a clean restart. Repeating an identical owned mapping is
idempotent; different destinations, foreign owners and occupied ports fail.
Forwarding initialization/recovery failure prevents readiness. No LAN listener
is created. See [slirp4netns forwarding](https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1).

## Stop and uncertain recovery

```sh
agent-workflow-runtime stop development
```

Stop atomically closes membership admission and refuses active, reserved or
uncertain members, as well as retained container network namespaces. Container
creation reserves membership before effects and releases it only after exact
cleanup. Disconnecting one native caller closes only its lifetime channel.
Explicit stop owns runtime components only; no broad container cleanup occurs.

Failed admission health still permits inspection and cleanup of an existing
owned container through its retained identity, generation and endpoint. Recovery
checks the same daemon/store identity, exact container ID and ownership labels
before removal, then proves absence before releasing the reservation. The
unhealthy API proxy permits only recovery reads and deletion by full container
ID, checking every HTTP request; admission, launch, forwarding and general API
operations remain blocked. Unavailable API evidence or failed inspection/listing
retains uncertainty and never counts as absence.

A clean restart or daemon crash produces a new identity/generation and a new
API socket. Old snapshots and ownership records cannot silently follow it.
Native invocation supervisors retain root-owned completion receipts even when
the runtime controller dies. Restart accepts only confirmed-clean prior members.
A missing receipt or unresolved container reservation refuses restart for
operator inspection; there is deliberately no force-stop/forget switch that
would erase uncertainty. Preserve the registry, inspect the original workflow's
invocation records and the shared Podman metadata, and reconcile ownership
before changing retained records. Do not use a stored PID to kill a workload.

## Verification

Ordinary `make test`, `make check` and `make ui-pr-check BASE=origin/main` remain
deterministic. Unix protocol fixtures cover membership concurrency, disconnect,
stop admission, identity fencing, duplicate/lost acknowledgements, forwarding
ownership, uncertain receipts and failure closure without root or providers.

The separate opt-in harness needs rootful Podman, Go, Node, the UI's locked
Playwright dependencies, normal Google Chrome, outbound HTTPS, and the operator
privileges above:

```sh
npm ci --prefix ui --ignore-scripts
node conformance/network-runtime/acceptance.mjs --run-privileged
```

It creates only uniquely named fixture images, containers, networks and runtime
state. It uses a scratch image, never pulls an image, never invokes a model, and
never publishes GitHub resources. It checks three independent callers, DNS,
HTTPS, API access, published ports, runtime-loopback forwarding, cached-image
reuse, integrated local agent/command and container command execution, restart,
crash recovery, and overlapping Chrome HTTP/2 requests while checking unchanged
host links, addresses and routes. Root-owned receipts remain for operator audit.
