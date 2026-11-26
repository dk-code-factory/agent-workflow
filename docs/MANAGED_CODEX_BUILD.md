# Managed Codex build design for issue 134

The repository supports one source-pinned local Linux amd64 broker-only Codex
profile. Its canonical gate is temporarily disabled while the revised source
build completes deterministic validation. Stock `codex-app-server` continues to reject managed tool subsets and
repair. The managed profile requires its captured build manifest, held executable
identity and native protocol handshake; a version string or engine fake process
is insufficient. Whole-PR validation remains a separate readiness requirement.

## Build identity and admission

Keep the existing `codex-app-server` backend and workspace-owned runtime role.
The proposed build profile binds upstream commit
`0a94398fa60c4fac0e7659e4213a0a36e613dd46` (rust-v0.167.1), the complete
repository-owned patch digest, protocol revision, source recipe digest, target
platform, and binary digest. Neither the version string nor a new experimental
field is sufficient evidence. A build must identify that tuple in its initialize
response, and the engine must verify the executable digest before sending task
data. The executable and build identity remain fixed for that provider process.
The workspace's optional `managed-build.manifest` captures the original manifest
bytes and full tuple in retained Roles. Later manifest replacement or removal
cannot recapture authority. Local launch holds an executable descriptor through
the existing invocation guardian and verifies the actual owned child image before
protocol input. Container, network-runtime and unverified platforms remain closed.
The manifest is a trusted workspace provenance input. Its digest, source tuple,
executed image and handshake do not authenticate a fabricated manifest from an
untrusted caller or a hostile workspace owner emulating the ABI. Unadmitted
repository content, skills and task/provider arguments cannot select or replace
the retained manifest. Conformance receipts bind the exact tested artifact;
there is no universal binary whitelist that excludes independent source-pinned
builds.
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
| Durable seal | Before the first public broker callback, materialize and flush the original local native rollout, explicitly sync its regular file and newly created directory entries, then atomically write/sync/rename/sync the full bounded broker journal. Bind independent original SessionMeta, turn/response envelopes, full original call set and native Completed usage witness. Missing, foreign, partial or ambiguous evidence holds. |
| Usage before effects | The attestation carries the original native Completed usage. Commit its stable response-ID observation before capacity release or any broker dispatch. Budget, storage or unknown-accounting failure holds the batch while preserving known durable charges. Zero automatic request/stream retries and no fallback transport prevent one later response from hiding an earlier unknown-cost attempt. |
| Receipt recovery | Use the same original turn, engine invocation and consumed allowance through CoreRecoverTurn/RegularTask. Validate original raw evidence before prompt normalization, replay original callbacks through the existing dynamic-tool path and original engine receipts, and require a fresh control permit epoch. Persist the entire semantic result set before resolving any callback; append only missing real outputs and sync completion before sampling. Cancelled batches and unknown native attempts cannot obtain a fresh invocation to bypass the hold. |

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

The [source profile manifest](../patches/codex/profile.json) binds the exact
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
only its owned process group if the owned target reaches 12 GiB, combined owned
cache/target reaches 16 GiB, or the physical filesystem has less than 24 GiB
available. Escalation kills the entire group even after its leader exits; receipts
retain descendant states and before/after source hashes. Record sizes; preserve
shared caches and foreign targets. Check protocol/core compilation first, run
explicit counted core test selectors, then build only the standalone
`cargo build -p codex-app-server --bin codex-app-server` target for local protocol
and engine conformance. Its startup argv does not contain the CLI `app-server`
subcommand.
Do not build release images, all targets, or distribution artifacts at this
stage. Source checking and helper unit tests alone do not enable the profile.

Historical prototype preparation evidence
records the initial missing Git submodule, the release workspace-version lock
mismatch, and the separate checksum-verified preparation of 53 absent archives.
The corrected source passed locked offline app-server metadata resolution: its
normal/build closure contains neither V8 nor the code-mode runtime. Historical
v12 source produced an actual standalone binary, SHA-256
`46da679ee4cfe106564ed68a28adffde6f85e0bc1fd1b4ca08c8c5c4b1d7c565`.
Owned local fake-gateway diagnostics verified its executed image, delayed complete
batch barrier, full result buffering, original durable journal ordering, normal
subsequent turn, unknown-attempt hold across restart, and actual directory write
denial before callbacks. These are bounded diagnostics: v12 retained one stale
unit-test expectation failure and did not implement pending-effect recovery.

The previously verified source tuple adds separate fresh permit epochs, original-seal replay
and usage-before-effects control. It passed compilation, counted source tests,
actual binary crash-cut/gateway cases and the connected production Go engine
harness. Historical binaries and private controller tests alone do not admit it.

## Source and native conformance

The preceding verified source build uses patch digest
`bd1e1ebd4608c30eaef97a7009b383c417060a83764a8513058b958d800930d6`,
source tree digest
`c4cad66776ac52a7fc92a7a52dec1dd90292d4268068018c68bc38cd6ae7a790`,
and build identity
`74c02cbfee1e7713a13c90702aa80f1ef533c06cd46fbc31abd611f3d3507b0d`.
The source recipe digest is
`5ad28b4f3ff6dc4b8db88494fc3db4309c8309fa54224440277d7493773b8899`.
Its actual standalone Linux amd64 executable is 509,664,312 bytes, SHA-256
`4058a428a4740700b4fdebfdf050886851a84d24924d8fd281d480770fed4648`.
The recipe passed locked offline metadata, 11 managed runtime tests, 11 journal
tests, one submission-ceiling test and the standalone binary build. Every stage
retained identical before/after source digests, its inherited network filter and
an empty owned process group after completion. These facts identify this tested
artifact; they do not impose a universal binary whitelist.
The new recipe tuple also passed 16 actual-executable protocol probes and the
focused production engine race suite. The probes include delayed stream sealing,
empty and denied tool ceilings, partial results, invalid/repeated permits,
cancellation, startup exclusions and retained policy mismatches. Earlier complete
builds and failed stages remain separate historical evidence.

The revised candidate changes only three source-test waits to use local
one-second failure deadlines. It has patch digest
`e199ca21257398300424f8cccee368fb74cf1ba97c4c3ea9e630739aa0c204fc`,
source digest
`a2ae12dd20ee47bab14e3545ca36159988b6acab7857d6bb54e62550c25884a5`,
and build identity
`4953fb670677ee6e0ae39d05ec88612c175a31a1370890366c289312ccc609fa`.
Its recipe and offline-wrapper pins remain unchanged. This candidate is disabled
until its counted source tests, standalone build and actual binary conformance
complete; the preceding binary does not prove the new build identity.

The production Go transport now verifies the held executable and actual guardian
child before initialize, checks the source build handshake, uses the maintained
skill isolation helper, and binds the full broker policy to the original worker
invocation. Immutable `agentWorkflow/modelCompleted` notifications carry the
witness captured at that exact native completion. They commit known charges only;
they do not establish durable seal, quiescence or authority to resume. Broker
effects additionally require the native full-batch attestation, official turn/start
readback, durable original usage, current engine ownership and the original turn's
remaining authority/deadline. Generic thread counters are not used for this profile.

Private bookkeeping in existing usage observation rows distinguishes managed
admission before thread/start can receive task data. It records no token charge.
Only that exact validated bookkeeping record can be excluded from response totals;
unknown observations, conflicting witnesses and uncertain attempts remain holds.
Each completed native response is charged once under its original response ID,
including replay in a later process. Known charges survive a later deadline,
storage, ownership or accounting failure; no usage is invented for an unknown
first attempt. Empty catalogs use the original engine context for accounting even
when public tools and bindings are absent.

Recovery retains the original mailbox message, start time, admitted boundary,
provider session and consumed allowance. It requires exclusive owner-loss recovery,
a newly admitted owner/generation, an unexpired original deadline, authoritative
runtime cleanup receipts, and no unresolved tool receipt. Original call IDs and
semantic results remain fixed; the source generates a fresh permit epoch. Public
mailbox acknowledgement cannot discard a managed pending or unknown invocation.
A private engine terminal checkpoint applies redaction, limits and fencing before
transport return. Terminal result, final accounting, marker completion and mailbox
acknowledgement commit together. A retained terminal checkpoint can settle a lost
completion acknowledgement without launching a provider.

| Evidence at this checkpoint | Scope and remaining work |
| --- | --- |
| Actual binary with capacity 1, two engine child workers, parallel spawn/wait batches, three original responses and four durable broker receipts | Original parent allowance ends at zero after one parent admission and two child reservations. Parent usage is 33 input/9 output tokens; two child fixtures add 2/2. Children use deterministic fake SessionRunners. The SDK independently exercises the same capacity-one engine contract below. |
| Verified native SIGKILL after two effects commit and before permits; SQLite reopen and new engine objects/current owner | Same original message/start time/zero allowance; each physical effect occurs once; original broker receipts retain their old generation; exactly two native API requests and 22/6 total usage. This is native process restart within one Go test OS process, not a whole engine OS process crash claim. |
| Owned disposable SQLite trigger aborts final mailbox commit after native terminal checkpoint | New owner commits the same saved terminal result, with no second provider process or request, unchanged zero allowance, and 11/3 usage counted once. This tests a failed completion transaction, not arbitrary hardware failure. |
| First native stream ends before Completed, despite forced retry/websocket configuration | One API request, no Completed observation, no broker callback, unknown usage retained; later queued work and public mailbox acknowledgement stay blocked across SQLite reopen/current-owner replacement. |
| Historical native crash cuts at seal, partial result buffering, accepted semantic results and real outputs before completion marker | Original IDs, exact real output list and fresh permit epochs were verified. The precise fsync boundary used an explicitly labelled owned test-only interposer; recovery ran without it. These native proofs supplement the connected engine cases. |
| Known-charge, conflict, overflow, deadline, ownership and bookkeeping controls | Focused race tests preserve prior charges, reject arbitrary unknown observations and charged bookkeeping, and reject replacement original results. They do not establish provider usage that was never observed. |
| Actual empty-catalog Codex repair followed by original-session continuation | Catalog sizes are 2, 0 and 2. Original instructions, prompt and input are absent from repair; repair data is absent from original continuation. Three responses total 33/9 usage and consume the original allowance. A forced omitted native tool produces no callback or file effect. The native fixture seeds existing private queued-repair fields; focused public output-controller and public SDK adapter tests separately verify the production repair caller and shared sanitation. |
| Known pre-dispatch denial versus external effect with lost reply | A committed denial performs no effect and permits a normal final response (22/6 usage). A lost reply after an actual disposable effect retains an uncertain receipt, one request and 11/3 known usage; later queued work and public acknowledgement remain blocked without spending the remaining allowance. |
| Explicit cancellation while parent reacquisition is blocked | Two completed physical effects remain recorded, one request has 11/3 known usage, and two unused turns stay unchanged across owner replacement. Runtime cleanup joins before recovery checks; cancellation cannot become original-message process-loss recovery. |
| Extra original call inserted only into owned native raw history | The independent sealed journal and engine witness remain unchanged. Cold resume rejects the full-call mismatch before another request or effect and retains the original message and known charge. Explicit managed peer rejection and failed native terminal status carry an integrity hold, while transport EOF remains distinct for verified process-loss recovery. |
| Ordinary worker emits progress or returns a result after its execution owner changes | Both publication paths retain the original worker/message bytes and publish no progress or completion event. The original execution fence now covers workers without native definitions as well as managed workers. |
| Locked SDK repair and forced automatic/reactive compaction controls | Actual fake-gateway repair and native summarization controls exist. The native automatic/reactive threshold negative is exercised. Separate idle/precompute positive-path activation was not triggered; their disable controls are verified against the pinned source. Shared broker recovery and accounting evidence is described below. |

The enabled profile requires the verified source tuple and retained admission
policy 2. Historical policy 0/1 snapshots retain their original route and cannot
acquire a managed build. The supplied-skill implementation and usage guards stay
separate; this Codex build declares selected native skills unsupported.
Unproven direct native execution/background work, native delegation, container and
other platform profiles remain closed. The SQLite/native-process recovery scope
does not establish a whole engine OS process crash claim.

A final promotion must keep build preparation reproducible. The recipe accepts a
strict boolean runtime admission gate independently of its immutable build tuple.
It checks the exact patch, recipe, offline wrapper and prepared source; an enabled
descriptor must declare the canonical source digest. Changing runtime admission
or descriptive status does not alter build identity. Preparing or building a
descriptor never enables admission. Final current
`make test`, `make check` and `make ui-pr-check` remain required after integration.

## SDK integration checkpoint after issue 139

The branch now includes the merged supplied-skill implementation. Selected
skills remain outside the managed Codex build profile and selected-skill output
repair remains rejected before provider setup. Fresh managed Codex admission
uses policy 2; historical policy 0/1 roles cannot acquire a managed build.
Admission requires the enabled source profile and its verified tuple and local runtime.

The locked SDK broker-only controller captures the original model response ID,
all original tool IDs/arguments, final usage delta and completed block set.
Callbacks queue until this full response proof is known and known usage is
committed. Each observed serial callback suspends the parent, invokes the engine
broker, reacquires the parent lease and then returns its semantic result. The
reader stays live during reacquisition. A later original callback from that
same plan is permitted exactly once; a new response cannot overlap an unresolved
plan. Final SDK counters must equal the sum of those original response counters.

A normalized SDK `message_stop` is not independently a completion proof. Native
2.1.322 also emits it after a source error. Its source explicitly accepts a
finished block set plus final stop-reason/usage delta as a complete response,
including a connection error after that delta. The local control verifies that
any next model request follows both actual broker results with the parent lease
held. Missing final usage and unadmitted trailing tools must instead stop the
original native process before iterator cleanup can synthesize interrupted
results and continue. The public `spawnClaudeCodeProcess` hook retains the exact
owned ChildProcess for that termination; it preserves the SDK-provided command,
arguments, directory, environment and forwarded signal. The existing outer
runtime guardian still joins descendants and records cleanup.

Current deterministic tests include a capacity-one SDK engine invocation with
two child workers, four broker receipts, three API responses, zero remaining
parent allowance and 50/17 aggregate tokens; delayed final-delta/EOF, omitted
final-delta, trailing unadmitted-tool and cancellation during reacquisition
controls; and SQLite-reopen holds retaining an original known 16/5 token charge.
The private attempt marker is installed before task data can reach the SDK.
Only the private original-message terminal checkpoint and existing completion
transaction can settle it. An unknown SDK callback is held rather than replayed.

The public YAML `agent/run` test exercises both adapters through the ordinary
`Runner.RunTurn` entry, including continuation by the original agent reference.
It retains one worker and native session, two original messages and complete
accounting. Ordinary tasks keep the existing WorkerManager path.

Conversation participant turns use a private tagged original Attempt, not a
worker surrogate. The tag binds the actual physical attempt coordinates and
input/start time to its persisted origin owner and parent conversation Active,
Calls, maximum allowance and deadline. Public tests exercise three turns,
original send/send/finish receipts, two retained participant sessions and complete
accounting on both adapters. Final accounting and marker settlement commit in
the existing `Journal.Complete` transaction. Completed journals are reused without
another provider request. Unknown or unsettled conversation attempts require
explicit reconciliation: they do not automatically replay native callbacks or
saved terminals. Before Begin can reuse or create any matching attempt, an indexed
origin/logical-coordinate guard checks every earlier matching original, including
parent turns, older unresolved rows and ordered physical siblings. Public retry
after an actual first-request loss preserves Calls/Active/deadline and launches
no replacement request.

SDK native terminal cost is cumulative across fresh-process resume, while the
response token counters used here are per invocation. The managed SDK completion
transaction retains the raw native cost and applies a durable origin/session/
profile/model cost highwater. Only cost uses `DeltaCostUSD`; original response
token totals stay flat. Two actual public messages reporting $0.000123 and
$0.000246 charge $0.000246 altogether. Original receipt replay, lost completion
acknowledgement and transaction rollback cannot double-advance the baseline.
Malformed, negative, nonfinite, regressing or changed original costs hold without
changing earlier charges. A retained SDK session must have this exact profile's
original settled terminal and cost baseline before task data. Older unmanaged
history cannot silently adopt the profile; it requires explicit reconciliation
or a fresh admitted session. No new USD or token limit is introduced.

A private SDK original-message checkpoint saves the validated, redacted and
limited native terminal result. After a failed completion transaction, verified
owner-loss recovery can commit that original result and accounting without a
new native process or API request. Unknown active SDK callbacks remain held;
completed engine receipts are retained, but native callback replay is unsupported.
Direct native tools combined with broker lease release are rejected at fresh
configuration/task/conversation admission and again at runtime. Native-only and
selected-skill routes keep their separate policies.

The manifest remains trusted workspace authority; these controls do not provide
filesystem isolation or authenticate a hostile workspace owner. Native restart
proofs use SIGKILL, SQLite reopen and new engine objects inside the same Go test
OS process. They do not establish whole-engine OS crash or hardware durability
claims. Current whole-PR checks, UI verification, independent review and CI remain
separate from this deterministic provider conformance evidence.

## Reproducing bounded shared conformance

Install the locked Node dependencies with `npm ci --prefix bridge/claude` and
build the exact source tuple with the recipe above. Then run:

```sh
AGENT_WORKFLOW_TEST_MANAGED_CODEX_MANIFEST=/absolute/path/managed-binary.json \
  make managed-conformance
```

This entry enables only local fake gateways and disposable state, runs counted
race tests for both adapters, and requires the manifest rather than treating a
skipped Codex arm as proof. It does not build Rust, call a real provider, refresh
credentials or run publication/release scenarios. Native CI runs the SDK arms;
the custom Codex source build remains an explicit separately prepared artifact.
The tests live in [managed_conversation_test.go](../pkg/clirunner/managed_conversation_test.go),
[sdk_managed_engine_test.go](../pkg/clirunner/sdk_managed_engine_test.go),
[sdk_managed_terminal_test.go](../pkg/clirunner/sdk_managed_terminal_test.go),
[sdk_broker_boundary_test.go](../pkg/clirunner/sdk_broker_boundary_test.go) and
[managed_task_attempt_test.go](../pkg/workflows/managed_task_attempt_test.go).

The SDK records a validated native terminal's known cumulative cost before
waiting for EOF or cleanup. This incomplete original receipt retains its exact
cost delta without advancing the session baseline, finishing usage or settling
the attempt. Later cancellation, protocol errors and cleanup failures retain the
hold. Successful original completion upgrades that same receipt and advances
the baseline atomically. Returning each broker result also rechecks the original
durable owner after capacity reacquisition; a live context alone is insufficient.
