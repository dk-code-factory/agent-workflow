# Shared Linux network runtime

`execution.network-runtime` places agent and command processes and the selected
Podman service in an explicitly prepared, persistent runtime. It applies to local and container
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
Networking operations made through that service remain in its namespaces.
An operator-enabled BPF LSM boundary denies connections to other host Unix
sockets even when routing values disappear. Saved TCP/SSH container API routes
are rejected before workload execution. This closes the fallback described in
[#38](https://github.com/dk-code-factory/agent-workflow/issues/38).
This is not a security sandbox against host-root access, other API destinations
or unrestricted privileged containers.

## Operator preparation

Build with `make build`, or use the `agent-workflow-runtime` executable from an
extracted distribution in place of the source-build `./bin/` paths below. Then
explicitly enroll the existing operator and start:

```sh
sudo ./bin/agent-workflow-runtime prepare development --uid "$(id -u)" --socket-boundary
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

The host needs an active BPF LSM (`CONFIG_BPF_LSM`, kernel BTF, and `bpf` in
`/sys/kernel/security/lsm`), mounted bpffs at `/sys/fs/bpf`, a writable unified
cgroup v2 hierarchy at `/sys/fs/cgroup`, and clone3 with `CLONE_INTO_CGROUP`.
Enable these through the host
[kernel LSM configuration](https://docs.kernel.org/bpf/prog_lsm.html) and boot
configuration before starting the runtime. Startup never edits boot settings or
loads a replacement kernel. Absent BPF LSM support fails startup, including for
previously prepared runtimes: stop and re-prepare with `--socket-boundary`.
`status.socket_enforcement` must report `bpf-lsm-unix-v1`; older launch services
without this capability cannot admit new work.

The host also needs `/usr/bin/podman`, `/usr/bin/slirp4netns`, `/usr/bin/nsenter`,
`/usr/bin/setpriv`, `/usr/sbin/ip`, and namespace/mount privileges. Rootless
Podman, Docker, and macOS are outside this runtime's compatibility boundary.
Workflows never prepare or start the service automatically. Missing, changed,
or unhealthy runtime identity rejects engine admission and launch without a
replacement runtime. Saved network API routes are unsupported; deliberate
arbitrary remote APIs and unrestricted host root remain outside this boundary.

Root-owned registration, generation, membership receipts, forwarding ownership
and the installed launcher live in `/var/lib/agent-workflow/runtimes/NAME`.
Logs are in `service.log`. Unix sockets and private networking state live under
`/run/agent-workflow-runtimes/NAME/IDENTITY`. Unix peer credentials authenticate
both launch and API proxy connections. Only enrolled UIDs and root can connect;
caller messages cannot supply another workload UID.

Status replies preview at most 128 memberships and 128 forwards, with total
counts and explicit truncation markers. Active and uncertain memberships take
priority over completed history. The root-owned `status.json` and per-invocation
completion receipts retain the full audit record; truncating a reply never
changes ownership, admission or busy-stop decisions.

The controller stays on the host. The workload and rootful Podman service join
persistent network and mount namespaces. Private mounts cover
`/run/containers/networks`, `/etc/containers/networks`, `/run/netns` and the
resolver. slirp provides outbound networking. Only the runtime TAP interface
sets `route_localnet=1`, allowing explicit slirp forwarding to runtime loopback;
no host interface, route, firewall or resolver setting is changed. Health checks
verify pinned namespace handles, private mounts, loopback/TAP, resolver responses,
slirp and the Podman API. A failed check latches the generation unhealthy.

## Socket boundary and authentication exceptions

Every local workload is born in a root-owned, nondelegated leaf cgroup through
`CLONE_INTO_CGROUP`, before namespace entry, credential dropping, or user code.
Forked children, threads, exec replacements and detached descendants inherit
membership. Cgroup movement/delegation is not granted. Local Podman still runs
in the selected network/mount namespaces; clients needing host service sockets
now fail unless those sockets are explicitly enrolled.

The `unix_stream_connect` and `unix_may_send` BPF LSM hooks inspect the resolved
peer. They permit the generation's API/control sockets and sockets created by
workloads in the same protected cgroup. Other host pathname and abstract Unix
sockets are denied before requests reach their listeners. Symlinks, bind aliases
and saved custom paths do not change the filesystem device/inode identity. The
service and per-invocation supervisors hold socket inode handles, preventing
identity reuse while protected work remains alive. The host controller and the
runtime's Podman service remain outside the workload cgroup.

Authentication can require a host socket, for example an SSH agent. Explicitly
enroll each required socket while preparing the stopped runtime:

```sh
sudo ./bin/agent-workflow-runtime prepare development --uid "$(id -u)" \
  --socket-boundary --auth-socket "$SSH_AUTH_SOCK"
```

`--auth-socket` is repeatable and each preparation replaces the exception list.
It enrolls the current resolved identity; restarting the authentication agent
requires re-enrollment. Enrolling an API, generic proxy or service-launch socket
would grant its authority to every enrolled runtime user. Enroll only the
required authentication service. Existing file credentials and provider
configuration remain in place; restricted containers still receive no host
socket or home mounts, including these authentication exceptions.

Before executing the workload, a helper running as the caller inspects Podman's
system/user `containers.conf`, caller-specific rootless files and drop-ins,
`podman-connections.json`, and Docker's saved context metadata. It checks known
account/home defaults alongside configuration overrides so filtering a selector
cannot reveal an unchecked saved route. Native source roles determine JSON or
TOML parsing even for custom filenames. Saved `engine.env` endpoint defaults and
connection-file selectors are inspected as well. Configuration overrides
`CONTAINERS_CONF`, `CONTAINERS_CONF_OVERRIDE`, `PODMAN_CONNECTIONS_CONF`,
`XDG_CONFIG_HOME` and `DOCKER_CONFIG` are respected. All saved container
destinations must use absolute Unix endpoints, including unselected
connections/contexts. Non-Unix saved
transports and unreadable/malformed configurations fail admission to execution.
Move unsupported saved connections out of the selected configuration explicitly;
the launcher never rewrites them. This check covers saved selection at launch,
not arbitrary remote API configuration introduced by a running workload.

Frozen maps bind the cgroup and allowed identities to the exact runtime identity
and generation. Pinned LSM links in `/sys/fs/bpf/agent-workflow/IDENTITY` survive
service crashes. Root-owned `enforcement.json` under the generation's runtime
directory retains kernel object identities. Launch rechecks those identities
and refuses missing, replaced, stale or uninitialized enforcement. Clean
shutdown detaches only after the kernel confirms an empty cgroup and removes it
atomically against new launches. Uncertain or populated groups retain their
pins and receipts; preserve them for operator inspection. After an unacknowledged
guardian exit, the existing root member supervisor also retains the original
socket inode handles independently of the service, including service SIGKILL.
It records the uncertain outcome and closes its invocation streams and lifetime
channel before notifying the caller. The caller can finish or cancel without
waiting for this reference retention. Only kernel proof of an empty retained
cgroup releases those handles; missing or malformed observations keep them.
Because the cgroup is shared, another active member can conservatively keep an
uncertain supervisor and its descriptors alive indefinitely. Successful cleanup
acknowledgements still complete promptly without waiting for unrelated members.
A host reboot ends
protected processes and clears kernel pins; no retained invocation is silently
transferred into a new generation.

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
Launch metadata travels through private sealed memory descriptors, without
flattening the caller's arguments and environment into one executable argument
or environment entry. Credentials stay out of helper/guardian arguments and
persistent membership records; transport descriptors never reach the workload.
Extra inherited file descriptors beyond standard streams are rejected.
Native argument, environment and pathname bytes retain their Linux meaning;
transport adds no UTF-8 conversion or separate environment-entry count limit.

When stdin and stdout are terminals, standalone `exec` allocates a
private PTY and gives the workload its own controlling terminal and foreground
process group. This supports `/dev/tty`, individual keystrokes, Ctrl-C and window
resize. The frontend temporarily relays the caller's terminal in raw mode.
When stdin and stdout identify different terminals, the frontend proves that
stdin matches the caller's controlling terminal and has an authorized writer
for it before keeping stdout direct. Stdout then retains its original descriptor
and native output processing. Private PTY echo and `/dev/tty` output return to
the input terminal through that authorized writer.

After workload exit, ordinary completion waits for accepted relay output to
drain, so a blocked consumer can keep `exec` pending. Keyboard input controls
restore before this wait. Output processing on the relay destination remains
disabled until the wait ends, preserving bytes already processed by the private
PTY. The exact saved terminal settings then restore; restoration and drain
failures are reported. Cancellation permits a one-second drain grace and
then reports an unsuccessful drain if the consumer remains blocked. The relay
cannot forcibly interrupt an arbitrary caller-owned writer, and unsuccessful
cancellation does not guarantee complete output.
Diagnostics targeting the input terminal share its private PTY; other stderr
destinations retain their original descriptors. Noninteractive streams and pipes
retain their original descriptors and separation. SIGTERM cancels only that
invocation; terminal children and detached descendants remain owned by its
existing guardian.

Preservation defects remain when distinct, redirected read-only input has no
authorized writer, or the caller's controlling terminal is different or cannot
be proved. The existing combined relay still admits these inputs, but can change
output that depends on terminal columns, such as UTF-8 text followed by a tab
with `TAB3` enabled. These defects remain unresolved; they are not accepted
compatibility exclusions.

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
The Codex plugin below repairs these values for covered shell tool calls after
native filtering/login/snapshot preparation. Provider configuration arguments
are passed through unchanged. The engine's container policy continues to exclude the runtime socket, host homes,
SSH agents and controller credentials from container mounts. Selecting a
runtime does not grant a restricted container access to the rootful API.

## Codex routing plugin

Install the bundled plugin from the source checkout or extracted distribution:

```sh
codex plugin marketplace add /absolute/path/to/agent-workflow
codex plugin add network-runtime@agent-workflow
```

Explicitly enable the plugin and its native plugin/hook features, then use
`/hooks` in Codex to review and trust its current definition. Installation alone
does not trust hooks; changed definitions require another review. These are
operator actions. Agent Workflow does not install or trust plugins in your
normal Codex configuration automatically. See the
[native installation](https://developers.openai.com/plugins/build/plugins) and
[hook trust](https://learn.chatgpt.com/docs/hooks) instructions.

Start a fresh local Codex session through the prepared runtime:

```sh
agent-workflow-runtime exec development -- codex --enable plugins --enable hooks
agent-workflow-runtime exec development -- codex --enable plugins --enable hooks exec --profile work 'Run the tests'
```

Native profiles, authentication, approval and sandbox rules remain applicable.
The new command prefix changes approval-rule matching, so an existing allow rule
may require fresh approval. The plugin registers no approval-request hook and
must not be used to bypass native approval or trust review.

The POSIX trampoline validates the runtime name and invokes the protected
`/var/lib/agent-workflow/runtimes/NAME/launcher` shared Go hook handler. Use a
matching launcher build when preparing the runtime; an old/missing helper is
normally denied by the wrapper. The independent, nonsecret admitted-reference
marker preserves identity/generation even if login or snapshot setup changes
the public selectors. The handler checks the exact healthy runtime and current
network namespace before returning a rewrite. It executes no tool or privileged
launch service. Codex runs the command in its original shell/sandbox with the
original options and success-path exit status. The prefix sets exactly the three
routing values and stops before the original command if export fails.

The tested Codex 0.164.0 coverage includes direct shell tools, code-mode nested
dispatch, native named profiles, restrictive allowlists, snapshot/login setup,
unrelated filtered values, approval denial and sandbox restrictions. Outside
reserved runtime context the globally installed plugin is a no-op. Covered
ordinary malformed-context, runtime-probe and helper failures return valid deny
responses where the handler/wrapper can process the failure. Production use
requires Go launcher and POSIX shell availability, not Python.

Raw hook termination/timeouts, untrusted/disabled/modified hooks, a later
competing rewrite, direct `command/exec` RPC and native Codex-managed MCP startup
can bypass repair. Engine-controlled stdio MCP remains separately supervised.
Private `codex-app-server` roles do not inherit user plugins/config/trust from
normal `CODEX_HOME`; their hook coverage is not automatic. Do not copy that user
state into managed provider directories or relax managed-worker/tool-subset
certification gates. Use the native `codex-cli`/standalone setup above for the
documented operator installation path.

The hook remains a POSIX/local routing guardrail. The launch-layer socket
boundary applies independently of hooks, including direct command RPC and
descendant MCP startup. The recorded turn-interrupt experiment let long commands
continue in both baseline and plugin variants, so it proves no new per-turn
descendant termination. The Go invocation guardian remains the separate lifetime
boundary. Hook failure can still cause a client operation to fail instead of
routing to the intended API; the independent socket boundary prevents host Unix
API fallback.

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
API connections accepted after health loss permit only recovery reads and
deletion by full container ID, checking every HTTP request. Connections accepted
while healthy retain byte-stream forwarding for attached execution and protocol
upgrades; a later health failure does not retroactively filter those streams.
Engine admission, launch and forwarding remain health-gated. Existing API
streams retain their original generation; socket fencing does not revoke
already-connected streams on health loss. Unavailable API evidence or failed inspection/listing
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

Linux deterministic policy fixtures require a C compiler (`cc`). The embedded
CO-RE programs are included in distributed Go binaries; production hosts need no
compiler. To regenerate them after C source changes, install clang with the BPF
target and run `go generate ./internal/socketfence`. Source digests detect stale
embedded objects.

Ordinary `make test`, `make check` and `make ui-pr-check BASE=origin/main` remain
deterministic. Unix protocol fixtures cover membership concurrency, disconnect,
stop admission, identity fencing, duplicate/lost acknowledgements, forwarding
ownership, uncertain receipts and failure closure without root or providers.
Native C fixtures exercise the production policy decision function with
recording fake APIs and substituted kernel reads. These verify decisions and
zero requests following denial; they do not prove actual LSM attachment or
syscall interception.

Kernel enforcement has a separate explicit opt-in using disposable recording
Unix APIs, no Podman daemon, and a BPF-LSM-enabled host:

```sh
sudo env "PATH=$PATH" AGENT_WORKFLOW_SOCKET_LSM_TEST=1 \
  go test ./internal/socketfence -run '^TestKernelSocketBoundary$' -count=1 -v
```

This checks syscall denial through a saved socket alias, permitted runtime and
authentication sockets, inherited descendant membership, stale generations and
enforcement retained after the service handle closes. It is skipped by ordinary
validation. The Podman/Chrome harness below additionally requires the BPF LSM
prerequisites and remains separately opt-in.

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
host links, addresses and routes. The monitor's actual netlink multicast
subscription is confirmed before runtime churn. It also invokes the production
plugin trampoline/Go helper against the real runtime, including stale public
selectors and rejection of changed-generation or host-namespace context.
Root-owned receipts remain for operator audit.

The separate [production plugin experiment](../conformance/network-runtime/codex-plugin.md)
uses installed Codex with disposable configuration/trust/auth state, production
assets, local fake services and no external network route. Its root-authenticated
runtime control/status is a fixture, not the real Podman runtime. Together these
checks establish the respective native-hook and actual-runtime interactions;
neither substitutes for BPF LSM kernel certification or live model certification.
