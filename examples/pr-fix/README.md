# Portable PR maintenance

This example runs PR maintenance with the stock `agent-workflow` binary. YAML
chooses tasks, parallel read barriers, bounded review/fix cycles, same-session
checklist follow-ups, finalizers and publication gates. Standard-library Python
helpers run in a caller-selected, digest-pinned container image. The controller
owns provider sessions, worker waits, scheduling and guarded recovery.

Read the [setup, execution and recovery guide](../../docs/PR_FIX.md) before use.
The example performs real GitHub operations only when selected permissions and
the exact original user authorization allow them. The sample disables remote
permissions and supplies no authorizations.
Its native `workspace-write` sandbox permits local source edits after the
required iteration authorization is supplied.

## Files and requirements

- [request.example.json](request.example.json) declares the PR, original request,
  image, options, settings, roles and reviewer adapter. Replace its placeholder
  image digest and PR, and retain private copies outside version control.
- [Native definitions](workflows/agents/) provide leaf, reviewer, verifier, fixer
  and audience roles. The admitted Codex runtime supplies credentials and policy.
- [action.py](workflows/helpers/action.py) handles one structured operation per
  process. [prepare.py](workflows/helpers/prfix/prepare.py) generates a complete
  static workflow closure before admission. The generated YAML is inspectable.
- [Python modules](workflows/helpers/prfix/) implement application capture,
  fingerprints, evidence/checklists, retained allowances, controlled commits,
  CI classification, publication intents and receipt reconciliation.
- [Tests](tests/) use disposable Git repositories, SQLite and fake transports.
  They do not refresh provider login or contact real providers/GitHub.

Use an independent Git clone with a `.git` directory, a clean initial worktree,
the PR head and target objects, an installed stock binary and Docker. The tool
image must already exist locally and contain Python 3.11+, Git, `gh` and `sleep`.
The [guide](../../docs/PR_FIX.md#setup-and-preparation) includes a generic image
build using Python 3.13 plus Debian's Git/`gh`/coreutils packages and pins its
resulting local `sha256:...` content ID. A compatible registry image digest also
works. The sample's zero digest is a placeholder that must be replaced; no
PR-specific Dockerfile or host Python is necessary. Run preparation and the controller as the same non-root
workspace owner. Native agents use the configured host Codex app-server runtime.

## Prepare and run

Commit the example files in the working repository first. Make a private request
file and edit its placeholders, exact original instruction and selected settings:

```sh
printf '\n/.pr-fix-state/\n/workflows/.pr-fix-portable/\n/examples/pr-fix/request.json\n' >> .git/info/exclude
cp examples/pr-fix/request.example.json examples/pr-fix/request.json
```

Set `TOOL_IMAGE` to the exact locally available digest used in `request.image`.
Provide the existing GitHub credential through `GH_TOKEN`, then prepare:

```sh
docker run --rm --pull=never --network bridge \
  --user "$(id -u):$(id -g)" -e GH_TOKEN \
  -v "$PWD:/workspace" \
  -v "$PWD/examples/pr-fix/workflows/helpers:/actions/helpers:ro" \
  -w /workspace "$TOOL_IMAGE" \
  python3 -B /actions/helpers/action.py prepare examples/pr-fix/request.json
```

Preparation reads GitHub and produces private application state and generated
definitions. It starts no agent and publishes nothing. Use its returned `config`,
`plan`, `request_id` and `workflow` values in these stock commands:

```sh
agent-workflow validate --workspace . --config CONFIG WORKFLOW
agent-workflow run --workspace . --config CONFIG \
  --secret-env github_token=GH_TOKEN --input plan=PLAN \
  --request-id REQUEST_ID --detach WORKFLOW
agent-workflow status --workspace . RUN_ID
```

Review the generated closure and byte manifest before running it. A selected
stage grants no publication authority. Remote writes need both a configured
permission and the retained exact authorization; merge requires a separate
`merge` authorization and current review, verification, final-head CI and live
repository requirements. `complete` and `ready` are distinct outputs.

Resume an interrupted original run with stock `resume`, preserving its original
session and configuration. Replacement executions keep the same `logical_run`
and cannot reset consumed allowances or CI deadlines. Unknown external effects
must reconcile before retry. See the [recovery and compatibility contract](../../docs/PR_FIX.md#stock-cli-execution-and-recovery).

State lives in `.pr-fix-state`; generated closures live in
`workflows/.pr-fix-portable/LOGICAL_RUN/FINGERPRINT`. Both namespaces carry
ownership markers. Back them up with the engine's recovery bundle and referenced
assets. Private state is outside read-only `/actions`, and neither cleanup nor
backup should discard another run's or unrelated workspace files.

## Deterministic development checks

The Python suite is a contributor check; deployed execution uses Python only
inside the selected image:

```sh
python3 -m unittest discover -s examples/pr-fix/tests -p 'test_*.py' -v
make check
make ui-pr-check BASE=origin/main
```

The repository also exercises the portable definitions through a stock-engine
fixture with fake providers/container transport. Real Docker, provider and
publication conformance are separately authorized opt-ins. No historical live
budget authorizes new calls.

The portable request deliberately replaces implicit XDG/plugin settings and old
Go-specific CLI preparation. It does not import historical run/session ledgers.
Custom adapters use explicit JSON native task definitions; legacy process hooks
and arbitrary installed skills are not implicitly executed. See the complete
[compatibility notes](../../docs/PR_FIX.md#state-backup-and-compatibility).
