# Scoped task-state helper

`workflows/client.py` is a Python standard-library client for the controller-owned task-state
service. Copy it into an admitted helper bundle and construct `TaskState(request)`
inside an `action-v1` hook. It sends each RPC once and never starts agents, opens
the engine database or accesses the general controller API.

`workflows/helper.py` demonstrates a pure capture with an input fingerprint, idempotent
allowance reservation and a completed result reference. Its checkpoint hook
revalidates the current value and evidence identity through a read-only RPC. It
deliberately stops on an interrupted claim, preserving the need to reconcile the
original operation instead of treating an interrupted process as permission to
dispatch again.

`workflows/main.yml` retains the client and helper through `container.files`. Use a
locally available image containing Python 3; admission resolves the example tag
to its immutable content ID and never pulls it. Start the workflow with an
explicit `--task-group` and JSON inputs containing `value` and a stable
`operation_id`. A replacement run uses the same group and operation identity.
Use a new operation identity for different business input. The image, guardian
and runtime setup follows [the container example](../container-workflows/README.md).

For example, from the repository root after configuring the runtime and local
image, use the stock CLI's foreground mode:

```sh
agent-workflow run --foreground --workspace examples/task-state --task-group capture-demo \
  --input value=hello --input operation_id=capture-1 workflows/main.yml
```

The runnable example requires a container runtime that can bind the controller's
host Unix socket into Linux containers. It is separate from deterministic tests:
tests use a local fake container API and disposable SQLite state, without Docker,
provider credentials or publication.

See [the task-state contract](../../docs/TASK_STATE.md) for deadline operations,
continuation provenance, uncertain claims, inspection and retention.
