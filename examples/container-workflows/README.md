# Portable container helpers

These examples use ordinary Python/Git images, workflow-local helper files and
the stock CLI. `main.yml` calls `wrapper.yml`, which calls `capture.yml`, then
prints the captured HEAD through existing reusable-workflow outputs. `mutate.yml`
uses a standalone Docker step and durable local receipts to simulate a service
with idempotent operations and authoritative reconciliation. The shared
`helpers/common/protocol.py` import exercises recursive bundle capture.

Copy this directory’s contents into the root of an independent disposable Git
checkout that already has a committed HEAD. Do not use a linked Git worktree: container
mount policy rejects shared host Git administration. Build the image explicitly
before admission; image tags are pinned to local content identities and the engine
never pulls them. The sample Dockerfile needs network access during its separate
build, while workflow execution uses `network: none`.

```sh
# From that disposable checkout, explicitly prepare the local image:
docker build -t python-git:local .
# The selected workspace must already have a readable Git HEAD:
agent-workflow run --workspace . --config agents.yml --foreground workflows/main.yml
```

Use a packaged release with its sibling `guardians/` directory, or `make build`
from the engine repository. Docker uses the selected local Unix socket, including
Docker Desktop on macOS. To use Podman, configure `execution.runtime: podman`
with `mode: container` and `image: python-git:local`, using a locally available
image in that store; configure the unused agent explicitly as local.

For the simulated mutation, first create a dedicated writable `receipts/`
directory inside the disposable workspace. Then run:

```sh
agent-workflow run --workspace . --config mutation-agents.yml --foreground workflows/mutate.yml
```

The container uses a non-root UID (the invoking UID, or 65532 when the engine is
root). That UID must be able to read the checkout and write `receipts/`; explicit
positive `execution.uid`/`gid` overrides are available. Bindings, checkpoint
validation and reconciliation only read application state. `execute` atomically
links and fsyncs an operation-keyed receipt. A receipt establishes completion; an
authoritative absence establishes no effect. Unexpected contents stay unknown.
This models one local application operation; real service helpers need their own
stable bindings, idempotency keys, evidence and reconciliation proofs.

Helper declarations are relative to each owning workflow. Admission retains
regular files in immutable per-origin bundles and mounts them read-only at
`/actions`. Source changes or deletion do not change a resumed operation. Use
`python3 -B` when helpers need no bytecode cache. Store application receipts in
permitted durable application storage; `/tmp` and `/state` are disposable.

All helper phases use one JSON request on stdin and one JSON response on stdout;
logs go to stderr. See [the phase contract](../../docs/CONTAINER_WORKFLOWS.md).
For credentials declare `env: {TOKEN: '${{ secrets.token }}'}`, declare/forward the
secret through reusable workflow interfaces, and supply `--secret-env token=ENV`
on run/resume. Neither schemas nor commands interpolate credential values.

A database-only backup retains manifests and checkpoints, but cannot restore
helper bytes. Export a recovery bundle (database plus referenced assets) for
portable recovery. Resume rejects missing/corrupted assets, changed bindings or
invalid evidence before checkpoint reuse. Retained image content and the exact
verified guardian must still be available on the selected runtime/platform.
