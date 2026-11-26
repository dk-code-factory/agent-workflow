# Managed Codex build design for issue 134

This design is under implementation. No managed Codex profile is enabled. The
stock `codex-app-server` adapter continues to reject managed tool subsets and
repair. Passing an engine fake-process test cannot certify an upstream binary.

## Build identity and admission

Keep the existing `codex-app-server` backend and workspace-owned runtime role.
The proposed build profile binds upstream commit
`0a94398fa60c4fac0e7659e4213a0a36e613dd46` (rust-v0.167.1), the complete
repository-owned patch digest, protocol revision, source recipe digest, target
platform, and binary digest. Neither the version string nor a new experimental
field is sufficient evidence. A build must identify that tuple in its initialize
response, and the engine must verify the executable digest before sending task
data. The executable and build identity remain fixed for that provider process.
Patch and recipe digests are injected through build inputs or a verified build
sidecar. The binary digest is computed after the build and verified against the
executed image; no artifact embeds its own digest or the digest of its own source
patch. This keeps the provenance tuple acyclic.
Stock 0.170.0, commit `29eb79e9866b3262a777fdf8195c1b9e63684c91`, has the
same missing public policy boundary and supplies no substitute proof.

Admission must retain the complete build identity and immutable policy digest
before claiming work. The native admission marker, historical definition
snapshots, role bindings, effect receipts, and turn/repair allowances remain
authoritative. Old stock sessions remain on their historical route. A managed
request must reject unmanaged or unknown history; an old snapshot must not be
silently upgraded or assigned a larger allowance. Rollback disables new managed
admissions and leaves retained managed work requiring its original executable.

## Source patch boundary

The initial patch is limited to the upstream protocol, app-server processor,
core session/turn scheduler, and dynamic-tool handler. It does not create another
agent loop or bypass the engine broker. The patch must supply these connected
boundaries before its profile can be admitted:

| Boundary | Required source change |
| --- | --- |
| Public policy | Typed experimental `agentWorkflowPolicy` on thread/start and thread/resume, with protocol version, build identity, policy identity and exact flat broker wire names in the canonical default namespace. This profile always selects isolated startup and rejects native/namespace-container tools. Validate before starting a session or accepting model input. |
| Runtime enforcement | Convert the validated request to the existing startup `ToolPolicy` and `SessionIsolation::Isolated` through `ExtensionDataInit`. No generic config overlay may relax this ceiling. Unknown tools, aliases outside the canonical namespace mapping, discovery, code mode, delegation, or extensions outside the retained policy remain unavailable. |
| Retention | Store the full immutable policy with session creation metadata. Compare it before cold resume, warm attach, fork, internal resume, or child creation. Missing, changed, or unsupported identity fails before thread execution. Native children and all fork routes stay disabled until inheritance has its own source and conformance proof. Engine workers remain the supported child mechanism. |
| Turn identity | Require typed `agentWorkflowInvocation` on each managed turn/start. Carry it through turn extension data and bind each model batch to thread, turn, engine invocation, unique batch identity, model response identity and immutable policy. Reject duplicate call IDs and catalogs/batches exceeding 128 calls before scheduling work. Buffer early callbacks until the engine validates the turn/start reply and the exact invocation mapping. |
| Planned work | Force managed tool dispatch to use the existing deferred path. Register every planned tool future before dispatch, including denied/unknown calls. Seal the complete plan only after response.completed, stream disposal, and all allowed post-request work has joined. An incomplete stream cannot seal a batch. |
| Park attestation | Start the accounted deferred futures. Emit a typed attestation only when every planned future is either terminal or parked at a correlated broker response channel, and every nonbroker work category is empty. Unpolled futures count as outstanding work. No outstanding work is inferred from an empty callback map. |
| Reacquisition | Broker results cannot resolve their source-owned response channels until an exact batch/call-set reacquire permit is accepted. The event reader remains live while callbacks wait. The engine suspends its lease only on that attestation, buffers all correlated batch results, then reacquires once before issuing the permit and delivering the complete batch. Reacquiring on the first result can deadlock another callback requiring child capacity. A permit is single-use and cannot grant another batch or invocation. |
| Cancellation | Cancellation or connection loss invalidates the park token and every pending permit. Before-spawn dispatch guards account for child tasks; join them after joining the model task. The upstream model-task abort has a 100 ms grace period and cannot itself prove dispatch tasks have exited. Transition/receiver/submission failures revoke and join directly, including when the submission queue is closed. A mismatched result clears its batch before returning an error; the operation handler interrupts and joins. Native pending-mail automatic turns are disabled. Engine journals retain usage and uncertain effects. |

Before native session construction, the profile inserts the existing
`McpServersDisabled` marker, which empties configured and contributed MCP
registrations before discovery. It disables incompatible native startup features
and rejects managed requirements that would restore them. Source guards suppress
plugin recommendations, shell snapshots, MCP refresh workers and startup model,
auth and code-mode prewarm. Session isolation and tool filtering alone do not
establish these exclusions.

The first bounded source profile excludes native execution, background work,
hosted tools, executor MCP discovery, code mode, native delegation, and hooks.
This restriction must be enforced at construction and dispatch. Allowing any of
those paths later requires accounting for all work they can schedule, including
descendants, before quiescence. An executable background process surviving a
native callback is a counterexample, not a completed work receipt. A restricted
profile must reject unsupported requested tools before admission; it cannot
claim unrestricted native-tool parity.

Tool-free repair is a fresh isolated session with an empty ceiling, no broker
catalog, no inherited task/session input, and only the repair data. Structured
output must be validated by the engine rather than enabling an SDK formatting
tool. Repair consumes the original retained task allowance but cannot replace
the worker's original continuation session.

## Exact source anchors

These links identify the pinned source, not latest documentation:

- [ToolPolicy](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/ext/extension-api/src/tool_policy.rs#L6)
  supplies an exact startup ceiling and explicitly requires resupply on resume.
- [SessionIsolation](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/ext/extension-api/src/session_isolation.rs#L9)
  removes inherited instruction providers, extensions and implicit discovery;
  explicit instructions and permissions remain the caller's responsibility.
- [Thread startup](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/app-server/src/request_processors/thread_processor.rs#L1582)
  currently inserts selected capability roots, not the private policy. The
  [cold-resume start](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/app-server/src/request_processors/thread_processor.rs#L4334)
  constructs default extension data. Both need explicit immutable policy handling.
- [Tool scheduling](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/stream_events_utils.rs#L352)
  dispatches immediately unless the deferred path is selected. An unpolled
  future therefore cannot be assumed inert without forcing that path.
- [Batch scheduling](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/session/turn.rs#L3144),
  [response completion](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/session/turn.rs#L3331),
  and [stream disposal and drain](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/session/turn.rs#L3541)
  provide the source-owned plan/seal/accounting insertion points. Completion of
  the final drain cannot be the release barrier: it already awaits broker results.
- [Dynamic response wait](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/tools/handlers/dynamic.rs#L203)
  currently parks one callback. It does not attest invocation-wide quiescence.
- [Native task spawn](https://github.com/openai/codex/blob/0a94398fa60c4fac0e7659e4213a0a36e613dd46/codex-rs/core/src/tools/parallel.rs#L271)
  starts an independent Tokio task. All such tasks must be accounted or excluded.

## Deterministic proof matrix

Every row needs the actual patched executable against a local fake gateway and
the common engine harness. Fake processes separately test malformed-wire
rejection; they do not establish upstream enforcement.

| Positive control | Required negative controls |
| --- | --- |
| Exact broker subset and empty ceiling | Force omitted builtin, MCP, namespace alias, discovery, nested skill/delegation and unknown tool; no dispatch or effect. Reject unsupported native tool requests at admission. |
| Isolated repair and original continuation | Force a tool call despite empty advertisement; original prompt/input/instructions/session nonces absent; no effect; original worker resumes original session; original allowance charged. |
| Same-policy cold resume | Missing/changed policy, build identity, old unmanaged history, and child/fork/internal resume cannot restore default authority. |
| Capacity-one broker child wait | Delay final stream delta, add a late planned call, leave a future unpolled, and delay post-request work; no attestation or suspension until every accounted condition holds. |
| Parallel broker calls with stable IDs | Each call is parked before release; complete results are buffered before one parent reacquisition. Duplicate ID with changed arguments, omitted/extra batch IDs, false quiescence, partial or reordered results, and callback after seal fail closed. |
| Permit after engine reacquisition | Early, conflicting, replayed, wrong-generation and wrong-batch permits cannot resume source execution; results arriving before permit do not release a response channel. |
| Restart and cancellation | Kill controller/provider while parked, cancel while reacquiring, lose result after durable effect receipt, and race completion against a new owner; no duplicate effect, turn allowance or budget charge. |
| Usage and retained budgets | Partial/final usage across park/repair/resume and worker continuation use original journal semantics; exhausted retained allowances reject before another provider turn. |

## Bounded local build plan

The owned source copy is `/extra/i134-upstream`, extracted from the exact commit
without modifying upstream checkout refs. Its tracked files total 98,336,010
bytes. Rust 1.95.0 is already installed. Existing Cargo registry/git caches total
about 9.8 GB and remain shared read-only inputs; they are not cleanup targets.

The [disabled profile manifest](../patches/codex/profile.json) binds the exact
source patch, [recipe](../scripts/codex_managed_build.py) and
[offline helper](../scripts/codex_offline_exec.py). The recipe creates fresh owned
source storage, verifies patch application, freezes the complete source tree and
records an acyclic build identity. Cache preparation copies locked registry
archives/index entries and pinned Git/submodule objects into disposable storage;
shared extracted sources, checkouts and writable aliases are excluded.

Run metadata preparation before compilation. The recipe's default check phase
uses `cargo metadata --locked --offline --filter-platform` and records the actual
app-server normal/build dependency closure plus build-script digests. The compile
phase requires that exact metadata admission and rejects unprepared V8 native
artifacts. Runtime feature flags cannot establish compile-time dependency absence.
Missing dependencies remain explicit preparation failures, with no implicit
download, lock update or provider execution.

The release tag's manifest declares workspace version `0.167.1`, but its
original lock retains `0.0.0` for 171 workspace packages. The owned patch corrects
only those workspace versions. Preparation checks both lock hashes and compares
the complete parsed lock: all 1,308 external package records and other fields
must remain identical. External dependency changes fail preparation.

Missing registry archives require a separate explicit preparation command,
[codex_prepare_archives.py](../scripts/codex_prepare_archives.py). It first checks
the existing cache, then downloads only the reviewed missing locked crates from
canonical `https://static.crates.io` URLs, with proxies disabled and redirects
restricted to that host. Each original lock checksum must match before atomic
installation into owned storage. Limits are 53 archives, 64 MiB per archive,
512 MiB total, 15 minutes and a 24 GiB free-space floor. This step never runs
Cargo or changes a lock. Subsequent Cargo stages remain locked and offline.

Every Cargo/build child runs under the inherited Linux syscall filter. It denies
non-Unix socket families and io_uring operations while preserving Unix IPC and
ordinary compiler children. The ABI uses the
[libseccomp 2.6.0 header](https://github.com/seccomp/libseccomp/blob/v2.6.0/include/seccomp.h.in).
Deterministic controls verify a forced HTTP download cannot reach a local fake
server, subprocess IPv4/IPv6 access fails, and a compiler child and linker work.
This filter is build-only and makes no provider/filesystem isolation claim.

Source checks use Rust 1.95.0, an owned physical TMPDIR and target directory, two
build jobs, incremental compilation disabled and debug information disabled.

Each compile stage must have a 30-minute deadline and a monitor that terminates
only its owned process group if the owned target reaches 12 GB, combined owned
cache/target reaches 16 GB, or the physical filesystem has less than 24 GB
available. Escalation kills the entire group even after its leader exits; receipts
retain descendant states and before/after source hashes. Record sizes; preserve
shared caches and foreign targets. Check protocol/core compilation first, then
build only the app-server test executable and required local conformance cases.
Do not build release images, all targets, or distribution artifacts at this
stage. Source checking and helper unit tests alone do not enable the profile.

The repository-owned source patch is a prototype. Retained preparation evidence
records the initial missing Git submodule, the release workspace-version lock
mismatch, and the separate checksum-verified preparation of 53 absent archives.
The corrected source passed locked offline app-server metadata resolution: its
normal/build closure contains neither V8 nor the code-mode runtime. Compiler
checking is in progress. Exact executable fake-gateway evidence and the connected
Go adapter remain required before enabling the profile. No verified binary or
managed Codex conformance result is claimed by this design document.
