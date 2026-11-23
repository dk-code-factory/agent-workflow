# Manual Agent Workflow plugin acceptance

These three named subjects are separate from ordinary tests, PR/main CI and
recurring release gates. Run them only with current explicit authorization:

| Subject | Intentionally incomplete request | Required observations |
| --- | --- | --- |
| `plugin-parallel-audit` | Audit this repository with multiple reviewers. | Clarify scope/output; two independent native agent jobs overlap, combine structured findings; fixture hashes unchanged. |
| `plugin-bounded-fix` | Fix the small Python bug and check it. | Clarify allowed edits/completion; passing tests, independent review, at most two implementation iterations; no publication. |
| `plugin-gated-report-recovery` | Process CSV and make an approved report. | Clarify validation/approval; owned controller restart at wait, same run/gate, stale answer rejected, exactly one preparation, correct report. |

The [driver](acceptance.py) seeds disposable Git repositories, installs the
production plugin through native Codex marketplace commands in an isolated
`CODEX_HOME`, and communicates over native app-server JSON-RPC. It requires the
repository-pinned Codex version (currently 0.167.1), a local candidate archive
and expected digest, and an existing explicitly selected native credential
route. It does not alter normal Codex state, install provider versions, run
login, create test issues, or publish anything. It copies selected authentication
into private state and removes refresh authority; unavailable authentication
fails the attempt. Node's locked Codex dependency can be installed explicitly
with `npm ci --prefix images/codex --ignore-scripts --no-audit --no-fund`.

The isolated outer authoring client's workspace-write sandbox enables network
syscalls because Codex's network-disabled sandbox also denies the engine's local
Unix-socket control protocol. This does not authorize outbound tool calls or
publication; inspect native tool traces for both. Nested workflow agents retain
their separately configured sandbox. The temporary client configuration is
confined to the fixture, and no normal installation settings are changed.
The harness owns the controller outside native shell tool lifetimes, so ending
a tool call cannot kill it. A separate short private cache keeps Unix socket
paths within the platform limit. Both writable roots are confined to the fixture;
`cache-location.txt` records the owned cache for later inspection and cleanup.

Example invocation (live; never part of `make test`/`make check`):

```sh
python3 conformance/plugin/acceptance.py --allow-live \
  --scenario plugin-parallel-audit \
  --codex /absolute/path/to/pinned/codex \
  --credential-file /private/existing/auth.json \
  --candidate-archive /local/agent-workflow-0.2.2-linux-amd64.tar.gz \
  --sha256 EXPECTED_ARCHIVE_SHA256 \
  --allowance /private/plugin-acceptance/allowance.json \
  --evidence-parent /private/plugin-acceptance
```

Use the **same allowance journal for all three subjects**. It admits one attempt
per subject, at most 15 minutes each and 45 minutes total, including waiting and
nested workflow agents. Time is reserved before the first native conversation,
so a killed harness does not reclaim allowance. A failed attempt stays failed.
Further attempts need renewed explicit authorization and a new allowance;
never delete or edit the old journal. The driver refuses CI.

The operator supplies newline-delimited JSON on stdin after each native turn:

1. Read the model's clarification and provide focused answers through
   `{"message":"..."}`. The canonical subject answer packets are in
   [subjects.py](subjects.py); use them to answer actual questions, continuing
   until material requirements and architecture are settled. Native
   `requestUserInput` receives that packet automatically. Do not send approval
   while decisions remain open.
2. Inspect the full implementation/test plan and private native tool trace.
   Verify no implementation attempt preceded approval, including failed write
   attempts. The driver additionally checks unchanged workspace bytes after
   every preapproval turn, but that alone cannot prove no attempted mutation.
3. Send `{"approve":"PLAN_SHA256"}` using the current digest printed by the
   driver. This records the exact plan and sends explicit approval in a separate
   native user turn. Inspect implementation and validation evidence.
4. Send `{"run":true}` to explicitly invoke the installed run skill once. Keep
   the durable run ID. For the gate subject, send `{"recover":"RUN_ID"}` while
   waiting: the harness stops its owned controller, resumes that same run,
   checks gate identity, rejects stale evidence and submits the current approval.
5. Inspect native conversations and tools, admitted definition, attempts,
   dependencies, structured results, limits and fixture artifacts. For audit,
   compare distinct native agent attempts' `[started_at, finished_at]` intervals
   and check a later dependent combine job. For repair, run immutable tests and
   inspect retained iterations and review output. For gate recovery, inspect
   `recovery.json`, `processed.count`, consumed gate and report. Inspect traces
   for remote publication attempts in every subject. Judge behavior and artifacts,
   not particular phrases, per [OpenAI skill-testing guidance](https://developers.openai.com/plugins/build/skills).
6. Only after actual review, send `{"finish":{"necessary_questions":true,
   "resolved_architecture":true,"complete_plan":true,
   "no_implementation_attempt_before_approval":true,"artifact_checks":true}}`.
   A failure or unsatisfied criterion must remain a failed subject.
   Record failed observations with `{"fail":{"reason":"...","observations":{}}}`.

Raw native traces, agent state, credentials and review notes stay in the private
mode-0700 evidence directory. The separately stored sanitized summary records
version, candidate digest, elapsed time and observed native/nested usage. Cost
remains unavailable unless the provider reports it; never estimate it as zero.
Review summaries before copying them into repository evidence. The driver stops
only the disposable workspace's controller on completion or failure.

Deterministic scripts test installers, packaged resources, local bounded
failures, CSV validation, declined approval and gate recovery. They cannot prove
the model follows a skill. Candidate acceptance does not establish public
distribution readiness: after the signed-tag release gates succeed, install
from the actual tagged marketplace and verify real manifest provenance,
download/platform identity and cached offline execution. That remains a separate
post-release check and does not authorize publication from this harness.
